// route_a_lp_network.go — Route B: our own LP network settles orders.
//
// The insight that shapes this file: when our LP settles, THE BRIDGE
// IS UNNECESSARY. The customer's USDC already sits at the Sui
// aggregator; the LP wants USDC; the merchant wants NGN that the LP
// has already deposited into the pooled balance. So a Route B fill is
// three internal moves and no external bridge at all:
//
//	1. MATCH+CLAIM  pick an LP (active, funded, Sui address set) and
//	                atomically: debit their ledger by the merchant's
//	                NGN entitlement + record a pending `fill` entry +
//	                CAS the order pending→dispatching.
//	2. PAYOUT       pooled Korapay balance → merchant bank
//	                (idempotent attempt-scoped reference).
//	3. DELIVER      the order's USDC, Sui aggregator → LP's address.
//	                The LP just bought USDC at the order's rate.
//
// Solvency invariant: pooled NGN == Σ LP ledger balances. The LP is
// debited exactly what the merchant receives; the platform's margin
// stays where it already accrues (the card-collection buffer in Sui
// USDC). LP economics v1: fee-free conversion at the order rate —
// LP-set rates/spreads arrive with the pricing engine later.
//
// Failure shapes (same discipline as Routes A/C):
//   - No matchable LP / payout below the rail minimum → the order
//     falls back to the bridge path. Degraded latency, never stuck.
//   - Payout terminal failure → ledger re-credited (entry failed),
//     order released to pending; next attempt re-matches with fresh
//     attempt-scoped refs.
//   - USDC delivery submit ambiguity → order PARKS loudly for ops
//     (auto-retrying an ambiguous Sui submit could double-send).
package services

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	suiconst "github.com/block-vision/sui-go-sdk/constant"
	suimodels "github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/mystenbcs"
	"github.com/block-vision/sui-go-sdk/transaction"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lpaccount"
	"github.com/usezoracle/rails-sui/ent/lpledgerentry"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/settlement"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

const (
	// Attempt-scoped reference prefixes: merchant payout + LP ledger.
	lpNetPayRefPrefix  = "rbpay-"
	lpNetFillRefPrefix = "lpfill-"
	// deliveryParkedMarker prefixes failure_reason when a USDC
	// delivery submit was ambiguous — ops must verify on Suiscan
	// before clearing (auto-retry risks a double-send).
	deliveryParkedMarker = "usdc delivery ambiguous"
)

// advanceLpNetwork drives mode=lp_network orders from pending into a
// claimed fill. Runs each tick before the generic pending loop ever
// sees these orders (advancePending excludes the mode).
func (d *RouteADispatcher) advanceLpNetwork(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.Query().
		Where(
			routeaorder.ModeEQ(routeaorder.ModeLpNetwork),
			routeaorder.BridgeStatusIn(
				routeaorder.BridgeStatusPending,
				routeaorder.BridgeStatusAwaitingFunds,
			),
		).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithToken()
			poq.WithRecipient()
		}).
		All(ctx)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if err := d.fillViaLpNetwork(ctx, order); err != nil {
			if errorsIsAwaiting(err) {
				continue // funds not at the aggregator yet
			}
			logger.Errorf("❌ route-a: lp-network fill %d: %v", order.ID, err)
		}
	}
	return nil
}

func errorsIsAwaiting(err error) bool {
	_, ok := err.(*awaitingDepositErr)
	return ok
}

