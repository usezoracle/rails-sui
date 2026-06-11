// route_a_treasury.go — Route C: instant merchant payouts from the
// platform's managed NGN float.
//
// For mode=treasury orders the slow leg (Paycrest LP fill, ~minutes)
// moves OFF the critical path:
//
//	bridged ──[float gate]──→ dispatching: BaaS payout float → merchant
//	        └──────────────→ settled when the rail confirms (~seconds)
//
//	reload (independent machine, same order row): once the payout is
//	submitted, Gateway createOrder with recipient = OUR OWN float
//	account; Paycrest's NGN lands back in the float. Tracked on the
//	gateway_order_id + settlement_status columns the lp path doesn't
//	use for treasury orders.
//
// Money discipline (same shapes as the rest of Route A):
//   - Claim-first: the payout reference is persisted (CAS bridged→
//     dispatching) BEFORE the transfer is submitted; both BaaS
//     adapters are idempotent on the payment reference, so crash
//     replays re-submit the same ref instead of double-paying.
//   - Refs are attempt-scoped ("rctp-<order>-<n>") so a terminally
//     FAILED transfer doesn't poison retries at rails that remember
//     references forever.
//   - The reload recipient is hard-wired to the configured float
//     account at ONE choke point — a treasury order can never aim
//     Paycrest at the merchant (double-payout tripwire).
//   - Float gate: insufficient float flips the order to mode=lp (CAS),
//     falling back to today's direct-to-merchant flow. Degraded
//     latency, never a stuck order.
package services

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/evm"
	"github.com/usezoracle/rails-sui/services/settlement"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// treasuryRefPrefix namespaces Route C payout references on the BaaS
// rail (webhook/poll routing + audit greppability).
const treasuryRefPrefix = "rctp-"

// PlatformFloatRef is the fixed Korapay account_reference of the
// platform's own virtual account — the float reload destination.
// Korapay is the source of truth (no env, no DB row): the account is
// provisioned once via the admin console (POST
// /v1/admin/treasury/float-account) and discovered here by reference.
const PlatformFloatRef = "platform-float"

// floatAccount is the resolved reload destination.
type floatAccount struct {
	bankCode      string
	accountNumber string
	accountName   string
}

// resolveFloatAccount resolves (and caches for an hour, per rail) the
// reload destination of the CURRENT float rail — "Paycrest pays to
// the float of the present config":
//
//	korapay → the platform VBA (reference "platform-float")
//	fintava → the merchant wallet's own NUBAN (live-verified), with
//	          the Paycrest institution code set via admin config
//
// Returns an error until resolvable — the reload loop waits, payouts
// are unaffected.
func (d *RouteADispatcher) resolveFloatAccount(ctx context.Context) (*floatAccount, error) {
	rail := CurrentFloatRail()
	d.floatAcctMu.Lock()
	defer d.floatAcctMu.Unlock()
	if d.floatAcct != nil && d.floatAcctRail == rail && time.Since(d.floatAcctAt) < time.Hour {
		return d.floatAcct, nil
	}

	var acct *floatAccount
	switch rail {
	case "fintava":
		if d.fintavaClient == nil {
			return nil, fmt.Errorf("fintava not configured — cannot resolve the float account")
		}
		institution := FintavaFloatInstitution()
		if institution == "" {
			return nil, fmt.Errorf("fintava float: set the Paycrest institution code of the merchant wallet's bank in admin config (Payment Rails)")
		}
		mw, err := d.fintavaClient.MerchantBalance(ctx)
		if err != nil {
			return nil, fmt.Errorf("fintava merchant wallet: %w", err)
		}
		if mw.AccountNumber == "" {
			return nil, fmt.Errorf("fintava merchant wallet has no account number")
		}
		acct = &floatAccount{
			bankCode:      institution,
			accountNumber: mw.AccountNumber,
			accountName:   mw.AccountName,
		}
	default: // korapay (and the boot default)
		if d.koraVBA == nil {
			return nil, fmt.Errorf("korapay not configured — cannot resolve the platform float account")
		}
		va, err := d.koraVBA.GetVirtualAccount(ctx, PlatformFloatRef)
		if err != nil {
			return nil, fmt.Errorf("platform float account %q not found on Korapay (provision it from the admin console): %w", PlatformFloatRef, err)
		}
		acct = &floatAccount{
			bankCode:      va.BankCode,
			accountNumber: va.AccountNumber,
			accountName:   va.AccountName,
		}
	}

	d.floatAcct = acct
	d.floatAcctRail = rail
	d.floatAcctAt = time.Now()
	return acct, nil
}

