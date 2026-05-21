// route_a_dispatcher.go orchestrates the Route-A (LiFi bridging) flow.
//
// Lifecycle per RouteAOrder:
//
//	pending      → quote retrieved + Sui-side bridge tx submitted
//	bridging     → polling LiFi /status (every minute) until DONE/FAILED
//	bridged      → BSC USDC sitting in our hot wallet, ready to dispatch
//	dispatching  → dispatch in progress (BSC EVM Gateway re-entry, or treasury fiat payout)
//	settled      → fiat hit merchant
//	failed       → bridge or dispatch failed; reconciliation triggers refund
//
// The dispatcher's three entry points correspond to the three driveable
// transitions; the cron in tasks.go calls them in order each tick.
package services

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	suimodels "github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/lifi"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// ErrBSCDispatcherNotWired surfaces when a Route-A order in mode=lp reaches
// the dispatch step but the BSC EVM Gateway client isn't integrated.
// Tracked as a real external dependency: see docs/route-a-spec.md.
var ErrBSCDispatcherNotWired = errors.New("rails: BSC EVM Gateway client not integrated (route-a-spec.md, mode=lp dispatch)")

// ErrTreasuryDispatcherNotWired surfaces when a Route-A order in mode=treasury
// reaches dispatch but the BaaS partner integration isn't picked.
var ErrTreasuryDispatcherNotWired = errors.New("rails: BaaS partner not integrated for treasury payout (route-a-spec.md, mode=treasury dispatch)")

// Default BSC USDC type (Circle native) used as LiFi's toToken. Canonical
// and immutable on BSC mainnet.
const bscUSDCAddress = "0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d"

// RouteADispatcher drives Route-A orders through their lifecycle. One
// instance held by tasks.go cron.
type RouteADispatcher struct {
	conf      *config.OrderConfiguration
	suiClient *suisdk.Client
	signer    *suisigner.Signer // aggregator key — signs bridge txs on Sui
	lifi      *lifi.Client
}

// NewRouteADispatcher constructs the dispatcher from config. Returns nil
// (with a logged warning) if the aggregator key isn't configured, since
// dispatcher cannot operate without it.
func NewRouteADispatcher() *RouteADispatcher {
	conf := config.OrderConfig()

	apiClient := suisdk.NewSuiClient(conf.SuiRpcURL)
	suiClient, _ := apiClient.(*suisdk.Client)

	var signer *suisigner.Signer
	if len(conf.SuiAggregatorPrivateKey) == 32 {
		signer = suisigner.NewSigner(conf.SuiAggregatorPrivateKey)
	}

	return &RouteADispatcher{
		conf:      conf,
		suiClient: suiClient,
		signer:    signer,
		lifi:      lifi.New(conf.LiFiAPIKey),
	}
}

// Tick runs one full pass through the dispatcher: advance any pending,
// bridging, bridged, or dispatching orders. Safe to call repeatedly; each
// state guard prevents re-entry.
//
// Per-order errors are logged but do not abort the tick — one stuck order
// shouldn't block the rest.
func (d *RouteADispatcher) Tick(ctx context.Context) error {
	if d.signer == nil {
		return errors.New("route-a dispatcher: SUI_AGGREGATOR_PRIVATE_KEY not configured")
	}
	if err := d.advancePending(ctx); err != nil {
		logger.Errorf("route-a: advance pending: %v", err)
	}
	if err := d.advanceBridging(ctx); err != nil {
		logger.Errorf("route-a: advance bridging: %v", err)
	}
	if err := d.advanceBridged(ctx); err != nil {
		logger.Errorf("route-a: advance bridged: %v", err)
	}
	return nil
}

