// Merchant-facing Tap Card endpoints — the path the Tapp Merchant
// app drives during an in-person debit.
//
//	GET  /v1/sender/me/tap-card/nonce
//	POST /v1/sender/me/tap-card
//	POST /v1/sender/me/tap-card/:order_id/token-ack
//	GET  /v1/sender/me/tap-card/step-up
//
// The strict state machine described in `rails/docs/tapp-card-spec.md`
// lives in `TapCardDebit`. Token rotation only fires inside the
// success branch — failures of any prior step leave the legitimate
// card perfectly in sync.

package cards

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/cardservernonce"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/merchantbankaccount"
	"github.com/usezoracle/rails-sui/ent/network"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	tokenEnt "github.com/usezoracle/rails-sui/ent/token"
	"github.com/usezoracle/rails-sui/ent/transactionlog"
	svc "github.com/usezoracle/rails-sui/services"
	orderSvc "github.com/usezoracle/rails-sui/services/order"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

const (
	nonceTTL = 60 * time.Second

	// Default thresholds when the cardholder hasn't customized them.
	// Per `docs/tapp-card-spec.md`: per-tap (PIN gate) ₦2k, step-up
	// gate ₦15k, daily ₦40k. Stored as subunit on the card row;
	// fall back to these if the card has zeroes (pre-link state).
	defaultPerTapNGNKobo  = 200_000   // ₦2,000.00
	defaultStepUpNGNKobo  = 1_500_000 // ₦15,000.00
	defaultDailyNGNKobo   = 4_000_000 // ₦40,000.00
)

// -----------------------------------------------------------------------------
// GET /v1/sender/me/tap-card/nonce
// -----------------------------------------------------------------------------

type nonceRequest struct {
	Amount      string `form:"amount"        binding:"required"`
	CardUIDHash string `form:"card_uid_hash" binding:"required"`
}

type nonceResponse struct {
	Tier         string `json:"tier"`
	ServerNonce  string `json:"server_nonce"`
	StepUpURL    string `json:"step_up_url,omitempty"`
	StepUpToken  string `json:"step_up_token,omitempty"`
}

// TapCardNonce resolves the auth tier for a given amount + card and
// issues a single-use 32-byte nonce that the debit POST must echo.
func (ctrl *Controller) TapCardNonce(ctx *gin.Context) {
	var req nonceRequest
	if err := ctx.ShouldBindQuery(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate query", u.GetErrorData(err))
		return
	}

	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || !amount.IsPositive() {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Amount must be a positive decimal", nil)
		return
	}

	uidHash, err := DecodeHex(req.CardUIDHash)
	if err != nil || len(uidHash) != 32 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"card_uid_hash must be hex sha256 (64 chars)", nil)
		return
	}

	card, err := storage.Client.TappCard.
		Query().
		Where(tappcard.CardUIDHashEQ(uidHash)).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"Card not found",
			map[string]any{"code": "card_unrecognized"})
		return
	}
	if card.Status != tappcard.StatusLive {
		u.APIResponse(ctx, http.StatusConflict, "error",
			"Card not available", map[string]any{
				"code":   "card_unavailable",
				"status": string(card.Status),
			})
		return
	}

	// Convert NGN decimal to kobo (subunit). v1 = NGN only.
	amountKobo := amount.Mul(decimal.NewFromInt(100)).BigInt().Uint64()

	tier := resolveTier(card, amountKobo)

	nonceBytes, err := GenerateServerNonce()
	if err != nil {
		logger.Errorf("TapCardNonce: rng: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to issue nonce", nil)
		return
	}

	nonceRow, err := storage.Client.CardServerNonce.Create().
		SetNonce(nonceBytes).
		SetTier(cardservernonce.Tier(tier)).
		SetAmount(amount.String()).
		SetCurrency("NGN").
		SetExpiresAt(time.Now().Add(nonceTTL)).
		SetCard(card).
		SetSenderProfile(sender).
		Save(ctx)
	if err != nil {
		logger.Errorf("TapCardNonce: persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to issue nonce", nil)
		return
	}

	resp := nonceResponse{
		Tier:        tier,
		ServerNonce: EncodeHex(nonceBytes),
	}
	if tier == "step_up" {
		// Step-up token = the nonce ID itself, base64-safe. The
		// cardholder PWA reads it from the QR, hits a /step-up/parse
		// endpoint, completes WebAuthn, and the merchant-side poll
		// flips the grant flag (TODO: full step-up backend lands in
		// next chunk; v1 stub returns the URL skeleton).
		resp.StepUpToken = nonceRow.ID.String()
		resp.StepUpURL = strings.TrimRight(config.ServerConfig().PWABaseURL, "/") +
			"/cards/step-up?token=" + nonceRow.ID.String()
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Nonce issued", resp)
}

