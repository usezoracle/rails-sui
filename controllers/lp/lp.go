// Package lp is the liquidity-provider surface for Route B: onboarding
// (BVN → dedicated virtual deposit account), the platform-side ledger
// (the rail pools funds, so per-LP balances live HERE), withdrawals,
// and the Korapay webhook that drives it all.
//
// Money discipline:
//   - Every balance mutation happens in the same DB transaction as the
//     ledger entry that justifies it.
//   - Ledger idempotency = the UNIQUE provider_ref: webhook redelivery
//     and crash replays insert-or-skip, never double-credit.
//   - Withdrawals reserve (debit) the balance BEFORE the bank transfer
//     is submitted; a terminal transfer.failed re-credits. Same
//     claim-first shape as the Route A dispatcher.
package lp

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lpaccount"
	"github.com/usezoracle/rails-sui/ent/lpledgerentry"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/baas/fintava"
	"github.com/usezoracle/rails-sui/services/baas/korapay"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// suiAddressRe validates a 32-byte hex Sui address.
var suiAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

// withdrawalRefPrefix namespaces our deterministic withdrawal payment
// references so the webhook can route transfer.* events back to ledger
// entries (and ignore unrelated payouts, e.g. Route C merchant ones).
const withdrawalRefPrefix = "lpwd-"

// Controller serves /v1/lp/* and /v1/korapay/webhook. The Korapay
// adapter is constructed directly from config — independent of
// BAAS_PROVIDER, so LP deposits can run on Korapay while the payout
// default stays SafeHaven. The raw client is kept alongside the
// adapter for VBA issuance (the neutral baas.Account doesn't carry
// bank name/code).
type Controller struct {
	kora       *korapay.Adapter // nil when KORAPAY_SECRET_KEY unset
	koraClient *korapay.Client
	fintava    *fintava.Adapter // nil when FINTAVA_API_KEY unset

	banksMu    sync.Mutex
	banksCache []gin.H
	banksAt    time.Time
}

// NewController builds the controller from env config. Both rails are
// constructed when configured — deposits can arrive on either.
func NewController() *Controller {
	conf := config.BaaSConfig()
	c := &Controller{}
	if conf.KorapaySecretKey != "" {
		client := korapay.New(
			conf.KorapaySecretKey, conf.KorapayPublicKey, conf.KorapayBaseURL,
			conf.KorapayPayoutEmail, conf.KorapayVBABankCode,
		)
		c.kora = korapay.NewAdapter(client)
		c.koraClient = client
	}
	if conf.FintavaAPIKey != "" {
		c.fintava = fintava.NewAdapter(fintava.New(
			conf.FintavaAPIKey, conf.FintavaWebhookSecret, conf.FintavaBaseURL,
		))
	}
	return c
}

func (c *Controller) railReady(ctx *gin.Context) bool {
	if c.kora == nil {
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error",
			"LP rail not configured (KORAPAY_SECRET_KEY missing)", nil)
		return false
	}
	return true
}

func userFromCtx(ctx *gin.Context) (*ent.User, bool) {
	v, exists := ctx.Get("user_id")
	if !exists {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Not authenticated", nil)
		return nil, false
	}
	userID, err := uuid.Parse(v.(string))
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid session", nil)
		return nil, false
	}
	usr, err := storage.Client.User.Query().Where(userEnt.IDEQ(userID)).Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "User not found", nil)
		return nil, false
	}
	return usr, true
}

func accountView(a *ent.LpAccount) gin.H {
	return gin.H{
		"id":                a.ID.String(),
		"name":              a.Name,
		"status":            a.Status.String(),
		"balance":           a.Balance.String(),
		"currency":          "NGN",
		"deposit_account":   a.AccountNumber,
		"deposit_bank":      a.BankName,
		"deposit_bank_code": a.BankCode,
		"account_reference": a.AccountReference,
		"sui_usdc_address":  a.SuiUsdcAddress,
		"created_at":        a.CreatedAt,
	}
}

// -----------------------------------------------------------------------------
// POST /v1/lp/onboard — BVN verify + issue the deposit virtual account
// -----------------------------------------------------------------------------

type onboardRequest struct {
	Name string `json:"name" binding:"required"`
	BVN  string `json:"bvn"  binding:"required,len=11,numeric"`
}