// korapayMinPayoutNGN is Korapay's per-transfer minimum (verified on
// sandbox). Orders below it fall back to the bridge path rather than
// failing the rail forever.
var korapayMinPayoutNGN = decimal.NewFromInt(1000)

// floatRail picks the rail Routes B/C pay from: a test-injected rail
// wins; otherwise the runtime switch (admin dashboard, FLOAT_RAIL env
// fallback) selects among the configured rails; "default" or an
// unconfigured choice falls back to baas.Default().
func (d *RouteADispatcher) floatRail() baas.Provider {
	if d.treasuryRail != nil {
		return d.treasuryRail
	}
	if p := d.rails[CurrentFloatRail()]; p != nil {
		return p
	}
	return baas.Default()
}

// dispatchTreasury pays the merchant from the platform float and kicks
// the reload. Called from advanceBridged for mode=treasury orders.
func (d *RouteADispatcher) dispatchTreasury(ctx context.Context, order *ent.RouteAOrder) (err error) {
	provider := d.floatRail()
	if provider == nil {
		return ErrTreasuryDispatcherNotWired
	}
	po := order.Edges.PaymentOrder
	if po == nil || po.Edges.Recipient == nil {
		return fmt.Errorf("route-a order %d missing payment_order/recipient edge", order.ID)
	}
	rcpt := po.Edges.Recipient

	// The merchant's exact entitlement, fixed at quote time.
	amountNGN := po.Amount.Mul(po.Rate).Round(2)
	if !amountNGN.IsPositive() {
		return fmt.Errorf("route-a order %d has non-positive NGN amount", order.ID)
	}

	timer := TimeSampled(ctx, order.ID, StepTreasuryPayout, ActorDispatcher).
		With("amount_ngn", amountNGN.String()).
		With("provider", provider.Name())
	defer timer.End(&err)

	// Rail minimum: Korapay refuses transfers under ₦1,000 — those
	// orders settle via the bridge path instead.
	if provider.Name() == "korapay" && amountNGN.LessThan(korapayMinPayoutNGN) {
		d.fallbackToLp(ctx, order, "below_rail_minimum", amountNGN.String())
		timer.With("fallback", "lp_below_minimum")
		return nil
	}

	// FLOAT GATE: can the float cover this exact payout? If not, fall
	// back to the lp path (direct Paycrest → merchant) rather than
	// queueing behind an empty float.
	floatBal, err := d.treasuryFloatBalance(ctx, provider)
	if err != nil {
		return fmt.Errorf("float balance: %w", err)
	}
	if floatBal.LessThan(amountNGN) {
		logger.Warnf("⚠️ route-a: order %d float too low (₦%s < ₦%s) — falling back to lp",
			order.ID, floatBal, amountNGN)
		d.fallbackToLp(ctx, order, "insufficient_float", floatBal.String())
		timer.With("fallback", "lp")
		return nil // dispatchLP picks it up next tick
	}

	// Attempt-scoped deterministic reference: count prior payout
	// attempts so a FAILED ref never blocks a retry at the rail.
	attempts, qerr := order.QueryEvents().
		Where(
			routeaevent.StepEQ(routeaevent.StepTreasuryPayout),
			routeaevent.StatusEQ(routeaevent.StatusStarted),
		).Count(ctx)
	if qerr != nil {
		attempts = int(time.Now().Unix() % 1000) // degraded uniqueness, never blocks
	}
	payRef := fmt.Sprintf("%s%d-%d", treasuryRefPrefix, order.ID, attempts)

	// CLAIM-FIRST: persist the ref + state before the money moves.
	if _, cerr := order.Update().
		Where(
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridged),
			routeaorder.ModeEQ(routeaorder.ModeTreasury),
		).
		SetTreasuryPayoutRef(payRef).
		SetBridgeStatus(routeaorder.BridgeStatusDispatching).
		Save(ctx); cerr != nil {
		if isStaleTransition(cerr) {
			logStaleTransition(order.ID, "dispatching (treasury claim)")
			return nil
		}
		return fmt.Errorf("claim treasury dispatch: %w", cerr)
	}

	transfer, terr := provider.Transfer(ctx, baas.TransferRequest{
		DebitAccountNumber:  config.BaaSConfig().DebitAccountNumber, // named-float rails (SafeHaven) use it; pooled rails ignore
		BeneficiaryBankCode: rcpt.Institution,
		BeneficiaryAccount:  rcpt.AccountIdentifier,
		Amount:              amountNGN,
		Narration:           "Tapp settlement",
		PaymentReference:    payRef,
	})
	if terr != nil {
		// Both adapters are idempotent on payRef, so this is a definite
		// no-submit. Release the claim; the retry mints a new ref.
		if _, uerr := order.Update().
			Where(
				routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching),
				routeaorder.TreasuryPayoutRefEQ(payRef),
			).
			SetBridgeStatus(routeaorder.BridgeStatusBridged).
			Save(ctx); uerr != nil && !isStaleTransition(uerr) {
			logger.Errorf("❌ route-a: revert treasury claim %d: %v", order.ID, uerr)
		}
		return fmt.Errorf("treasury payout submit: %w", terr)
	}

	timer.With("pay_ref", payRef).
		With("rail_status", transfer.RawStatus).
		With("fees", transfer.Fees.String())
	logger.Infof("⚡ route-a: order %d treasury payout submitted ₦%s → %s/%s (ref=%s, status=%s)",
		order.ID, amountNGN, rcpt.Institution, rcpt.AccountIdentifier, payRef, transfer.Status)

	// Terminal-on-submit rails resolve immediately; otherwise the
	// poller (advanceTreasuryPayouts) confirms within a tick.
	if transfer.Status == baas.TransferSuccess {
		d.finalizeTreasuryPayout(ctx, order.ID, payRef, transfer)
	}
	d.ExtendBurst()
	return nil
}