func resolveTier(card *ent.TappCard, amountSubunit uint64) string {
	perTap := card.PerTapLimitSubunit
	if perTap == 0 {
		perTap = defaultPerTapNGNKobo
	}
	stepUp := card.StepUpThresholdSubunit
	if stepUp == 0 {
		stepUp = defaultStepUpNGNKobo
	}
	switch {
	case amountSubunit < perTap:
		return "none"
	case amountSubunit < stepUp:
		return "pin"
	default:
		return "step_up"
	}
}

// -----------------------------------------------------------------------------
// POST /v1/sender/me/tap-card  — the debit itself
// -----------------------------------------------------------------------------

type debitRequest struct {
	CardUIDHash     string `json:"card_uid_hash"     binding:"required"`
	CurrentTokenCT  string `json:"current_token_ct"  binding:"required"`
	Amount          string `json:"amount"            binding:"required"`
	Currency        string `json:"currency"          binding:"required"`
	Memo            string `json:"memo"`
	ServerNonce     string `json:"server_nonce"      binding:"required"`
	PinResponse     string `json:"pin_response"`
	StepUpToken     string `json:"step_up_token"`
}

type debitResponse struct {
	Status          string `json:"status"`
	OrderID         string `json:"order_id"`
	Amount          string `json:"amount"`
	Currency        string `json:"currency"`
	NewCardToken    string `json:"new_card_token"`
	CardPassword    string `json:"card_password"`
	RemainingDaily  uint64 `json:"remaining_daily"`
	TxHash          string `json:"tx_hash,omitempty"`
}