// Onboard verifies the LP's BVN on the rail, issues their permanent
// NGN virtual deposit account, and opens a zero-balance ledger. One LP
// account per user; calling again returns the existing one.
func (c *Controller) Onboard(ctx *gin.Context) {
	if !c.railReady(ctx) {
		return
	}
	usr, ok := userFromCtx(ctx)
	if !ok {
		return
	}
	var req onboardRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"name and an 11-digit bvn are required", u.GetErrorData(err))
		return
	}

	if existing, err := storage.Client.LpAccount.Query().
		Where(lpaccount.HasUserWith(userEnt.IDEQ(usr.ID))).
		Only(ctx); err == nil {
		u.APIResponse(ctx, http.StatusOK, "success", "LP account already exists", accountView(existing))
		return
	}

	// BVN verification (one-shot on this rail), then the account.
	if _, err := c.kora.InitiateIdentity(ctx, baas.IdentityInit{Type: "bvn", Number: req.BVN}); err != nil {
		logger.Errorf("lp onboard: bvn verify for %s: %v", usr.Email, err)
		u.APIResponse(ctx, http.StatusBadGateway, "error",
			"BVN verification failed", map[string]any{"detail": err.Error()})
		return
	}

	accountRef := "lp-" + usr.ID.String()
	va, err := c.koraClient.CreateVirtualAccount(ctx, accountRef, req.Name, usr.Email, req.BVN)
	if err != nil {
		logger.Errorf("lp onboard: create VBA for %s: %v", usr.Email, err)
		u.APIResponse(ctx, http.StatusBadGateway, "error",
			"Could not issue a deposit account", map[string]any{"detail": err.Error()})
		return
	}

	acct, err := storage.Client.LpAccount.Create().
		SetName(req.Name).
		SetEmail(usr.Email).
		SetBvnLast4(req.BVN[len(req.BVN)-4:]).
		SetAccountReference(accountRef).
		SetAccountNumber(va.AccountNumber).
		SetBankName(va.BankName).
		SetBankCode(va.BankCode).
		SetBalance(decimal.Zero).
		SetUser(usr).
		Save(ctx)
	if err != nil {
		logger.Errorf("lp onboard: persist for %s: %v", usr.Email, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to save LP account", nil)
		return
	}
	u.APIResponse(ctx, http.StatusCreated, "success",
		"LP account ready — fund it by bank transfer to your deposit account", accountView(acct))
}

// -----------------------------------------------------------------------------
// GET /v1/lp/account · GET /v1/lp/ledger
// -----------------------------------------------------------------------------

func (c *Controller) loadAccount(ctx *gin.Context) (*ent.LpAccount, bool) {
	usr, ok := userFromCtx(ctx)
	if !ok {
		return nil, false
	}
	acct, err := storage.Client.LpAccount.Query().
		Where(lpaccount.HasUserWith(userEnt.IDEQ(usr.ID))).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"No LP account — call POST /v1/lp/onboard first", nil)
		return nil, false
	}
	return acct, true
}

// GetAccount returns the LP's account + live platform balance.
func (c *Controller) GetAccount(ctx *gin.Context) {
	acct, ok := c.loadAccount(ctx)
	if !ok {
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", accountView(acct))
}

type updateAccountReq struct {
	SuiUsdcAddress string `json:"sui_usdc_address" binding:"required"`
}

// UpdateAccount sets the LP's USDC delivery address — where Route B
// fills send the USDC they've bought. Required before the LP is
// matchable for fills.
//
//	PUT /v1/lp/account
func (c *Controller) UpdateAccount(ctx *gin.Context) {
	acct, ok := c.loadAccount(ctx)
	if !ok {
		return
	}
	var req updateAccountReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "sui_usdc_address is required", nil)
		return
	}
	addr := strings.TrimSpace(req.SuiUsdcAddress)
	if !suiAddressRe.MatchString(addr) {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"sui_usdc_address must be a 0x-prefixed 64-hex Sui address", nil)
		return
	}
	updated, err := acct.Update().SetSuiUsdcAddress(strings.ToLower(addr)).Save(ctx)
	if err != nil {
		logger.Errorf("lp update account %s: %v", acct.ID, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to update account", nil)
		return
	}
	view := accountView(updated)
	view["sui_usdc_address"] = updated.SuiUsdcAddress
	u.APIResponse(ctx, http.StatusOK, "success",
		"USDC delivery address saved — you're now matchable for fills", view)
}