// advancePending finds RouteAOrder rows in 'pending' state, fetches a LiFi
// quote, submits the Sui-side bridge tx, and transitions to 'bridging'.
func (d *RouteADispatcher) advancePending(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusPending)).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithToken()
		}).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		if err := d.startBridge(ctx, order); err != nil {
			logger.Errorf("route-a: start bridge for order %s: %v", order.ID, err)
			// Don't flip to failed on the first attempt — surface errors and let
			// the next tick retry. We only mark failed on hard rejections (bad
			// payload, etc.) which are unlikely from LiFi's quote endpoint.
		}
	}
	return nil
}

// startBridge fetches a quote, signs + submits the source-chain tx, and
// persists the resulting quote ID + tx digest. Transitions pending→bridging.
func (d *RouteADispatcher) startBridge(ctx context.Context, order *ent.RouteAOrder) error {
	if order.Edges.PaymentOrder == nil {
		return fmt.Errorf("route-a order %s has no payment_order edge", order.ID)
	}
	if order.Edges.PaymentOrder.Edges.Token == nil {
		return fmt.Errorf("route-a order %s has no token edge", order.ID)
	}

	po := order.Edges.PaymentOrder
	tok := po.Edges.Token

	// fromAmount is in the source coin's smallest unit. PaymentOrder.Amount
	// is in decimal — convert via the token's decimals.
	fromAmount := po.Amount.Shift(int32(tok.Decimals)).Truncate(0).String()

	quote, err := d.lifi.GetQuote(ctx, lifi.QuoteRequest{
		FromChain:   strconv.FormatInt(lifi.SuiChainID, 10),
		ToChain:     strconv.FormatInt(lifi.BSCChainID, 10),
		FromToken:   tok.ContractAddress,
		ToToken:     bscUSDCAddress,
		FromAmount:  fromAmount,
		FromAddress: d.signer.Address,
		ToAddress:   d.conf.BSCAggregatorAddress,
	})
	if err != nil {
		return fmt.Errorf("get quote: %w", err)
	}
	if quote.TransactionRequest.Data == "" {
		return fmt.Errorf("quote returned no transactionRequest.data — LiFi may not route this pair")
	}

	// Sign + submit the source-chain tx. For Sui (MVM) the Data field is a
	// base64-encoded TransactionData; SignTransaction wraps it in the Sui
	// intent prefix and Ed25519-signs.
	signed, err := d.signer.SignTransaction(quote.TransactionRequest.Data)
	if err != nil {
		return fmt.Errorf("sign bridge tx: %w", err)
	}
	resp, err := d.suiClient.SuiExecuteTransactionBlock(ctx, suimodels.SuiExecuteTransactionBlockRequest{
		TxBytes:     quote.TransactionRequest.Data,
		Signature:   []string{signed.Signature},
		Options:     suimodels.SuiTransactionBlockOptions{ShowEffects: true},
		RequestType: "WaitForLocalExecution",
	})
	if err != nil {
		return fmt.Errorf("submit bridge tx: %w", err)
	}
	if resp.Digest == "" {
		return fmt.Errorf("bridge tx submission returned empty digest")
	}

	if _, err := order.Update().
		SetLifiQuoteID(quote.ID).
		SetLifiTool(quote.Tool).
		SetBridgeTxSui(resp.Digest).
		SetBridgeStatus(routeaorder.BridgeStatusBridging).
		Save(ctx); err != nil {
		return fmt.Errorf("persist bridging state: %w", err)
	}
	logger.Infof("route-a: bridge initiated order=%s tool=%s tx=%s", order.ID, quote.Tool, resp.Digest)
	return nil
}