// TapCardDebit is the strict state machine described in the spec.
// Token rotation only fires inside the success branch — any failure
// before step (9) leaves the legitimate card in sync.
func (ctrl *Controller) TapCardDebit(ctx *gin.Context) {
	var req debitRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	// Step 1: nonce — atomic consume to defeat replay.
	nonceBytes, err := DecodeHex(req.ServerNonce)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"server_nonce must be hex", nil)
		return
	}
	nonceRow, err := consumeServerNonce(ctx, sender.ID, nonceBytes)
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Nonce invalid, consumed, or expired",
			map[string]any{"code": "nonce_invalid"})
		return
	}

	// Step 2: card lookup — must be the same hash the nonce was issued for.
	uidHash, err := DecodeHex(req.CardUIDHash)
	if err != nil || len(uidHash) != 32 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"card_uid_hash must be hex sha256", nil)
		return
	}
	card, err := storage.Client.TappCard.
		Query().
		Where(tappcard.CardUIDHashEQ(uidHash)).
		WithUser().
		Only(ctx)
	if err != nil || card.Status != tappcard.StatusLive {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"Card not found or unavailable",
			map[string]any{"code": "card_unrecognized"})
		return
	}

	// Step 3: token match — single canonical, no sliding window.
	currentCT, err := DecodeHex(req.CurrentTokenCT)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"current_token_ct must be hex", nil)
		return
	}
	if card.CurrentTokenCiphertext == nil || !bytesEqual(currentCT, *card.CurrentTokenCiphertext) {
		// Increment the mismatch counter; >3 in 1h → lock.
		_ = card.Update().AddTokenMismatchCount(1).Exec(ctx)
		u.APIResponse(ctx, http.StatusForbidden, "error",
			"Card token mismatch — cardholder must resync",
			map[string]any{"code": "token_invalid_resync_required"})
		return
	}

	// Step 4: auth tier (trust the tier the nonce was issued at).
	switch nonceRow.Tier {
	case cardservernonce.TierNone:
		// no further auth
	case cardservernonce.TierPin:
		if err := verifyPin(ctx, card, nonceBytes, req.PinResponse); err != nil {
			handlePinFailure(ctx, card, err)
			return
		}
	case cardservernonce.TierStepUp:
		if req.StepUpToken == "" {
			u.APIResponse(ctx, http.StatusBadRequest, "error",
				"step_up_token required",
				map[string]any{"code": "step_up_required"})
			return
		}
		if req.StepUpToken != nonceRow.ID.String() {
			u.APIResponse(ctx, http.StatusForbidden, "error",
				"Invalid step-up token", nil)
			return
		}
		// The grant must already be recorded — cardholder completed
		// WebAuthn in the PWA, which POSTed /v1/cards/me/step-up/grant,
		// which flipped step_up_granted_at on this nonce row.
		if nonceRow.StepUpGrantedAt == nil {
			u.APIResponse(ctx, http.StatusForbidden, "error",
				"Step-up not yet granted by cardholder",
				map[string]any{"code": "step_up_pending"})
			return
		}
	}

	// Step 5: amount + currency consistency.
	if !strings.EqualFold(req.Currency, nonceRow.Currency) || req.Amount != nonceRow.Amount {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Amount/currency does not match the nonce",
			map[string]any{"code": "amount_mismatch"})
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Amount invalid", nil)
		return
	}
	amountKobo := amount.Mul(decimal.NewFromInt(100)).BigInt().Uint64()

	// Step 6: daily-limit pre-check (off-chain mirror).
	daily := card.DailyLimitSubunit
	if daily == 0 {
		daily = defaultDailyNGNKobo
	}
	if card.SpentTodaySubunit+amountKobo > daily {
		u.APIResponse(ctx, http.StatusPaymentRequired, "error",
			"Daily card limit exceeded",
			map[string]any{"code": "daily_limit_exceeded"})
		return
	}

	// Step 7: create the PaymentOrder so the merchant SSE stream picks
	// up the eventual settled event. Recipient comes from the
	// merchant's saved bank account.
	bank, err := storage.Client.MerchantBankAccount.
		Query().
		Where(merchantbankaccount.HasSenderProfileWith(senderprofile.IDEQ(sender.ID))).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusPreconditionFailed, "error",
			"Save a bank account before taking payments", nil)
		return
	}

	po, stubTxHash, perr := persistTapCardPaymentOrder(ctx, sender, bank, card, amount, req.Memo)
	if perr != nil {
		logger.Errorf("TapCardDebit: persist order: %v", perr)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to record payment", nil)
		return
	}

	// Step 8: submit the on-chain `tapp_card::debit` PTB.
	//
	// Falls back to the stub digest when:
	//   - The aggregator signer isn't configured (local dev without
	//     SUI_AGGREGATOR_PRIVATE_KEY), or
	//   - The card row is missing cap_object_id / coin_type (e.g. the
	//     PWA linking flow never completed for this card).
	//
	// In either case we proceed with token rotation + the SSE
	// merchant-side success so end-to-end UI flows can be tested
	// without on-chain infra. Production deployments will always have
	// both pieces present; the stub branch logs warnings so any
	// production fallback is obvious in observability.
	txHash := stubTxHash
	if card.CapObjectID != nil && card.CoinType != nil {
		// The cap is denominated in its coin (USDC, 6dp) — NOT NGN kobo.
		// Convert the fiat charge to USDC subunit via the live market rate so
		// the chain debits the correct value (₦1,500 → ~1.37 USDC, not the
		// 0.15 you'd get by passing kobo as micro). The off-chain daily-limit
		// bookkeeping (spent_today) stays in NGN kobo — only the chain amount
		// is converted. Caps are created with USDC-subunit limits (see the PWA
		// create_cap), so the Move per-tap check compares like units.
		ngn, cerr := storage.Client.FiatCurrency.Query().
			Where(fiatcurrency.CodeEQ("NGN"), fiatcurrency.IsEnabledEQ(true)).
			Only(ctx)
		if cerr != nil || !ngn.MarketRate.IsPositive() {
			u.APIResponse(ctx, http.StatusFailedDependency, "error",
				"Market rate unavailable — try again shortly",
				map[string]any{"code": "rate_unavailable"})
			return
		}
		debitUsdcSubunit := usdcSubunitFromNGN(amount, ngn.MarketRate)
		if svcSui, ok := orderSvc.NewOrderSui().(*orderSvc.OrderSui); ok {
			digest, err := svcSui.DebitCard(
				ctx.Request.Context(),
				*card.CapObjectID,
				*card.CoinType,
				debitUsdcSubunit,
				"", // recipient: default to aggregator address (see DebitCard)
				[]byte(po.ID.String()),
			)
			if err != nil {
				logger.Errorf("TapCardDebit: on-chain debit: %v", err)
				u.APIResponse(ctx, http.StatusBadGateway, "error",
					"On-chain debit failed",
					map[string]any{"code": "chain_debit_failed", "detail": err.Error()})
				return
			}
			txHash = digest

			// The debit moved the USDC cap→aggregator, which IS this
			// order's self-settlement — record the event the
			// dispatcher's awaiting-funds guard looks for. Without it a
			// card order that hits `awaiting_funds` (debit landed
			// seconds after the first tick) waits forever, because card
			// orders have no receive-address deposit flow to write the
			// event (see startBridge/startCCTPBridge).
			if raID, qerr := storage.Client.RouteAOrder.Query().
				Where(routeaorder.HasPaymentOrderWith(paymentorder.IDEQ(po.ID))).
				OnlyID(ctx.Request.Context()); qerr == nil {
				svc.LogOnce(ctx.Request.Context(), raID, svc.StepSelfSettle,
					svc.StatusSucceeded, svc.ActorSystem,
					map[string]any{"via": "card_debit", "tx": digest}, "", "")
			} else {
				logger.Errorf("TapCardDebit: locate route-a order for self_settle event: %v", qerr)
			}
			// Wake the dispatcher now — the funds are at the
			// aggregator; burst mode bridges within seconds instead
			// of waiting for the next cron tick.
			svc.KickRouteA()
		} else {
			logger.Warnf("TapCardDebit: aggregator service not initialized — proceeding with stub digest")
		}
	} else {
		logger.Warnf("TapCardDebit: card %s missing cap_object_id or coin_type — proceeding with stub digest", card.ID)
	}

	// Step 9: rotate the token. Atomic — if any earlier step had
	// failed we'd be unreachable here.
	newToken, err := GenerateRotationToken()
	if err != nil {
		logger.Errorf("TapCardDebit: token rng: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to rotate token", nil)
		return
	}
	if _, err := card.Update().
		SetCurrentTokenCiphertext(newToken).
		SetTokenRotatedAt(time.Now()).
		SetTokenMismatchCount(0).
		SetSpentTodaySubunit(card.SpentTodaySubunit + amountKobo).
		Save(ctx); err != nil {
		logger.Errorf("TapCardDebit: rotate persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to rotate token", nil)
		return
	}

	// Step 10: respond. The merchant app uses card_password (set
	// during PWA linking) for the NTAG215 PWD_AUTH before writing.
	var cardPwdHex string
	if card.CardPassword != nil {
		cardPwdHex = EncodeHex(*card.CardPassword)
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Card debited",
		debitResponse{
			Status:         "settled",
			OrderID:        po.ID.String(),
			Amount:         amount.String(),
			Currency:       "NGN",
			NewCardToken:   EncodeHex(newToken),
			CardPassword:   cardPwdHex,
			RemainingDaily: daily - (card.SpentTodaySubunit + amountKobo),
			TxHash:         txHash,
		})
}