// GetLedger lists the LP's ledger entries, newest first.
func (c *Controller) GetLedger(ctx *gin.Context) {
	acct, ok := c.loadAccount(ctx)
	if !ok {
		return
	}
	page, offset, limit := u.Paginate(ctx)
	q := acct.QueryLedgerEntries()
	total, err := q.Clone().Count(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to count ledger", nil)
		return
	}
	rows, err := q.Order(ent.Desc(lpledgerentry.FieldCreatedAt)).Offset(offset).Limit(limit).All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to load ledger", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, e := range rows {
		out = append(out, gin.H{
			"id":           e.ID.String(),
			"type":         e.EntryType.String(),
			"amount":       e.Amount.String(),
			"currency":     e.Currency,
			"status":       e.Status.String(),
			"provider_ref": e.ProviderRef,
			"note":         e.Note,
			"at":           e.CreatedAt,
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"total": total, "page": page, "count": len(out),
		"balance": acct.Balance.String(), "entries": out,
	})
}

// -----------------------------------------------------------------------------
// GET /v1/lp/banks · GET /v1/lp/resolve — beneficiary safety rails
// -----------------------------------------------------------------------------

// Banks lists NGN beneficiary banks (cached 1h — the list changes
// rarely and the rail rate-limits).
func (c *Controller) Banks(ctx *gin.Context) {
	if !c.railReady(ctx) {
		return
	}
	c.banksMu.Lock()
	if time.Since(c.banksAt) < time.Hour && len(c.banksCache) > 0 {
		cached := c.banksCache
		c.banksMu.Unlock()
		u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{"banks": cached})
		return
	}
	c.banksMu.Unlock()

	banks, err := c.kora.ListBanks(ctx)
	if err != nil {
		logger.Errorf("lp banks: %v", err)
		u.APIResponse(ctx, http.StatusBadGateway, "error", "Could not load banks", nil)
		return
	}
	out := make([]gin.H, 0, len(banks))
	for _, b := range banks {
		out = append(out, gin.H{"name": b.Name, "code": b.BankCode})
	}
	c.banksMu.Lock()
	c.banksCache, c.banksAt = out, time.Now()
	c.banksMu.Unlock()
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{"banks": out})
}

// Resolve confirms the beneficiary account name before a withdrawal —
// the LP sees who they're paying before any money is reserved.
func (c *Controller) Resolve(ctx *gin.Context) {
	if !c.railReady(ctx) {
		return
	}
	bank := strings.TrimSpace(ctx.Query("bank_code"))
	acct := strings.TrimSpace(ctx.Query("account_number"))
	if bank == "" || len(acct) != 10 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"bank_code and a 10-digit account_number are required", nil)
		return
	}
	ne, err := c.kora.NameEnquiry(ctx, bank, acct)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadGateway, "error",
			"Could not resolve that account", map[string]any{"detail": err.Error()})
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"account_name":   ne.AccountName,
		"account_number": ne.AccountNumber,
		"bank_code":      ne.BankCode,
	})
}

// -----------------------------------------------------------------------------
// POST /v1/lp/withdraw — ledger-reserved payout to the LP's bank
// -----------------------------------------------------------------------------

type withdrawRequest struct {
	Amount        string `json:"amount"         binding:"required"`
	BankCode      string `json:"bank_code"      binding:"required"`
	AccountNumber string `json:"account_number" binding:"required"`
}