// fallbackToLp flips a bridged treasury order onto the lp path (CAS),
// recording why.
func (d *RouteADispatcher) fallbackToLp(ctx context.Context, order *ent.RouteAOrder, reason, detail string) {
	if _, uerr := order.Update().
		Where(
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridged),
			routeaorder.ModeEQ(routeaorder.ModeTreasury),
		).
		SetMode(routeaorder.ModeLp).
		Save(ctx); uerr != nil && !isStaleTransition(uerr) {
		logger.Errorf("❌ route-a: fallback to lp for %d: %v", order.ID, uerr)
		return
	}
	LogOnce(ctx, order.ID, StepTreasuryPayout, StatusSkipped, ActorDispatcher,
		map[string]any{"reason": reason, "detail": detail}, "", "")
}

// treasuryFloatBalance reads the platform's spendable NGN on the rail.
func (d *RouteADispatcher) treasuryFloatBalance(ctx context.Context, provider baas.Provider) (decimal.Decimal, error) {
	accounts, err := provider.ListAccounts(ctx, false)
	if err != nil {
		return decimal.Zero, err
	}
	for _, a := range accounts {
		if strings.EqualFold(a.Currency, "NGN") || a.Currency == "" {
			return a.Balance, nil
		}
	}
	return decimal.Zero, fmt.Errorf("no NGN float account on rail %s", provider.Name())
}