// -----------------------------------------------------------------------------
// POST /v1/sender/me/tap-card/:order_id/token-ack
// -----------------------------------------------------------------------------

type tokenAckRequest struct {
	Written bool `json:"written"`
}

// TapCardTokenAck records whether the merchant app's NDEF write of
// the new rotation token actually landed. Failure → flag the card so
// the cardholder runs the PWA-driven resync flow next time.
func (ctrl *Controller) TapCardTokenAck(ctx *gin.Context) {
	orderID, err := uuid.Parse(ctx.Param("order_id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Invalid order_id", nil)
		return
	}
	var req tokenAckRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	// Find the order + its card.
	po, err := storage.Client.PaymentOrder.
		Query().
		Where(
			paymentorder.IDEQ(orderID),
			paymentorder.HasSenderProfileWith(senderprofile.IDEQ(sender.ID)),
		).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error", "Order not found", nil)
		return
	}

	// Find the card by parsing the linking ref. The PaymentOrder
	// metadata is the simplest correlation — TODO add a direct
	// PaymentOrder→TappCard edge in a follow-up. For now, look up
	// the merchant's most-recent debit-targeted card by checking the
	// reference field.
	cardID, err := uuid.Parse(po.Reference)
	if err == nil {
		card, err := storage.Client.TappCard.Get(ctx, cardID)
		if err == nil {
			if err := card.Update().SetNeedsResync(!req.Written).Exec(ctx); err != nil {
				logger.Errorf("TapCardTokenAck: update needs_resync: %v", err)
			}
		}
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Acknowledged",
		map[string]any{"acknowledged": true})
}