// fillViaLpNetwork runs MATCH + CLAIM + PAYOUT for one order.
func (d *RouteADispatcher) fillViaLpNetwork(ctx context.Context, order *ent.RouteAOrder) (err error) {
	provider := d.floatRail()
	if provider == nil {
		return ErrTreasuryDispatcherNotWired
	}
	po := order.Edges.PaymentOrder
	if po == nil || po.Edges.Token == nil || po.Edges.Recipient == nil {
		return fmt.Errorf("order %d missing payment_order/token/recipient edges", order.ID)
	}
	tok := po.Edges.Token
	rcpt := po.Edges.Recipient
	targetNGN := po.Amount.Mul(po.Rate).Round(2)
	if !targetNGN.IsPositive() {
		return fmt.Errorf("order %d has non-positive NGN target", order.ID)
	}

	timer := TimeSampled(ctx, order.ID, StepTreasuryPayout, ActorDispatcher).
		With("route", "lp_network").
		With("amount_ngn", targetNGN.String())
	defer timer.End(&err)

	// FUNDS GATE: the customer's USDC must be in our custody before
	// the LP's money moves (mirrors startCCTPBridge, incl. the card
	// self-settle semantics).
	usdcSubunits, err := usdcSubunitsUint64(po.Amount, int(tok.Decimals))
	if err != nil {
		return fmt.Errorf("order %d amount: %w", order.ID, err)
	}
	balResp, err := d.suiClient.SuiXGetBalance(ctx, suimodels.SuiXGetBalanceRequest{
		Owner:    d.signer.Address,
		CoinType: tok.ContractAddress,
	})
	if err != nil {
		return fmt.Errorf("check aggregator balance: %w", err)
	}
	have, ok := new(big.Int).SetString(balResp.TotalBalance, 10)
	if !ok {
		return fmt.Errorf("parse aggregator balance %q", balResp.TotalBalance)
	}
	if order.BridgeStatus == routeaorder.BridgeStatusAwaitingFunds &&
		!hasSuccessfulRouteAEvent(ctx, order.ID, StepSelfSettle) {
		return ErrAwaitingDepositAtAggregator
	}
	if have.Cmp(new(big.Int).SetUint64(usdcSubunits)) < 0 {
		return ErrAwaitingDepositAtAggregator
	}

	// Rail minimum (Korapay ₦1,000) — too small for the pooled payout.
	if provider.Name() == "korapay" && targetNGN.LessThan(korapayMinPayoutNGN) {
		d.lpNetworkFallback(ctx, order, "below_rail_minimum", targetNGN.String())
		timer.With("fallback", "bridge_below_minimum")
		return nil
	}

	// MATCH: richest active LP that can cover and can receive USDC.
	lp, err := db.Client.LpAccount.Query().
		Where(
			lpaccount.StatusEQ(lpaccount.StatusActive),
			lpaccount.BalanceGTE(targetNGN),
			lpaccount.SuiUsdcAddressNEQ(""),
			lpaccount.SuiUsdcAddressNotNil(),
		).
		Order(ent.Desc(lpaccount.FieldBalance)).
		First(ctx)
	if err != nil {
		// No LP can fill → bridge path takes the order.
		d.lpNetworkFallback(ctx, order, "no_matchable_lp", targetNGN.String())
		timer.With("fallback", "bridge_no_lp")
		return nil
	}

	// Attempt-scoped refs (a failed attempt's refs are retired).
	attempts, qerr := order.QueryEvents().
		Where(
			routeaevent.StepEQ(routeaevent.StepTreasuryPayout),
			routeaevent.StatusEQ(routeaevent.StatusStarted),
		).Count(ctx)
	if qerr != nil {
		attempts = int(time.Now().Unix() % 1000)
	}
	payRef := fmt.Sprintf("%s%d-%d", lpNetPayRefPrefix, order.ID, attempts)
	fillRef := fmt.Sprintf("%s%d-%d", lpNetFillRefPrefix, order.ID, attempts)

	// CLAIM: one transaction — LP debited (sufficiency-guarded),
	// pending fill entry written (unique ref), order moved to
	// dispatching with the payout ref. Any leg failing rolls all back.
	tx, err := db.Client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("claim tx: %w", err)
	}
	n, err := tx.LpAccount.Update().
		Where(lpaccount.IDEQ(lp.ID), lpaccount.BalanceGTE(targetNGN)).
		AddBalance(targetNGN.Neg()).
		Save(ctx)
	if err != nil || n == 0 {
		_ = tx.Rollback()
		return fmt.Errorf("lp %s debit lost race (balance moved)", lp.ID)
	}
	if _, err := tx.LpLedgerEntry.Create().
		SetEntryType(lpledgerentry.EntryTypeFill).
		SetAmount(targetNGN).
		SetStatus(lpledgerentry.StatusPending).
		SetProviderRef(fillRef).
		SetNote(fmt.Sprintf("route-b fill order %d → %s USDC incoming", order.ID, po.Amount)).
		SetLpAccountID(lp.ID).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("fill entry: %w", err)
	}
	cn, err := tx.RouteAOrder.Update().
		Where(
			routeaorder.IDEQ(order.ID),
			routeaorder.BridgeStatusIn(
				routeaorder.BridgeStatusPending,
				routeaorder.BridgeStatusAwaitingFunds,
			),
		).
		SetBridgeStatus(routeaorder.BridgeStatusDispatching).
		SetTreasuryPayoutRef(payRef).
		Save(ctx)
	if err != nil || cn == 0 {
		_ = tx.Rollback()
		logStaleTransition(order.ID, "dispatching (lp-network claim)")
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("claim commit: %w", err)
	}

	logger.Infof("🤝 route-a: order %d matched to LP %s (₦%s reserved, fill=%s)",
		order.ID, lp.ID, targetNGN, fillRef)
	timer.With("lp_id", lp.ID.String()).With("pay_ref", payRef)

	// PAYOUT: pooled balance → merchant. Idempotent on payRef.
	transfer, terr := provider.Transfer(ctx, baas.TransferRequest{
		BeneficiaryBankCode: rcpt.Institution,
		BeneficiaryAccount:  rcpt.AccountIdentifier,
		Amount:              targetNGN,
		Narration:           "Tapp settlement",
		PaymentReference:    payRef,
	})
	if terr != nil {
		// Definite no-submit (adapters are idempotent on payRef) —
		// unwind the whole claim.
		d.releaseLpFill(ctx, order.ID, fillRef, "payout submit failed: "+terr.Error())
		return fmt.Errorf("lp-network payout submit: %w", terr)
	}
	if transfer.Status == baas.TransferSuccess {
		d.finalizeLpFill(ctx, order.ID)
	}
	d.ExtendBurst()
	return nil
}