// advanceTreasuryPayouts confirms in-flight Route C payouts: polls the
// rail by reference; success → settled (merchant paid — fire SSE),
// terminal failure → release back to bridged for a fresh attempt.
func (d *RouteADispatcher) advanceTreasuryPayouts(ctx context.Context) error {
	provider := d.floatRail()
	if provider == nil {
		return nil
	}
	orders, err := db.Client.RouteAOrder.Query().
		Where(
			routeaorder.ModeEQ(routeaorder.ModeTreasury),
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching),
			routeaorder.TreasuryPayoutRefNEQ(""),
		).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) { poq.WithSenderProfile() }).
		All(ctx)
	if err != nil {
		return err
	}
	for _, order := range orders {
		st, serr := provider.TransferStatus(ctx, order.TreasuryPayoutRef)
		if serr != nil {
			logger.Errorf("❌ route-a: treasury status %d (%s): %v", order.ID, order.TreasuryPayoutRef, serr)
			continue
		}
		switch st.Status {
		case baas.TransferSuccess:
			d.finalizeTreasuryPayout(ctx, order.ID, order.TreasuryPayoutRef, st)
		case baas.TransferFailed:
			logger.Warnf("⚠️ route-a: order %d treasury payout FAILED on rail (%s) — releasing for retry",
				order.ID, st.RawStatus)
			LogOnce(ctx, order.ID, StepTreasuryPayout, StatusFailed, ActorDispatcher,
				map[string]any{"pay_ref": order.TreasuryPayoutRef, "raw_status": st.RawStatus}, st.Message, "")
			if _, uerr := order.Update().
				Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
				SetBridgeStatus(routeaorder.BridgeStatusBridged).
				SetTreasuryPayoutRef(""). // failed ref retired; retry mints a new one
				Save(ctx); uerr != nil && !isStaleTransition(uerr) {
				logger.Errorf("❌ route-a: release failed treasury payout %d: %v", order.ID, uerr)
			}
		}
	}
	return nil
}

// finalizeTreasuryPayout marks the order settled — the merchant HAS
// the naira — and fans out the SSE event. Reload continues separately.
func (d *RouteADispatcher) finalizeTreasuryPayout(ctx context.Context, orderID int, payRef string, st *baas.Transfer) {
	order, err := db.Client.RouteAOrder.Query().
		Where(routeaorder.IDEQ(orderID)).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) { poq.WithSenderProfile() }).
		Only(ctx)
	if err != nil {
		logger.Errorf("❌ route-a: load order %d for treasury finalize: %v", orderID, err)
		return
	}
	if _, uerr := order.Update().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
		SetBridgeStatus(routeaorder.BridgeStatusSettled).
		Save(ctx); uerr != nil {
		if !isStaleTransition(uerr) {
			logger.Errorf("❌ route-a: persist treasury settled %d: %v", orderID, uerr)
		}
		return
	}
	if order.Edges.PaymentOrder != nil {
		if _, perr := order.Edges.PaymentOrder.Update().
			SetStatus(paymentorder.StatusSettled).
			Save(ctx); perr != nil {
			logger.Errorf("❌ route-a: payment_order → settled (%d): %v", orderID, perr)
		}
	}
	LogOnce(ctx, orderID, StepTreasuryPayout, StatusSucceeded, ActorDispatcher,
		map[string]any{"pay_ref": payRef, "fees": st.Fees.String()}, "", "")
	logger.Infof("✅ route-a: order %d SETTLED from float (ref=%s)", orderID, payRef)
	d.publishOrderEvent(order, settlement.StatusSettled, routeaorder.BridgeStatusSettled, paymentorder.StatusSettled)
}

// -----------------------------------------------------------------------------
// Float reload — Paycrest pays our own account back
// -----------------------------------------------------------------------------