// -----------------------------------------------------------------------------
// GET /v1/sender/me/tap-card/step-up
// -----------------------------------------------------------------------------

// TapCardStepUpPoll resolves the step-up grant state for the
// merchant app's polling loop. Reads CardServerNonce by ID; reports:
//   - granted  → cardholder completed WebAuthn in their PWA
//   - expired  → nonce TTL elapsed without grant
//   - pending  → still waiting
func (ctrl *Controller) TapCardStepUpPoll(ctx *gin.Context) {
	tokenStr := ctx.Query("token")
	if tokenStr == "" {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"token query param required", nil)
		return
	}
	nonceID, err := uuid.Parse(tokenStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"token must be a uuid", nil)
		return
	}
	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}
	nonceRow, err := storage.Client.CardServerNonce.
		Query().
		Where(
			cardservernonce.IDEQ(nonceID),
			cardservernonce.HasSenderProfileWith(senderprofile.IDEQ(sender.ID)),
		).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"Step-up token not found", nil)
		return
	}
	now := time.Now()
	if nonceRow.StepUpGrantedAt != nil {
		u.APIResponse(ctx, http.StatusOK, "success", "Granted",
			map[string]any{"status": "granted"})
		return
	}
	if nonceRow.ExpiresAt.Before(now) {
		u.APIResponse(ctx, http.StatusOK, "success", "Expired",
			map[string]any{"status": "expired"})
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "Pending",
		map[string]any{"status": "pending"})
}

// -----------------------------------------------------------------------------
// Internals
// -----------------------------------------------------------------------------

// consumeServerNonce atomically marks the nonce consumed if it exists,
// belongs to this sender, isn't already consumed, and isn't expired.
// Returns the row so the caller can read its tier + amount.
func consumeServerNonce(ctx *gin.Context, senderID uuid.UUID, nonce []byte) (*ent.CardServerNonce, error) {
	now := time.Now()
	rows, err := storage.Client.CardServerNonce.
		Update().
		Where(
			cardservernonce.NonceEQ(nonce),
			cardservernonce.HasSenderProfileWith(senderprofile.IDEQ(senderID)),
			cardservernonce.ConsumedAtIsNil(),
			cardservernonce.ExpiresAtGT(now),
		).
		SetConsumedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, errors.New("nonce not found / consumed / expired")
	}
	// Re-fetch (the bulk update doesn't return the row directly in ent).
	return storage.Client.CardServerNonce.
		Query().
		Where(cardservernonce.NonceEQ(nonce)).
		Only(ctx)
}