// lpNetworkFallback routes the order to the bridge path (mode=lp).
func (d *RouteADispatcher) lpNetworkFallback(ctx context.Context, order *ent.RouteAOrder, reason, detail string) {
	if _, uerr := order.Update().
		Where(
			routeaorder.ModeEQ(routeaorder.ModeLpNetwork),
			routeaorder.BridgeStatusIn(
				routeaorder.BridgeStatusPending,
				routeaorder.BridgeStatusAwaitingFunds,
			),
		).
		SetMode(routeaorder.ModeLp).
		Save(ctx); uerr != nil && !isStaleTransition(uerr) {
		logger.Errorf("❌ route-a: lp-network fallback %d: %v", order.ID, uerr)
		return
	}
	LogOnce(ctx, order.ID, StepTreasuryPayout, StatusSkipped, ActorDispatcher,
		map[string]any{"route": "lp_network", "reason": reason, "detail": detail}, "", "")
}

// releaseLpFill unwinds a claimed fill: entry → failed, LP re-credited,
// order back to pending with the ref retired. Guarded on pending so
// replays are no-ops.
func (d *RouteADispatcher) releaseLpFill(ctx context.Context, orderID int, fillRef, note string) {
	entry, err := db.Client.LpLedgerEntry.Query().
		Where(lpledgerentry.ProviderRefEQ(fillRef)).
		WithLpAccount().
		Only(ctx)
	if err != nil {
		logger.Errorf("❌ route-a: release fill %s: lookup: %v", fillRef, err)
		return
	}
	tx, err := db.Client.Tx(ctx)
	if err != nil {
		return
	}
	n, err := tx.LpLedgerEntry.Update().
		Where(lpledgerentry.IDEQ(entry.ID), lpledgerentry.StatusEQ(lpledgerentry.StatusPending)).
		SetStatus(lpledgerentry.StatusFailed).
		SetNote(note).
		Save(ctx)
	if err != nil || n == 0 {
		_ = tx.Rollback()
		return // already finalized
	}
	if entry.Edges.LpAccount != nil {
		if _, err := tx.LpAccount.Update().
			Where(lpaccount.IDEQ(entry.Edges.LpAccount.ID)).
			AddBalance(entry.Amount).
			Save(ctx); err != nil {
			_ = tx.Rollback()
			logger.Errorf("❌ route-a: release fill %s: re-credit: %v", fillRef, err)
			return
		}
	}
	if _, err := tx.RouteAOrder.Update().
		Where(
			routeaorder.IDEQ(orderID),
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching),
		).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetTreasuryPayoutRef("").
		Save(ctx); err != nil {
		_ = tx.Rollback()
		logger.Errorf("❌ route-a: release fill %s: order reset: %v", fillRef, err)
		return
	}
	if err := tx.Commit(); err != nil {
		logger.Errorf("❌ route-a: release fill %s: commit: %v", fillRef, err)
	}
}