// advanceFloatReload runs the reload leg for treasury orders whose
// payout has been submitted (dispatching/settled): submit the Gateway
// createOrder aimed at OUR float account, then poll Paycrest until the
// NGN lands. Completely off the merchant's critical path — failures
// here never touch customer experience, only float thickness.
func (d *RouteADispatcher) advanceFloatReload(ctx context.Context) error {
	if d.evm == nil || d.settlement == nil {
		return nil
	}

	orders, err := db.Client.RouteAOrder.Query().
		Where(
			routeaorder.ModeEQ(routeaorder.ModeTreasury),
			routeaorder.BridgeStatusIn(
				routeaorder.BridgeStatusDispatching,
				routeaorder.BridgeStatusSettled,
			),
			// NULL-safe: a fresh treasury order has no settlement_status
			// yet (NEQ alone would exclude NULL rows entirely).
			routeaorder.Or(
				routeaorder.SettlementStatusIsNil(),
				routeaorder.SettlementStatusNEQ("settled"),
			),
		).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) { poq.WithToken() }).
		All(ctx)
	if err != nil {
		return err
	}

	if len(orders) == 0 {
		return nil
	}

	// Resolve the reload destination from Korapay (cached). Until the
	// platform VBA is provisioned, reloads wait — warn occasionally,
	// never block payouts.
	acct, aerr := d.resolveFloatAccount(ctx)
	if aerr != nil {
		if time.Since(d.lastFloatWarn) > 10*time.Minute {
			d.lastFloatWarn = time.Now()
			logger.Warnf("⚠️ route-a: %d order(s) awaiting float reload: %v", len(orders), aerr)
		}
		// Still poll already-submitted reloads.
		for _, order := range orders {
			if order.GatewayOrderID != "" {
				d.pollFloatReload(ctx, order)
			}
		}
		return nil
	}

	for _, order := range orders {
		if order.GatewayOrderID == "" {
			d.submitFloatReload(ctx, order, acct)
			continue
		}
		d.pollFloatReload(ctx, order)
	}
	return nil
}

// submitFloatReload aims dispatchLP's machinery at the platform's own
// bank account. Claim-first on the gateway_order_id emptiness.
func (d *RouteADispatcher) submitFloatReload(ctx context.Context, order *ent.RouteAOrder, acct *floatAccount) {
	var err error
	timer := TimeSampled(ctx, order.ID, StepFloatReload, ActorDispatcher)
	defer timer.End(&err)

	if order.BridgedAmount == nil || order.BridgedAmount.IsZero() {
		err = fmt.Errorf("order %d has no bridged_amount to reload", order.ID)
		return
	}

	// THE choke point: recipient is the Korapay-resolved platform
	// float account. Never derived from the order — a treasury reload
	// cannot pay a merchant by construction.
	gatewayID, txHash, derr := d.createGatewayOrder(ctx, order,
		acct.bankCode, acct.accountNumber, acct.accountName,
	)
	if derr != nil {
		err = derr
		logger.Errorf("❌ route-a: float reload submit %d: %v", order.ID, derr)
		return
	}
	if _, perr := order.Update().
		Where(
			routeaorder.IDEQ(order.ID),
			routeaorder.Or(routeaorder.GatewayOrderIDIsNil(), routeaorder.GatewayOrderIDEQ("")),
		).
		SetGatewayOrderID(gatewayID).
		SetGatewayChainID(uint64(d.conf.BaseChainID)).
		SetBridgeTxDest(txHash).
		Save(ctx); perr != nil {
		if isStaleTransition(perr) {
			logStaleTransition(order.ID, "reload gateway id")
			return
		}
		err = fmt.Errorf("persist reload gateway id (order %s on-chain!): %w", gatewayID, perr)
		return
	}
	timer.With("gateway_order_id", gatewayID)
	logger.Infof("🔄 route-a: order %d float reload submitted (gateway=%s)", order.ID, gatewayID)
	d.ExtendBurst()
}