// verifyPin pulls the PIN response off the request, hex-decodes, and
// matches against linking_proof + server_nonce. Caller handles
// attempt-count decrement on failure.
func verifyPin(_ *gin.Context, card *ent.TappCard, serverNonce []byte, pinResponseHex string) error {
	if pinResponseHex == "" {
		return errors.New("pin_response required")
	}
	if card.LinkingProof == nil {
		return errors.New("card has no linking_proof — needs re-link")
	}
	pinResp, err := DecodeHex(pinResponseHex)
	if err != nil {
		return fmt.Errorf("pin_response hex: %w", err)
	}
	ok, err := VerifyPinResponse(*card.LinkingProof, serverNonce, pinResp)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("pin mismatch")
	}
	return nil
}

func handlePinFailure(ctx *gin.Context, card *ent.TappCard, err error) {
	remaining := card.PinAttemptsRemaining - 1
	upd := card.Update().SetPinAttemptsRemaining(maxInt(remaining, 0))
	if remaining <= 0 {
		lockUntil := time.Now().Add(24 * time.Hour)
		upd = upd.SetLockedUntil(lockUntil).SetStatus(tappcard.StatusLocked)
	}
	if err2 := upd.Exec(ctx); err2 != nil {
		logger.Errorf("handlePinFailure: persist: %v", err2)
	}
	u.APIResponse(ctx, http.StatusForbidden, "error",
		"PIN check failed",
		map[string]any{
			"code":               "pin_invalid",
			"attempts_remaining": maxInt(remaining, 0),
			"detail":             err.Error(),
		})
}

// persistTapCardPaymentOrder writes the PaymentOrder + Recipient
// + TransactionLog. Mirrors the shape created by InitiateTapPayment
// (controllers/sender/merchant.go) so the rest of the lifecycle
// (settled webhook, SSE event, merchant dashboard) treats Tap Card
// orders identically to phone-to-phone orders.
//
// Returns the persisted order + a placeholder tx hash (real digest
// lands once the Sui PTB call is wired — see TODO at step 8).
func persistTapCardPaymentOrder(
	ctx *gin.Context,
	sender *ent.SenderProfile,
	bank *ent.MerchantBankAccount,
	card *ent.TappCard,
	amount decimal.Decimal,
	memo string,
) (*ent.PaymentOrder, string, error) {
	// Default token: a USDC-class token on sui-*. Same selection
	// rule as merchant.go's InitiateTapPayment.
	tok, err := storage.Client.Token.
		Query().
		Where(
			tokenEnt.IsEnabledEQ(true),
			tokenEnt.SymbolEQ("USDC"),
			tokenEnt.HasNetworkWith(network.IdentifierHasPrefix("sui-")),
		).
		WithNetwork().
		First(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("default token: %w", err)
	}

	// Denominate the order in USDC (the cap debits USDC and Route A bridges
	// USDC). Convert the fiat charge via the live market rate — same as the
	// QR/broadcast flow — so the dispatcher bridges the correct amount, not
	// the raw NGN figure.
	ngn, err := storage.Client.FiatCurrency.Query().
		Where(fiatcurrency.CodeEQ("NGN"), fiatcurrency.IsEnabledEQ(true)).
		Only(ctx)
	if err != nil || !ngn.MarketRate.IsPositive() {
		return nil, "", fmt.Errorf("market rate unavailable: %w", err)
	}
	usdcAmount := usdcFromNGN(amount, ngn.MarketRate)

	// Receive address — even though the card flow doesn't actually
	// use it (the Move debit settles directly), the existing
	// PaymentOrder schema requires one. PoC: generate a fresh
	// throwaway. v2: introduce a "tap-card direct" branch that skips.
	addrSvc := newReceiveAddrShim()
	address, encryptedSeed, err := addrSvc.create(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("receive address: %w", err)
	}
	receiveAddr, err := storage.Client.SuiReceiveAddress.Create().
		SetAddress(address).
		SetEncryptedSeed(encryptedSeed).
		SetCoinType(tok.ContractAddress).
		SetExpectedAmount(0).
		SetStatus(suireceiveaddress.StatusUnused).
		SetValidUntil(time.Now().Add(time.Minute)).
		Save(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("persist receive addr: %w", err)
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		return nil, "", err
	}

	orderID := uuid.New()
	txLog, err := tx.TransactionLog.Create().
		SetStatus(transactionlog.StatusOrderInitiated).
		SetNetwork(tok.Edges.Network.Identifier).
		SetMetadata(map[string]interface{}{
			"Source":   "tapp_card_debit",
			"SenderID": sender.ID.String(),
			"CardID":   card.ID.String(),
			"BankCode": bank.BankCode,
		}).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, "", err
	}

	po, err := tx.PaymentOrder.Create().
		SetID(orderID).
		SetReference(card.ID.String()). // correlate this order back to the card
		SetSenderProfile(sender).
		SetAmount(usdcAmount).
		SetAmountPaid(decimal.NewFromInt(0)).
		SetAmountReturned(decimal.NewFromInt(0)).
		SetPercentSettled(decimal.NewFromInt(0)).
		SetNetworkFee(tok.Edges.Network.Fee).
		SetProtocolFee(decimal.NewFromInt(0)).
		SetSenderFee(decimal.NewFromInt(0)).
		SetToken(tok).
		SetRate(ngn.MarketRate).
		SetSuiReceiveAddress(receiveAddr).
		SetReceiveAddressText(receiveAddr.Address).
		SetFeePercent(decimal.NewFromInt(0)).
		SetFeeAddress("").
		SetReturnAddress("").
		AddTransactions(txLog).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, "", err
	}

	if _, err := tx.PaymentOrderRecipient.Create().
		SetInstitution(bank.BankCode).
		SetAccountIdentifier(bank.AccountNumber).
		SetAccountName(bank.AccountName).
		SetMemo(memo).
		SetPaymentOrder(po).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, "", err
	}

	// Enrol the card-debit order in Route A — same settlement path as the
	// QR/broadcast flow. The card debit already sends the USDC to the
	// aggregator wallet, so the dispatcher (advancePending checks the
	// aggregator balance) bridges it and settles to the merchant's bank.
	// Without this row the order is treated as Route B (LP matching).
	if _, err := tx.RouteAOrder.Create().
		SetMode(routeaorder.ModeLp).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetPaymentOrder(po).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, "", err
	}

	if err := tx.Commit(); err != nil {
		return nil, "", err
	}

	// Placeholder until the Move debit PTB call lands.
	return po, "stub-" + orderID.String(), nil
}