// Withdraw reserves the amount on the LP's ledger (claim-first: the
// debit lands in the same tx as the pending entry, guarded by a
// balance-sufficiency predicate), then submits the bank transfer with
// a deterministic reference. transfer.success/failed webhooks finalize
// or re-credit.
func (c *Controller) Withdraw(ctx *gin.Context) {
	if !c.railReady(ctx) {
		return
	}
	acct, ok := c.loadAccount(ctx)
	if !ok {
		return
	}
	if acct.Status != lpaccount.StatusActive {
		u.APIResponse(ctx, http.StatusForbidden, "error",
			"Account is suspended — contact support", map[string]any{"code": "lp_suspended"})
		return
	}
	var req withdrawRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"amount, bank_code, account_number are required", u.GetErrorData(err))
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || !amount.IsPositive() {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "amount must be a positive decimal", nil)
		return
	}
	// Rail constraint (verified on sandbox): ₦1,000 minimum.
	if amount.LessThan(decimal.NewFromInt(1000)) {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"minimum withdrawal is ₦1,000", map[string]any{"code": "amount_below_minimum"})
		return
	}

	entryID := uuid.New()
	payRef := withdrawalRefPrefix + entryID.String()

	// RESERVE: pending entry + balance debit, atomically, with a
	// sufficiency guard so concurrent withdrawals can't overdraw.
	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to start withdrawal", nil)
		return
	}
	n, err := tx.LpAccount.Update().
		Where(lpaccount.IDEQ(acct.ID), lpaccount.BalanceGTE(amount)).
		AddBalance(amount.Neg()).
		Save(ctx)
	if err != nil || n == 0 {
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusPaymentRequired, "error",
			"Insufficient LP balance", map[string]any{"balance": acct.Balance.String()})
		return
	}
	if _, err := tx.LpLedgerEntry.Create().
		SetID(entryID).
		SetEntryType(lpledgerentry.EntryTypeWithdrawal).
		SetAmount(amount).
		SetStatus(lpledgerentry.StatusPending).
		SetProviderRef(payRef).
		SetNote(fmt.Sprintf("to %s/%s", req.BankCode, req.AccountNumber)).
		SetLpAccount(acct).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to record withdrawal", nil)
		return
	}
	if err := tx.Commit(); err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to commit withdrawal", nil)
		return
	}

	// Submit the transfer. The adapter is idempotent on payRef, so a
	// crash here is recovered by retrying the same entry (ops or a
	// future reconciler) without double-paying.
	transfer, terr := c.kora.Transfer(ctx, baas.TransferRequest{
		BeneficiaryBankCode: req.BankCode,
		BeneficiaryAccount:  req.AccountNumber,
		Amount:              amount,
		Narration:           "Tapp LP withdrawal",
		PaymentReference:    payRef,
	})
	if terr != nil {
		// Submit definitively failed → release the reservation now.
		releaseWithdrawal(ctx, entryID, acct.ID, amount, "submit failed: "+terr.Error())
		u.APIResponse(ctx, http.StatusBadGateway, "error",
			"Withdrawal could not be submitted — your balance was not debited",
			map[string]any{"detail": terr.Error()})
		return
	}

	u.APIResponse(ctx, http.StatusAccepted, "success",
		"Withdrawal submitted — completion is confirmed by the bank rail",
		gin.H{
			"entry_id":  entryID.String(),
			"reference": payRef,
			"amount":    amount.String(),
			"status":    string(transfer.Status),
			"fee":       transfer.Fees.String(),
		})
}

// releaseWithdrawal reverts a reserved withdrawal: marks the entry
// failed and re-credits the balance, atomically, guarded on the entry
// still being pending (idempotent vs the webhook racing us).
func releaseWithdrawal(ctx *gin.Context, entryID, acctID uuid.UUID, amount decimal.Decimal, note string) {
	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		logger.Errorf("lp withdraw release %s: tx: %v", entryID, err)
		return
	}
	n, err := tx.LpLedgerEntry.Update().
		Where(lpledgerentry.IDEQ(entryID), lpledgerentry.StatusEQ(lpledgerentry.StatusPending)).
		SetStatus(lpledgerentry.StatusFailed).
		SetNote(note).
		Save(ctx)
	if err != nil || n == 0 {
		_ = tx.Rollback()
		return // already finalized elsewhere
	}
	if _, err := tx.LpAccount.Update().
		Where(lpaccount.IDEQ(acctID)).
		AddBalance(amount).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		logger.Errorf("lp withdraw release %s: re-credit: %v", entryID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		logger.Errorf("lp withdraw release %s: commit: %v", entryID, err)
	}
}

// -----------------------------------------------------------------------------
// Rail webhooks — deposits in, withdrawal finality
// -----------------------------------------------------------------------------

// Webhook ingests Korapay events (POST /v1/korapay/webhook).
func (c *Controller) Webhook(ctx *gin.Context) {
	var p baas.Provider
	if c.kora != nil {
		p = c.kora
	}
	c.railWebhook(ctx, p, "x-korapay-signature", "korapay")
}

// FintavaWebhook ingests Fintava events (POST /v1/fintava/webhook).
func (c *Controller) FintavaWebhook(ctx *gin.Context) {
	var p baas.Provider
	if c.fintava != nil {
		p = c.fintava
	}
	c.railWebhook(ctx, p, "x-fintava-signature", "fintava")
}

