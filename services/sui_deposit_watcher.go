// sui_deposit_watcher.go is the Path-2 deposit detector. It scans active
// SuiReceiveAddress rows for incoming Coin<USDC> objects via Sui's
// sui_getCoins JSON-RPC, flips status to "deposited" when a matching deposit
// lands, and forwards the coin into the Gateway escrow via
// OrderSui.CreateOrder.
//
// Lifecycle per address:
//
//	unused      → (deposit detected)       → deposited
//	deposited   → (forwarded successfully) → forwarded
//	unused      → (valid_until passed)     → expired
//
// Each CheckDeposits tick does three passes in order: expire stale, detect
// new deposits, forward deposited. Forwarding is idempotent (status guards
// re-entry), so a failed forward on one tick simply retries on the next.
//
// Cadence: wired into StartCronJobs to run every minute. Total user-facing
// latency = exchange withdrawal time (~1–5 min) + up-to-1 min poll delay.
// For sub-second detection at scale, swap polling for sui_subscribeEvent
// filtered to coin-transfer events to the receive-address set.
package services

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/sui"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	"github.com/usezoracle/rails-sui/services/contracts"
	orderpkg "github.com/usezoracle/rails-sui/services/order"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// SuiDepositWatcher polls per-order Sui receive addresses for incoming
// deposits and forwards them into the Gateway escrow.
type SuiDepositWatcher struct {
	suiClient    *sui.Client
	orderService types.OrderService
}

// NewSuiDepositWatcher constructs the watcher from config.
func NewSuiDepositWatcher() *SuiDepositWatcher {
	conf := config.OrderConfig()
	apiClient := sui.NewSuiClient(conf.SuiRpcURL)
	client, _ := apiClient.(*sui.Client)
	return &SuiDepositWatcher{
		suiClient:    client,
		orderService: orderpkg.NewOrderSui(),
	}
}

// CheckDeposits runs one watcher pass:
//  1. Mark unused-and-past-validity addresses as expired.
//  2. For each unused-and-still-valid address, query sui_getCoins; if a
//     matching deposit is present, flip status → deposited and persist the
//     deposit tx digest.
//  3. For each address in 'deposited' status, call OrderSui.CreateOrder to
//     build + submit the forwarding PTB.
//
// Returns the first hard error encountered. Per-address errors are logged
// and do not abort the pass — one bad address shouldn't block the rest.
func (w *SuiDepositWatcher) CheckDeposits(ctx context.Context) error {
	if err := w.expireStale(ctx); err != nil {
		return fmt.Errorf("sui deposit watcher: expire stale: %w", err)
	}
	if err := w.detectDeposits(ctx); err != nil {
		return fmt.Errorf("sui deposit watcher: detect: %w", err)
	}
	if err := w.forwardDeposits(ctx); err != nil {
		return fmt.Errorf("sui deposit watcher: forward: %w", err)
	}
	if err := w.reconcileForwardedRouteA(ctx); err != nil {
		return fmt.Errorf("sui deposit watcher: reconcile route-a: %w", err)
	}
	return nil
}

// expireStale flips status to 'expired' for any unused address whose
// valid_until has passed. Skips addresses with zero valid_until (private
// orders that never expire — memo prefix "P#P").
func (w *SuiDepositWatcher) expireStale(ctx context.Context) error {
	_, err := db.Client.SuiReceiveAddress.
		Update().
		Where(
			suireceiveaddress.StatusEQ(suireceiveaddress.StatusUnused),
			suireceiveaddress.ValidUntilLT(time.Now()),
			suireceiveaddress.ValidUntilNEQ(time.Time{}),
		).
		SetStatus(suireceiveaddress.StatusExpired).
		Save(ctx)
	return err
}