// pollFloatReload polls Paycrest for the reload order; settled means
// the NGN is back in the float.
func (d *RouteADispatcher) pollFloatReload(ctx context.Context, order *ent.RouteAOrder) {
	info, err := d.settlement.FetchOrderStatus(ctx, int64(order.GatewayChainID), order.GatewayOrderID)
	if err != nil {
		logger.Errorf("❌ route-a: reload status %d: %v", order.ID, err)
		return
	}
	if string(info.Status) == order.SettlementStatus {
		return
	}
	upd := order.Update().SetSettlementStatus(string(info.Status)).SetSettlementPolledAt(time.Now())
	if _, uerr := upd.Save(ctx); uerr != nil {
		logger.Errorf("❌ route-a: persist reload status %d: %v", order.ID, uerr)
		return
	}
	if info.Status == "settled" {
		LogOnce(ctx, order.ID, StepFloatReload, StatusSucceeded, ActorDispatcher,
			map[string]any{"gateway_order_id": order.GatewayOrderID}, "", "")
		logger.Infof("💧 route-a: order %d float reloaded (gateway=%s)", order.ID, order.GatewayOrderID)
	}
}

// createGatewayOrder submits a Gateway createOrder for the order's
// full bridged USDC aimed at the given bank recipient. Used ONLY by
// the float reload (the merchant-facing path is dispatchLP); sender
// fee is zero — this is our own money coming home, Paycrest's spread
// is the only cost.
func (d *RouteADispatcher) createGatewayOrder(
	ctx context.Context, order *ent.RouteAOrder,
	institution, accountNumber, accountName string,
) (gatewayID, txHash string, err error) {
	amount := decimalToSubunit(*order.BridgedAmount, d.conf.BaseUSDCDecimals)

	liveQuote, err := d.settlement.FetchRate(ctx, "base", "USDC", *order.BridgedAmount, "NGN")
	if err != nil {
		return "", "", fmt.Errorf("reload rate: %w", err)
	}
	rateScaled := liveQuote.Rate.Mul(decimal.NewFromInt(100)).Truncate(0)
	rate, ok := new(big.Int).SetString(rateScaled.String(), 10)
	if !ok {
		return "", "", fmt.Errorf("reload rate parse %q", rateScaled.String())
	}

	pem, err := d.settlement.FetchPublicKey(ctx)
	if err != nil {
		return "", "", fmt.Errorf("settlement pubkey: %w", err)
	}
	messageHash, err := settlement.EncryptRecipient(settlement.Recipient{
		AccountIdentifier: accountNumber,
		AccountName:       accountName,
		Institution:       institution,
		Memo:              "Tapp float reload",
		Nonce:             settlement.NewNonce(),
		Metadata:          map[string]string{"apiKey": d.conf.SettlementSenderAPIKeyID},
	}, pem)
	if err != nil {
		return "", "", fmt.Errorf("encrypt reload recipient: %w", err)
	}

	// Allowance is max-granted once (see dispatchLP); verify it covers.
	current, err := d.evm.USDC().Allowance(ctx, d.evm.From(), d.evm.Config().GatewayAddr)
	if err != nil {
		return "", "", fmt.Errorf("reload allowance: %w", err)
	}
	if current.Cmp(amount) < 0 {
		if _, aerr := d.evm.USDC().Approve(ctx, d.evm.Config().GatewayAddr, ethmath.MaxBig256); aerr != nil {
			return "", "", fmt.Errorf("reload approve: %w", aerr)
		}
	}

	aggregatorAddr := ethcommon.HexToAddress(d.conf.BaseAggregatorAddress)
	result, cerr := d.evm.Gateway().CreateOrder(ctx, evm.CreateOrderParams{
		Token:              d.evm.Config().USDCAddr,
		Amount:             amount,
		Rate:               rate,
		SenderFeeRecipient: aggregatorAddr,
		SenderFee:          big.NewInt(0),
		RefundAddress:      aggregatorAddr,
		MessageHash:        messageHash,
	})
	if cerr != nil {
		var submitted *evm.SubmittedError
		if errors.As(cerr, &submitted) {
			// On-chain but unconfirmed: report the hash so the caller's
			// claim guard (empty gateway_order_id) keeps it visible.
			return "", submitted.TxHash.Hex(), fmt.Errorf("reload createOrder ambiguous (tx %s): %w", submitted.TxHash.Hex(), cerr)
		}
		return "", "", fmt.Errorf("reload createOrder: %w", cerr)
	}
	return strings.ToLower(result.OrderID.Hex()), result.TxHash.Hex(), nil
}