// railWebhook is the shared, rail-agnostic webhook core. Fails closed
// on bad signatures.
//
//	deposit events + account number → idempotent LP ledger credit
//	transfer success/failed         → finalize lpwd-* withdrawals
//
// Unknown references are acknowledged (200) and ignored — these
// endpoints also see non-LP payouts (Route B/C merchant transfers,
// finalized by the dispatcher's pollers).
func (c *Controller) railWebhook(ctx *gin.Context, provider baas.Provider, sigHeader, railName string) {
	if provider == nil {
		ctx.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"status": "unconfigured"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(ctx.Request.Body, 1<<20))
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"status": "unreadable"})
		return
	}
	if !provider.VerifyWebhook(body, ctx.GetHeader(sigHeader)) {
		logger.Warnf("%s webhook: bad signature from %s", railName, ctx.ClientIP())
		ctx.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"status": "bad signature"})
		return
	}
	ev, err := provider.ParseWebhook(body)
	if err != nil {
		logger.Errorf("%s webhook: parse: %v", railName, err)
		ctx.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"status": "unparseable"})
		return
	}

	switch {
	case (ev.Type == "charge.success" || ev.Type == "deposit") &&
		ev.Status != baas.TransferFailed && ev.AccountNumber != "":
		c.handleDeposit(ctx, ev)
	case strings.HasPrefix(ev.PaymentReference, withdrawalRefPrefix):
		c.handleWithdrawalFinality(ctx, ev)
	default:
		// Not LP-related (other product flows share the rail) — ack.
	}
	ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleDeposit credits the owning LP. Idempotency: the UNIQUE
// provider_ref insert — a redelivered webhook fails the insert and
// changes nothing.
func (c *Controller) handleDeposit(ctx *gin.Context, ev *baas.WebhookEvent) {
	acct, err := storage.Client.LpAccount.Query().
		Where(lpaccount.AccountNumberEQ(ev.AccountNumber)).
		Only(ctx)
	if err != nil {
		logger.Warnf("korapay webhook: deposit to unknown VBA %s (ref=%s)", ev.AccountNumber, ev.ProviderRef)
		return
	}
	amount, err := decimal.NewFromString(ev.Amount)
	if err != nil || !amount.IsPositive() {
		logger.Errorf("korapay webhook: bad deposit amount %q (ref=%s)", ev.Amount, ev.ProviderRef)
		return
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		logger.Errorf("korapay webhook: tx: %v", err)
		return
	}
	if _, err := tx.LpLedgerEntry.Create().
		SetEntryType(lpledgerentry.EntryTypeDeposit).
		SetAmount(amount).
		SetStatus(lpledgerentry.StatusConfirmed).
		SetProviderRef(ev.ProviderRef).
		SetRawStatus(ev.RawStatus).
		SetNote("VBA deposit").
		SetLpAccount(acct).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			return // redelivery — already credited
		}
		logger.Errorf("korapay webhook: deposit entry (ref=%s): %v", ev.ProviderRef, err)
		return
	}
	if _, err := tx.LpAccount.Update().
		Where(lpaccount.IDEQ(acct.ID)).
		AddBalance(amount).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		logger.Errorf("korapay webhook: deposit credit (ref=%s): %v", ev.ProviderRef, err)
		return
	}
	if err := tx.Commit(); err != nil {
		logger.Errorf("korapay webhook: deposit commit (ref=%s): %v", ev.ProviderRef, err)
		return
	}
	logger.Infof("💰 lp deposit: ₦%s → %s (lp=%s ref=%s)", amount, ev.AccountNumber, acct.ID, ev.ProviderRef)
}

// handleWithdrawalFinality applies terminal payout outcomes to a
// reserved withdrawal: success confirms; failure re-credits. Both
// guarded on status=pending so replays are no-ops.
func (c *Controller) handleWithdrawalFinality(ctx *gin.Context, ev *baas.WebhookEvent) {
	entryID, err := uuid.Parse(strings.TrimPrefix(ev.PaymentReference, withdrawalRefPrefix))
	if err != nil {
		logger.Warnf("korapay webhook: malformed withdrawal ref %q", ev.PaymentReference)
		return
	}
	entry, err := storage.Client.LpLedgerEntry.Query().
		Where(lpledgerentry.IDEQ(entryID)).
		WithLpAccount().
		Only(ctx)
	if err != nil {
		logger.Warnf("korapay webhook: withdrawal ref %s has no entry", ev.PaymentReference)
		return
	}

	switch ev.Status {
	case baas.TransferSuccess:
		if _, err := storage.Client.LpLedgerEntry.Update().
			Where(lpledgerentry.IDEQ(entryID), lpledgerentry.StatusEQ(lpledgerentry.StatusPending)).
			SetStatus(lpledgerentry.StatusConfirmed).
			SetRawStatus(ev.RawStatus).
			Save(ctx); err != nil {
			logger.Errorf("korapay webhook: confirm withdrawal %s: %v", entryID, err)
		}
	case baas.TransferFailed:
		if entry.Edges.LpAccount != nil {
			releaseWithdrawal(ctx, entryID, entry.Edges.LpAccount.ID, entry.Amount,
				"bank transfer failed: "+ev.RawStatus)
		}
	}
}