// advanceLpNetworkPayouts confirms in-flight Route B payouts and runs
// USDC delivery + settlement.
func (d *RouteADispatcher) advanceLpNetworkPayouts(ctx context.Context) error {
	provider := d.floatRail()
	if provider == nil {
		return nil
	}
	orders, err := db.Client.RouteAOrder.Query().
		Where(
			routeaorder.ModeEQ(routeaorder.ModeLpNetwork),
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching),
			routeaorder.TreasuryPayoutRefNEQ(""),
		).
		All(ctx)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if strings.HasPrefix(order.FailureReason, deliveryParkedMarker) {
			continue // parked for ops — never auto-retry an ambiguous Sui submit
		}
		// Delivery already done → just finish settlement bookkeeping.
		if order.BridgeTxSui != "" {
			d.settleLpFill(ctx, order)
			continue
		}
		st, serr := provider.TransferStatus(ctx, order.TreasuryPayoutRef)
		if serr != nil {
			logger.Errorf("❌ route-a: lp-network payout status %d: %v", order.ID, serr)
			continue
		}
		switch st.Status {
		case baas.TransferSuccess:
			d.finalizeLpFill(ctx, order.ID)
		case baas.TransferFailed:
			attempt := strings.TrimPrefix(order.TreasuryPayoutRef, lpNetPayRefPrefix)
			fillRef := lpNetFillRefPrefix + attempt
			logger.Warnf("⚠️ route-a: order %d lp-network payout FAILED (%s) — unwinding fill", order.ID, st.RawStatus)
			d.releaseLpFill(ctx, order.ID, fillRef, "merchant payout failed: "+st.RawStatus)
		}
	}
	return nil
}