// detectDeposits queries sui_getCoins for each unused-and-still-valid
// address and flips status to 'deposited' when a coin with sufficient
// balance + matching type is found.
func (w *SuiDepositWatcher) detectDeposits(ctx context.Context) error {
	unused, err := db.Client.SuiReceiveAddress.
		Query().
		Where(
			suireceiveaddress.StatusEQ(suireceiveaddress.StatusUnused),
			suireceiveaddress.Or(
				suireceiveaddress.ValidUntilGTE(time.Now()),
				suireceiveaddress.ValidUntilEQ(time.Time{}),
			),
		).
		All(ctx)
	if err != nil {
		return err
	}

	for _, addr := range unused {
		coin, txDigest, found, observedBalance, err := w.findMatchingDeposit(ctx, addr)

		// Resolve the linked route_a_order id (if any) so audit-log
		// rows attach to it. Non-Route-A payment orders don't get
		// audit rows from the watcher — that's by design (the audit
		// table is Route-A-scoped).
		routeAOrderID := w.linkedRouteAOrderID(ctx, addr)

		if err != nil {
			logger.Errorf("sui deposit watcher: query coins for %s: %v", addr.Address, err)
			if routeAOrderID != 0 {
				LogOnce(ctx, routeAOrderID, StepDepositCheck, StatusFailed,
					ActorWatcher,
					map[string]any{
						"receive_address": addr.Address,
						"coin_type":       addr.CoinType,
					},
					err.Error(), "")
			}
			continue
		}
		if !found {
			// CRITICAL: this was the silent path that caused the
			// 2026-05-25 incident. The watcher checked, the deposit
			// didn't match `balance >= expected` (off-by-1429 from
			// USDC precision rounding), and we moved on without any
			// trace. Now every miss leaves a row showing exactly
			// what we saw vs expected so reconciliation has signal.
			if routeAOrderID != 0 {
				LogOnce(ctx, routeAOrderID, StepDepositCheck, StatusSkipped,
					ActorWatcher,
					map[string]any{
						"receive_address":  addr.Address,
						"coin_type":        addr.CoinType,
						"expected_amount":  addr.ExpectedAmount,
						"observed_balance": observedBalance,
						"short_by":         int64(addr.ExpectedAmount) - observedBalance,
					},
					"observed balance below expected", "")
			}
			continue
		}

		updateBuilder := addr.Update().SetStatus(suireceiveaddress.StatusDeposited)
		if txDigest != "" {
			updateBuilder = updateBuilder.SetDepositTxDigest(txDigest)
		}
		if _, err := updateBuilder.Save(ctx); err != nil {
			logger.Errorf("sui deposit watcher: mark %s deposited: %v", addr.Address, err)
			if routeAOrderID != 0 {
				LogOnce(ctx, routeAOrderID, StepDepositDetected, StatusFailed,
					ActorWatcher,
					map[string]any{"receive_address": addr.Address},
					err.Error(), "")
			}
			continue
		}
		logger.Infof("sui deposit watcher: detected deposit at %s (coin=%s balance=%s expected=%d tx=%s)",
			addr.Address, coin.CoinObjectId, coin.Balance, addr.ExpectedAmount, txDigest)
		if routeAOrderID != 0 {
			LogOnce(ctx, routeAOrderID, StepDepositDetected, StatusSucceeded,
				ActorWatcher,
				map[string]any{
					"receive_address": addr.Address,
					"coin_object_id":  coin.CoinObjectId,
					"balance":         coin.Balance,
					"expected_amount": addr.ExpectedAmount,
					"deposit_tx":      txDigest,
				},
				"", "")
		}
	}
	return nil
}

// linkedRouteAOrderID returns the route_a_order id associated with this
// receive address, if any. Returns 0 when the receive address belongs
// to a non-Route-A payment order or the lookup fails — those orders
// don't write to the Route-A audit log.
func (w *SuiDepositWatcher) linkedRouteAOrderID(
	ctx context.Context, addr *ent.SuiReceiveAddress,
) int {
	po, err := addr.QueryPaymentOrder().WithRouteAOrder().Only(ctx)
	if err != nil || po == nil || po.Edges.RouteAOrder == nil {
		return 0
	}
	return po.Edges.RouteAOrder.ID
}