// Helpers ---------------------------------------------------------------------

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// usdcFromNGN converts a NGN amount to USDC using the market rate (NGN per
// USDC). The cap is USDC-denominated, so card charges + order amounts go
// through this rather than passing the raw fiat figure.
func usdcFromNGN(amountNGN, marketRate decimal.Decimal) decimal.Decimal {
	return amountNGN.Div(marketRate)
}

// usdcSubunitFromNGN converts a NGN amount to USDC subunit (6 dp) for the
// on-chain debit. Never returns 0 — the Move debit rejects a zero amount
// (EZeroAmount).
func usdcSubunitFromNGN(amountNGN, marketRate decimal.Decimal) uint64 {
	sub := usdcFromNGN(amountNGN, marketRate).
		Mul(decimal.NewFromInt(1_000_000)).BigInt().Uint64()
	if sub == 0 {
		return 1
	}
	return sub
}

// senderFromCtx mirrors the helper in controllers/sender/merchant.go.
// We re-declare locally to avoid a circular import.
func senderFromCtx(ctx *gin.Context) (*ent.SenderProfile, bool) {
	senderCtx, ok := ctx.Get("sender")
	if !ok || senderCtx == nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Invalid API key or token", nil)
		return nil, false
	}
	sender, ok := senderCtx.(*ent.SenderProfile)
	if !ok || sender == nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Sender not authenticated", nil)
		return nil, false
	}
	return sender, true
}

// receiveAddrShim is a tiny per-controller cache for the
// ReceiveAddressService. Lazy-init avoids a package-init dependency
// on the Sui SDK key material.
type receiveAddrShim struct {
	svc *svc.ReceiveAddressService
}

func newReceiveAddrShim() *receiveAddrShim {
	return &receiveAddrShim{svc: svc.NewReceiveAddressService()}
}

func (r *receiveAddrShim) create(ctx *gin.Context) (string, []byte, error) {
	return r.svc.CreateSuiReceiveAddress(ctx.Request.Context())
}