// finalizeLpFill runs after the merchant payout succeeded: deliver the
// order's USDC to the LP, then settle.
func (d *RouteADispatcher) finalizeLpFill(ctx context.Context, orderID int) {
	order, err := db.Client.RouteAOrder.Query().
		Where(routeaorder.IDEQ(orderID)).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithToken()
			poq.WithSenderProfile()
		}).
		Only(ctx)
	if err != nil {
		logger.Errorf("❌ route-a: lp fill load %d: %v", orderID, err)
		return
	}
	po := order.Edges.PaymentOrder
	if po == nil || po.Edges.Token == nil {
		return
	}

	if order.BridgeTxSui == "" {
		attempt := strings.TrimPrefix(order.TreasuryPayoutRef, lpNetPayRefPrefix)
		fillRef := lpNetFillRefPrefix + attempt
		entry, eerr := db.Client.LpLedgerEntry.Query().
			Where(lpledgerentry.ProviderRefEQ(fillRef)).
			WithLpAccount().
			Only(ctx)
		if eerr != nil || entry.Edges.LpAccount == nil || entry.Edges.LpAccount.SuiUsdcAddress == "" {
			logger.Errorf("❌ route-a: lp fill %d: resolve LP for delivery: %v", orderID, eerr)
			return
		}
		usdcSubunits, aerr := usdcSubunitsUint64(po.Amount, int(po.Edges.Token.Decimals))
		if aerr != nil {
			logger.Errorf("❌ route-a: lp fill %d: amount: %v", orderID, aerr)
			return
		}
		digest, derr := d.suiTransferUSDC(ctx, po.Edges.Token.ContractAddress,
			usdcSubunits, entry.Edges.LpAccount.SuiUsdcAddress)
		if derr != nil {
			// Ambiguous-or-failed Sui submit: PARK. A blind retry could
			// double-send the LP's USDC; ops verifies on Suiscan.
			reason := fmt.Sprintf("%s: %v (LP %s, %d subunits)",
				deliveryParkedMarker, derr, entry.Edges.LpAccount.ID, usdcSubunits)
			if _, uerr := order.Update().
				Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
				SetFailureReason(reason).
				Save(ctx); uerr != nil && !isStaleTransition(uerr) {
				logger.Errorf("❌ route-a: persist delivery park %d: %v", orderID, uerr)
			}
			logger.Errorf("🚨 route-a: order %d USDC delivery parked: %v", orderID, derr)
			return
		}
		if _, uerr := order.Update().
			Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
			SetBridgeTxSui(digest).
			Save(ctx); uerr != nil && !isStaleTransition(uerr) {
			logger.Errorf("❌ route-a: persist delivery digest %d (tx=%s!): %v", orderID, digest, uerr)
			return
		}
		order.BridgeTxSui = digest
	}

	d.settleLpFill(ctx, order)
}

// settleLpFill confirms the ledger entry and settles the order.
func (d *RouteADispatcher) settleLpFill(ctx context.Context, order *ent.RouteAOrder) {
	attempt := strings.TrimPrefix(order.TreasuryPayoutRef, lpNetPayRefPrefix)
	fillRef := lpNetFillRefPrefix + attempt
	if _, err := db.Client.LpLedgerEntry.Update().
		Where(
			lpledgerentry.ProviderRefEQ(fillRef),
			lpledgerentry.StatusEQ(lpledgerentry.StatusPending),
		).
		SetStatus(lpledgerentry.StatusConfirmed).
		SetNote("filled — USDC delivered " + order.BridgeTxSui).
		Save(ctx); err != nil && !ent.IsNotFound(err) {
		logger.Errorf("❌ route-a: confirm fill entry %s: %v", fillRef, err)
	}

	if _, uerr := order.Update().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
		SetBridgeStatus(routeaorder.BridgeStatusSettled).
		Save(ctx); uerr != nil {
		if !isStaleTransition(uerr) {
			logger.Errorf("❌ route-a: persist lp-network settled %d: %v", order.ID, uerr)
		}
		return
	}
	if order.Edges.PaymentOrder != nil {
		if _, perr := order.Edges.PaymentOrder.Update().
			SetStatus(paymentorder.StatusSettled).
			Save(ctx); perr != nil {
			logger.Errorf("❌ route-a: payment_order → settled (%d): %v", order.ID, perr)
		}
	}
	LogOnce(ctx, order.ID, StepTreasuryPayout, StatusSucceeded, ActorDispatcher,
		map[string]any{"route": "lp_network", "usdc_delivery_tx": order.BridgeTxSui, "pay_ref": order.TreasuryPayoutRef}, "", "")
	logger.Infof("✅ route-a: order %d SETTLED via LP network (usdc tx=%s)", order.ID, order.BridgeTxSui)
	d.publishOrderEvent(order, settlement.StatusSettled, routeaorder.BridgeStatusSettled, paymentorder.StatusSettled)
	d.ExtendBurst()
}