// findMatchingDeposit queries sui_getCoins for the address + coin_type and
// returns the first coin whose balance meets or exceeds expected_amount.
//
// Also returns the total observed balance across all coins of the type
// (signed int64) so the caller can record it on a `skipped` audit row —
// makes precision-mismatch incidents (like 2026-05-25) self-diagnosing.
func (w *SuiDepositWatcher) findMatchingDeposit(
	ctx context.Context, addr *ent.SuiReceiveAddress,
) (models.CoinData, string, bool, int64, error) {
	resp, err := w.suiClient.SuiXGetCoins(ctx, models.SuiXGetCoinsRequest{
		Owner:    addr.Address,
		CoinType: addr.CoinType,
		Limit:    50,
	})
	if err != nil {
		return models.CoinData{}, "", false, 0, err
	}

	var totalObserved int64
	for _, coin := range resp.Data {
		balance, parseErr := strconv.ParseUint(coin.Balance, 10, 64)
		if parseErr != nil {
			continue
		}
		totalObserved += int64(balance)
		if balance >= addr.ExpectedAmount {
			return coin, coin.PreviousTransaction, true, totalObserved, nil
		}
	}
	return models.CoinData{}, "", false, totalObserved, nil
}

// forwardDeposits calls OrderSui.CreateOrder for every address in 'deposited'
// status. CreateOrder transitions the address to 'forwarded' on success;
// failures are logged and retried on the next tick (idempotent).
func (w *SuiDepositWatcher) forwardDeposits(ctx context.Context) error {
	deposited, err := db.Client.SuiReceiveAddress.
		Query().
		Where(suireceiveaddress.StatusEQ(suireceiveaddress.StatusDeposited)).
		WithPaymentOrder().
		All(ctx)
	if err != nil {
		return err
	}

	for _, addr := range deposited {
		if addr.Edges.PaymentOrder == nil {
			logger.Errorf("sui deposit watcher: address %s has no linked payment order — orphan", addr.Address)
			continue
		}
		// Wrap each CreateOrder in a panic recovery. Without this, a
		// panic deep in the Sui SDK (e.g., strings.Repeat with a
		// negative count from a too-long address-validation call —
		// the bug that crashed Rails on 2026-05-26 over a long
		// base64 messageHash) propagates up through the gocron
		// executor and exits the entire process. Per-order panics
		// must be contained so the rest of the pipeline keeps
		// running and the next watcher tick retries.
		safeForward(ctx, w.orderService, addr)
	}
	return nil
}

// safeForward calls orderService.CreateOrder with a deferred
// recover() so a panic in one order doesn't crash the process.
// Recovered panics are logged at error level and the address is
// left in 'deposited' so the next tick retries (idempotent).
func safeForward(ctx context.Context, svc types.OrderService, addr *ent.SuiReceiveAddress) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("sui deposit watcher: PANIC forward %s (order=%s): %v",
				addr.Address, addr.Edges.PaymentOrder.ID, r)
		}
	}()
	if err := svc.CreateOrder(ctx, addr.Edges.PaymentOrder.ID); err != nil {
		// Errors are non-fatal: address stays in 'deposited' and the next
		// tick retries. Only surface unexpected ones (not the "not in deposited"
		// guard from CreateOrder itself, which fires on race-conditions).
		if !errors.Is(err, orderpkg.ErrCreateOrderPath1OnlyClientSide) {
			logger.Errorf("sui deposit watcher: forward %s (order=%s): %v",
				addr.Address, addr.Edges.PaymentOrder.ID, err)
		}
	}
}

