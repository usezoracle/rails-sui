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
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
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
		if err := w.orderService.CreateOrder(ctx, addr.Edges.PaymentOrder.ID); err != nil {
			// Errors are non-fatal: address stays in 'deposited' and the next
			// tick retries. Only surface unexpected ones (not the "not in deposited"
			// guard from CreateOrder itself, which fires on race-conditions).
			if !errors.Is(err, orderpkg.ErrCreateOrderPath1OnlyClientSide) {
				logger.Errorf("sui deposit watcher: forward %s (order=%s): %v",
					addr.Address, addr.Edges.PaymentOrder.ID, err)
			}
		}
	}
	return nil
}