// suiTransferUSDC sends `amount` subunits of `coinType` from the
// aggregator to `recipient` — the LP's purchase landing. Mirrors the
// CCTP burn builder's coin handling.
func (d *RouteADispatcher) suiTransferUSDC(ctx context.Context, coinType string, amount uint64, recipient string) (string, error) {
	coinResp, err := d.suiClient.SuiXGetCoins(ctx, suimodels.SuiXGetCoinsRequest{
		Owner:    d.signer.Address,
		CoinType: coinType,
	})
	if err != nil {
		return "", fmt.Errorf("list coins: %w", err)
	}
	var (
		selected []suimodels.CoinData
		total    = new(big.Int)
		need     = new(big.Int).SetUint64(amount)
	)
	for _, c := range coinResp.Data {
		bal, ok := new(big.Int).SetString(c.Balance, 10)
		if !ok || bal.Sign() <= 0 {
			continue
		}
		selected = append(selected, c)
		total.Add(total, bal)
		if total.Cmp(need) >= 0 {
			break
		}
	}
	if total.Cmp(need) < 0 {
		return "", fmt.Errorf("insufficient USDC: have %s, need %s", total, need)
	}

	gasResp, err := d.suiClient.SuiXGetCoins(ctx, suimodels.SuiXGetCoinsRequest{
		Owner: d.signer.Address, CoinType: "0x2::sui::SUI",
	})
	if err != nil || len(gasResp.Data) == 0 {
		return "", fmt.Errorf("no gas coin: %v", err)
	}
	gas := gasResp.Data[0]
	gasRef, err := transaction.NewSuiObjectRef(
		suimodels.SuiAddress(gas.CoinObjectId), gas.Version, suimodels.ObjectDigest(gas.Digest))
	if err != nil {
		return "", fmt.Errorf("gas ref: %w", err)
	}

	tx := transaction.NewTransaction()
	tx.SetSuiClient(d.suiClient).
		SetSigner(d.signer).
		SetSender(suimodels.SuiAddress(d.signer.Address)).
		SetGasPayment([]transaction.SuiObjectRef{*gasRef}).
		SetGasBudget(20_000_000)

	coinArgs := make([]transaction.Argument, 0, len(selected))
	for _, c := range selected {
		ref, rerr := transaction.NewSuiObjectRef(
			suimodels.SuiAddress(c.CoinObjectId), c.Version, suimodels.ObjectDigest(c.Digest))
		if rerr != nil {
			return "", fmt.Errorf("coin ref: %w", rerr)
		}
		coinArgs = append(coinArgs, tx.Object(transaction.CallArg{
			Object: &transaction.ObjectArg{ImmOrOwnedObject: ref},
		}))
	}
	primary := coinArgs[0]
	if len(coinArgs) > 1 {
		tx.MergeCoins(primary, coinArgs[1:])
	}
	exact := tx.SplitCoins(primary, []transaction.Argument{tx.Pure(amount)})
	tx.TransferObjects([]transaction.Argument{exact}, tx.Pure(recipient))

	bcsBytes, err := tx.BuildBCSBytes(ctx)
	if err != nil {
		return "", fmt.Errorf("build transfer: %w", err)
	}
	txB64 := mystenbcs.ToBase64(bcsBytes)
	signed, err := d.signer.SignMessage(txB64, suiconst.TransactionDataIntentScope)
	if err != nil {
		return "", fmt.Errorf("sign transfer: %w", err)
	}
	resp, err := d.suiClient.SuiExecuteTransactionBlock(ctx, suimodels.SuiExecuteTransactionBlockRequest{
		TxBytes:     txB64,
		Signature:   []string{signed.Signature},
		Options:     suimodels.SuiTransactionBlockOptions{ShowEffects: true},
		RequestType: "WaitForLocalExecution",
	})
	if err != nil {
		return "", fmt.Errorf("submit transfer (AMBIGUOUS — may have landed): %w", err)
	}
	if resp.Digest == "" {
		return "", fmt.Errorf("transfer returned empty digest (AMBIGUOUS)")
	}
	if s := resp.Effects.Status.Status; s != "" && s != "success" {
		return "", fmt.Errorf("transfer %s failed on-chain: %s", resp.Digest, resp.Effects.Status.Error)
	}
	return resp.Digest, nil
}

// decimalUnused keeps the import pinned if refs shift during edits.
var _ = decimal.Zero