// reconcileForwardedRouteA closes the reliability gap between the Path-2
// deposit watcher and the live Sui event indexer. A successful forward tx
// creates a Gateway Order and emits OrderCreated, but the event subscription is
// live-only: deploys, reconnects, provider gaps, or older buggy event matching
// can miss it. Route A cannot progress until the Gateway escrow is self-settled
// to the aggregator wallet, so recover the order_id from the recorded forward
// transaction and perform the same self-settle action idempotently.
func (w *SuiDepositWatcher) reconcileForwardedRouteA(ctx context.Context) error {
	orderSui, ok := w.orderService.(*orderpkg.OrderSui)
	if !ok || orderSui == nil {
		return nil
	}

	addrs, err := db.Client.SuiReceiveAddress.
		Query().
		Where(
			suireceiveaddress.StatusEQ(suireceiveaddress.StatusForwarded),
			suireceiveaddress.ForwardTxDigestNotNil(),
			suireceiveaddress.ForwardTxDigestNEQ(""),
		).
		WithPaymentOrder(func(q *ent.PaymentOrderQuery) {
			q.WithRouteAOrder()
			q.WithToken()
		}).
		All(ctx)
	if err != nil {
		return err
	}

	for _, addr := range addrs {
		po := addr.Edges.PaymentOrder
		if po == nil || po.Edges.RouteAOrder == nil || po.Edges.Token == nil {
			continue
		}
		ro := po.Edges.RouteAOrder
		if ro.BridgeStatus != routeaorder.BridgeStatusPending &&
			ro.BridgeStatus != routeaorder.BridgeStatusAwaitingFunds {
			continue
		}

		gatewayOrderID := po.GatewayID
		if gatewayOrderID == "" {
			var lookupErr error
			gatewayOrderID, lookupErr = w.gatewayOrderIDFromForwardTx(ctx, addr.ForwardTxDigest)
			if lookupErr != nil {
				logger.Errorf("sui deposit watcher: route-a reconcile order=%s tx=%s: %v",
					po.ID, addr.ForwardTxDigest, lookupErr)
				LogOnce(ctx, ro.ID, StepOrderCreatedEvent, StatusFailed, ActorReconciler,
					map[string]any{"forward_tx": addr.ForwardTxDigest},
					lookupErr.Error(), addr.ForwardTxDigest)
				continue
			}
			if _, err := po.Update().
				SetGatewayID(gatewayOrderID).
				SetTxHash(addr.ForwardTxDigest).
				Save(ctx); err != nil {
				logger.Errorf("sui deposit watcher: route-a reconcile persist gateway order=%s tx=%s: %v",
					po.ID, addr.ForwardTxDigest, err)
				continue
			}
			LogOnce(ctx, ro.ID, StepOrderCreatedEvent, StatusSucceeded, ActorReconciler,
				map[string]any{
					"gateway_order_id": gatewayOrderID,
					"forward_tx":       addr.ForwardTxDigest,
				},
				"", addr.ForwardTxDigest)
		}

		if hasSuccessfulRouteAEvent(ctx, ro.ID, StepSelfSettle) {
			continue
		}
		if err := orderSui.SelfSettleToAggregator(ctx, gatewayOrderID, addr.CoinType); err != nil {
			logger.Errorf("sui deposit watcher: route-a reconcile self-settle order=%s gateway=%s: %v",
				po.ID, gatewayOrderID, err)
			LogOnce(ctx, ro.ID, StepSelfSettle, StatusFailed, ActorReconciler,
				map[string]any{
					"gateway_order_id": gatewayOrderID,
					"coin_type":        addr.CoinType,
					"forward_tx":       addr.ForwardTxDigest,
				},
				err.Error(), addr.ForwardTxDigest)
			continue
		}
		LogOnce(ctx, ro.ID, StepSelfSettle, StatusSucceeded, ActorReconciler,
			map[string]any{
				"gateway_order_id": gatewayOrderID,
				"coin_type":        addr.CoinType,
				"forward_tx":       addr.ForwardTxDigest,
			},
			"", addr.ForwardTxDigest)
		// Funds just landed at the aggregator — burst the dispatcher so
		// the bridge starts within seconds instead of at the next tick.
		KickRouteA()
	}
	return nil
}

func (w *SuiDepositWatcher) gatewayOrderIDFromForwardTx(ctx context.Context, txDigest string) (string, error) {
	resp, err := w.suiClient.SuiGetTransactionBlock(ctx, models.SuiGetTransactionBlockRequest{
		Digest: txDigest,
		Options: models.SuiTransactionBlockOptions{
			ShowEvents: true,
		},
	})
	if err != nil {
		return "", fmt.Errorf("get forward transaction: %w", err)
	}
	if !suiTxSuccess(resp) {
		return "", fmt.Errorf("forward transaction did not succeed: %s", txDigest)
	}
	for _, evt := range resp.Events {
		if !eventTypeMatches(config.OrderConfig().SuiGatewayPackageID, evt.Type, contracts.EventOrderCreated) {
			continue
		}
		orderID := getString(evt.ParsedJson, "order_id")
		if orderID == "" {
			return "", fmt.Errorf("OrderCreated event missing order_id in tx %s", txDigest)
		}
		return orderID, nil
	}
	return "", fmt.Errorf("OrderCreated event not found in forward tx %s", txDigest)
}

func suiTxSuccess(resp models.SuiTransactionBlockResponse) bool {
	if resp.Digest == "" {
		return false
	}
	if resp.Effects.Status.Status == "" {
		return true
	}
	return resp.Effects.Status.Status == "success"
}