// advanceBridging polls LiFi /status for every order in 'bridging' state.
// On status=DONE → 'bridged' (+ persist BSC tx hash + delivered amount).
// On status=FAILED → 'failed' (+ persist failure reason; reconciliation refunds).
func (d *RouteADispatcher) advanceBridging(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridging)).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		if order.BridgeTxSui == "" {
			continue
		}
		status, err := d.lifi.GetStatus(ctx, lifi.StatusRequest{
			TxHash:    order.BridgeTxSui,
			Bridge:    order.LifiTool,
			FromChain: strconv.FormatInt(lifi.SuiChainID, 10),
			ToChain:   strconv.FormatInt(lifi.BSCChainID, 10),
		})
		if err != nil {
			logger.Errorf("route-a: status poll for %s: %v", order.ID, err)
			continue
		}

		switch status.Status {
		case "DONE":
			update := order.Update().SetBridgeStatus(routeaorder.BridgeStatusBridged)
			if status.Receiving != nil {
				if status.Receiving.TxHash != "" {
					update = update.SetBridgeTxBsc(status.Receiving.TxHash)
				}
				if status.Receiving.Amount != "" {
					if amt, ok := parseAmountToDecimal(status.Receiving.Amount, status.Receiving.Token.Decimals); ok {
						update = update.SetBridgedAmount(amt)
					}
				}
			}
			if _, err := update.Save(ctx); err != nil {
				logger.Errorf("route-a: persist DONE for %s: %v", order.ID, err)
			} else {
				logger.Infof("route-a: bridge DONE order=%s", order.ID)
			}

		case "FAILED":
			reason := status.SubstatusMsg
			if reason == "" {
				reason = status.Substatus
			}
			if _, err := order.Update().
				SetBridgeStatus(routeaorder.BridgeStatusFailed).
				SetFailureReason(reason).
				Save(ctx); err != nil {
				logger.Errorf("route-a: persist FAILED for %s: %v", order.ID, err)
			} else {
				logger.Infof("route-a: bridge FAILED order=%s reason=%s", order.ID, reason)
			}

		default:
			// PENDING / NOT_FOUND — keep polling.
		}
	}
	return nil
}

// advanceBridged finds RouteAOrder rows in 'bridged' state (BSC USDC has
// arrived in our hot wallet) and triggers dispatch per mode:
//
//   - mode=lp        → re-enter the EVM Gateway on BSC (needs BSC client)
//   - mode=treasury  → trigger BaaS fiat payout (needs BaaS partner integration)
//
// Both dispatchers currently return typed errors documenting the missing
// external integration. Orders stay in 'bridged' until the integrations land;
// the bridged USDC is safe in our hot wallet.
func (d *RouteADispatcher) advanceBridged(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridged)).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		var dispatchErr error
		switch order.Mode {
		case routeaorder.ModeLp:
			dispatchErr = d.dispatchLP(ctx, order)
		case routeaorder.ModeTreasury:
			dispatchErr = d.dispatchTreasury(ctx, order)
		default:
			dispatchErr = fmt.Errorf("unknown mode %q", order.Mode)
		}
		if dispatchErr != nil {
			logger.Errorf("route-a: dispatch order=%s mode=%s: %v", order.ID, order.Mode, dispatchErr)
		}
	}
	return nil
}

// dispatchLP re-enters the EVM Gateway on BSC: bridged USDC → BSC LP via the
// existing EVM rails. Requires a BSC EVM client that doesn't exist in this
// repo (Sui-only port stripped it). Returns ErrBSCDispatcherNotWired until
// that integration lands.
func (d *RouteADispatcher) dispatchLP(_ context.Context, _ *ent.RouteAOrder) error {
	return ErrBSCDispatcherNotWired
}

// dispatchTreasury triggers a fiat payout to the merchant from our centralized
// treasury via the BaaS partner. Requires BaaS partner selection +
// integration in services/baas/. Returns ErrTreasuryDispatcherNotWired
// until that lands.
func (d *RouteADispatcher) dispatchTreasury(_ context.Context, _ *ent.RouteAOrder) error {
	return ErrTreasuryDispatcherNotWired
}

// parseAmountToDecimal converts a LiFi "amount" string (smallest unit, as
// string for big-integer safety) into a decimal scaled by `decimals`.
func parseAmountToDecimal(amount string, decimals int) (decimal.Decimal, bool) {
	d, err := decimal.NewFromString(amount)
	if err != nil {
		return decimal.Zero, false
	}
	return d.Shift(-int32(decimals)), true
}
