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
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	suiconst "github.com/block-vision/sui-go-sdk/constant"
	suimodels "github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/evm"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/services/settlement"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// ErrBaseDispatcherNotConfigured surfaces when a Route-A order in mode=lp
// reaches the dispatch step but the Base EVM client / settlement config is
// incomplete (signer key, chain id, USDC/gateway addresses, etc.).
// Operators should set the BASE_* + SETTLEMENT_* env vars per
// docs/route-a-settlement.md to unblock dispatch.
var ErrBaseDispatcherNotConfigured = errors.New("rails: Base EVM / settlement config missing (see docs/route-a-settlement.md)")

// ErrTreasuryDispatcherNotWired surfaces when a Route-A order in mode=treasury
// reaches dispatch but the BaaS partner integration isn't picked.
var ErrTreasuryDispatcherNotWired = errors.New("rails: BaaS partner not integrated for treasury payout (route-a-spec.md, mode=treasury dispatch)")

const (
	// maxQuoteRetries is the number of consecutive LiFi quote failures
	// before an order is marked failed. Prevents infinite log spam.
	maxQuoteRetries = 10

	// bridgingStaleTimeout is how long an order can stay in 'bridging'
	// with a "tx not found" response before we mark it failed.
	bridgingStaleTimeout = 15 * time.Minute
)

// RouteADispatcher drives Route-A orders through their lifecycle. One
// instance held by tasks.go cron.
type RouteADispatcher struct {
	conf      *config.OrderConfiguration
	suiClient *suisdk.Client
	signer    *suisigner.Signer // aggregator key — signs bridge txs on Sui
	lifi      *lifi.Client

	// EVM + settlement are lazy-initialized on first use so the dispatcher
	// can run for Sui-only flows even when Base env isn't configured.
	// Nil when configuration is incomplete; dispatchLP returns a typed
	// error in that case.
	evm      *evm.Client
	settlement *settlement.Client

	// quoteFailCounts tracks consecutive LiFi quote failures per order
	// ID in memory. Reset on success; orders are marked failed once the
	// count exceeds maxQuoteRetries.
	quoteFailMu     sync.Mutex
	quoteFailCounts map[int]int
}

// NewRouteADispatcher constructs the dispatcher from config. Returns nil
// (with a logged warning) if the aggregator key isn't configured, since
// dispatcher cannot operate without it. Base + settlement clients are
// lazy-initialised on first use — missing env doesn't block Sui-only
// flows.
func NewRouteADispatcher() *RouteADispatcher {
	conf := config.OrderConfig()

	apiClient := suisdk.NewSuiClient(conf.SuiRpcURL)
	suiClient, _ := apiClient.(*suisdk.Client)

	var signer *suisigner.Signer
	if len(conf.SuiAggregatorPrivateKey) == 32 {
		signer = suisigner.NewSigner(conf.SuiAggregatorPrivateKey)
	}

	d := &RouteADispatcher{
		conf:            conf,
		suiClient:       suiClient,
		signer:          signer,
		lifi:            lifi.New(conf.LiFiAPIKey),
		quoteFailCounts: make(map[int]int),
	}

	// settlement client is cheap to construct (no network on init).
	ttl := time.Duration(conf.SettlementPubkeyTTLSeconds) * time.Second
	d.settlement = settlement.New(conf.SettlementAPIURL, ttl)

	// EVM client requires a working RPC connection; dial here so failures
	// surface at boot, but degrade gracefully (nil client + dispatchLP
	// returns ErrBaseDispatcherNotConfigured) when key is missing.
	if d.baseReady() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		evmClient, err := evm.NewClient(ctx, d.baseChainConfig())
		if err != nil {
			logger.Errorf("route-a: EVM client init failed: %v", err)
		} else {
			d.evm = evmClient
		}
	}
	return d
}

// baseReady reports whether the Base config is complete enough to dial.
// Allows the dispatcher to skip evm.NewClient when ops haven't filled in
// the env yet (e.g. fresh dev box, Sui-only test).
func (d *RouteADispatcher) baseReady() bool {
	return d.conf.BaseRpcURL != "" &&
		d.conf.BaseSignerKey != "" &&
		d.conf.BaseGatewayContract != "" &&
		d.conf.BaseUSDCContract != "" &&
		d.conf.BaseChainID != 0
}

// baseChainConfig packs the env-derived Base config into the evm package's
// ChainConfig shape.
func (d *RouteADispatcher) baseChainConfig() evm.ChainConfig {
	name := "base-mainnet"
	if d.conf.BaseChainID == lifi.BaseSepoliaChainID {
		name = "base-sepolia"
	}
	return evm.ChainConfig{
		Name:         name,
		ChainID:      d.conf.BaseChainID,
		RPCURL:       d.conf.BaseRpcURL,
		GatewayAddr:  ethcommon.HexToAddress(d.conf.BaseGatewayContract),
		USDCAddr:     ethcommon.HexToAddress(d.conf.BaseUSDCContract),
		USDCDecimals: uint8(d.conf.BaseUSDCDecimals),
		SignerHex:    d.conf.BaseSignerKey,
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
	if err := d.advanceDispatching(ctx); err != nil {
		logger.Errorf("route-a: advance dispatching: %v", err)
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
			d.quoteFailMu.Lock()
			d.quoteFailCounts[order.ID]++
			count := d.quoteFailCounts[order.ID]
			d.quoteFailMu.Unlock()

			logger.Errorf("route-a: start bridge for order %d (attempt %d/%d): %v",
				order.ID, count, maxQuoteRetries, err)

			if count >= maxQuoteRetries {
				reason := fmt.Sprintf("LiFi quote failed %d consecutive times: %v", count, err)
				if _, uerr := order.Update().
					SetBridgeStatus(routeaorder.BridgeStatusFailed).
					SetFailureReason(reason).
					Save(ctx); uerr != nil {
					logger.Errorf("route-a: persist quote-fail for %d: %v", order.ID, uerr)
				} else {
					logger.Errorf("route-a: order %d marked FAILED after %d quote retries", order.ID, count)
				}
				d.quoteFailMu.Lock()
				delete(d.quoteFailCounts, order.ID)
				d.quoteFailMu.Unlock()
			}
		} else {
			// Success — clear any failure counter.
			d.quoteFailMu.Lock()
			delete(d.quoteFailCounts, order.ID)
			d.quoteFailMu.Unlock()
		}
	}
	return nil
}

// startBridge fetches a quote, signs + submits the source-chain tx, and
// persists the resulting quote ID + tx digest. Transitions pending→bridging.
func (d *RouteADispatcher) startBridge(ctx context.Context, order *ent.RouteAOrder) error {
	if order.Edges.PaymentOrder == nil {
		return fmt.Errorf("route-a order %d has no payment_order edge", order.ID)
	}
	if order.Edges.PaymentOrder.Edges.Token == nil {
		return fmt.Errorf("route-a order %d has no token edge", order.ID)
	}

	po := order.Edges.PaymentOrder
	tok := po.Edges.Token

	// fromAmount is in the source coin's smallest unit. PaymentOrder.Amount
	// is in decimal — convert via the token's decimals.
	fromAmount := po.Amount.Shift(int32(tok.Decimals)).Truncate(0).String()

	qr := lifi.QuoteRequest{
		FromChain:   strconv.FormatInt(lifi.SuiChainID, 10),
		ToChain:     strconv.FormatInt(d.conf.BaseChainID, 10),
		FromToken:   tok.ContractAddress,
		ToToken:     d.conf.BaseUSDCContract,
		FromAmount:  fromAmount,
		FromAddress: d.signer.Address,
		ToAddress:   d.conf.BaseAggregatorAddress,
	}
	quote, err := d.lifi.GetQuote(ctx, qr)
	if err != nil {
		logger.Errorf("route-a: quote request details order=%d fromChain=%s toChain=%s fromToken=%s toToken=%s fromAmount=%s fromAddr=%s toAddr=%s",
			order.ID, qr.FromChain, qr.ToChain, qr.FromToken, qr.ToToken, qr.FromAmount, qr.FromAddress, qr.ToAddress)
		return fmt.Errorf("get quote: %w", err)
	}
	if quote.TransactionRequest.Data == "" {
		return fmt.Errorf("quote returned no transactionRequest.data — LiFi may not route this pair")
	}

	// Sign + submit the source-chain tx. For Sui (MVM) the Data field is a
	// base64-encoded TransactionData; SignTransaction wraps it in the Sui
	// intent prefix and Ed25519-signs.
	// block-vision/sui-go-sdk v1.2.1's Signer.SignTransaction passes
	// PersonalMessageIntentScope (3) where Sui validation expects
	// TransactionDataIntentScope (0). We call SignMessage directly with
	// the right scope so the resulting signature actually verifies.
	signed, err := d.signer.SignMessage(quote.TransactionRequest.Data, suiconst.TransactionDataIntentScope)
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
	logger.Infof("route-a: bridge initiated order=%d tool=%s tx=%s", order.ID, quote.Tool, resp.Digest)
	return nil
}

// advanceBridging polls LiFi /status for every order in 'bridging' state.
// On status=DONE → 'bridged' (+ persist destination tx hash + delivered amount).
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
			ToChain:   strconv.FormatInt(d.conf.BaseChainID, 10),
		})
		if err != nil {
			logger.Errorf("route-a: status poll for %d (tx=%s tool=%s): %v", order.ID, order.BridgeTxSui, order.LifiTool, err)

			// If the tx has been "not found" for longer than bridgingStaleTimeout,
			// mark it failed — the source tx likely never landed on-chain or was
			// dropped. Without this, the order polls forever.
			if strings.Contains(err.Error(), "not found") && time.Since(order.UpdatedAt) > bridgingStaleTimeout {
				reason := fmt.Sprintf("LiFi status returned 'not found' for tx %s after %s", order.BridgeTxSui, bridgingStaleTimeout)
				if _, uerr := order.Update().
					SetBridgeStatus(routeaorder.BridgeStatusFailed).
					SetFailureReason(reason).
					Save(ctx); uerr != nil {
					logger.Errorf("route-a: persist stale-bridging FAILED for %d: %v", order.ID, uerr)
				} else {
					logger.Errorf("route-a: order %d marked FAILED — bridge tx not found after %s", order.ID, bridgingStaleTimeout)
				}
			}
			continue
		}

		switch status.Status {
		case "DONE":
			update := order.Update().SetBridgeStatus(routeaorder.BridgeStatusBridged)
			if status.Receiving != nil {
				if status.Receiving.TxHash != "" {
					update = update.SetBridgeTxDest(status.Receiving.TxHash)
				}
				if status.Receiving.Amount != "" {
					if amt, ok := parseAmountToDecimal(status.Receiving.Amount, status.Receiving.Token.Decimals); ok {
						update = update.SetBridgedAmount(amt)
					}
				}
			}
			if _, err := update.Save(ctx); err != nil {
				logger.Errorf("route-a: persist DONE for %d: %v", order.ID, err)
			} else {
				logger.Infof("route-a: bridge DONE order=%d", order.ID)
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
				logger.Errorf("route-a: persist FAILED for %d: %v", order.ID, err)
			} else {
				logger.Infof("route-a: bridge FAILED order=%d reason=%s", order.ID, reason)
			}

		default:
			// PENDING / NOT_FOUND — keep polling.
		}
	}
	return nil
}

// advanceBridged finds RouteAOrder rows in 'bridged' state (Base USDC has
// arrived in our hot wallet) and triggers dispatch per mode:
//
//   - mode=lp        → call settlement's Gateway on Base (approve + createOrder)
//   - mode=treasury  → trigger BaaS fiat payout (needs BaaS partner integration)
//
// LP path returns ErrBaseDispatcherNotConfigured when EVM config is missing.
// Treasury path returns ErrTreasuryDispatcherNotWired until BaaS is picked.
// Orders stay in 'bridged' on either; the bridged USDC is safe in our wallet.
func (d *RouteADispatcher) advanceBridged(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridged)).
		// dispatchLP needs both the PaymentOrder and its Recipient edge
		// to build the settlement messageHash; eager-load them here so the
		// dispatcher doesn't have to re-query per order.
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithRecipient()
		}).
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
			logger.Errorf("route-a: dispatch order=%d mode=%s: %v", order.ID, order.Mode, dispatchErr)
		}
	}
	return nil
}

// dispatchLP re-enters the settlement Gateway on Base: bridged USDC →
// approve(Gateway, amount+senderFee) → createOrder(...) → capture
// bytes32 orderId from the OrderCreated log → persist + transition to
// `dispatching`. The settlement aggregator picks up the on-chain event,
// routes to a Provision Node, and we learn of fill via advanceDispatching.
//
// Hot-path failures (RPC unreachable, allowance race, revert) leave the
// order in `bridged` for the next tick to retry. Hard failures (config
// missing, invalid recipient) mark the order failed and surface to ops.
func (d *RouteADispatcher) dispatchLP(ctx context.Context, order *ent.RouteAOrder) error {
	if d.evm == nil || d.settlement == nil {
		return ErrBaseDispatcherNotConfigured
	}
	if order.Edges.PaymentOrder == nil {
		return fmt.Errorf("route-a order %d missing payment_order edge", order.ID)
	}
	po := order.Edges.PaymentOrder
	if po.Edges.Recipient == nil {
		return fmt.Errorf("route-a order %d missing recipient edge", order.ID)
	}
	rcpt := po.Edges.Recipient

	// Amount + senderFee must equal what's in our wallet — settlement's
	// Gateway pulls `amount + senderFee` via transferFrom and reverts if
	// our balance is short. The bridged USDC IS our budget (LiFi already
	// took its slippage); we split it into (order amount) + (our skim) so
	// the contract's transfer always fits.
	if order.BridgedAmount == nil || order.BridgedAmount.IsZero() {
		return fmt.Errorf("route-a order %d has zero/nil bridged_amount", order.ID)
	}
	bridged := decimalToSubunit(*order.BridgedAmount, d.conf.BaseUSDCDecimals)
	senderFee := new(big.Int).Quo(
		new(big.Int).Mul(bridged, big.NewInt(d.conf.BaseSenderFeeBPS)),
		big.NewInt(10_000),
	)
	amount := new(big.Int).Sub(bridged, senderFee) // amount + senderFee == bridged
	senderFeeDec := decimal.NewFromBigInt(senderFee, -int32(d.conf.BaseUSDCDecimals))

	// Rate: uint96 fixed-point (NGN per USDC × 100). PaymentOrder.Rate
	// is decimal NGN per USDC.
	rateScaled := order.Edges.PaymentOrder.Rate.Mul(decimal.NewFromInt(100)).Truncate(0)
	rate, _ := new(big.Int).SetString(rateScaled.String(), 10)

	// Build + encrypt the recipient blob.
	pem, err := d.settlement.FetchPublicKey(ctx)
	if err != nil {
		return fmt.Errorf("settlement pubkey: %w", err)
	}
	recipient := settlement.Recipient{
		AccountIdentifier: rcpt.AccountIdentifier,
		AccountName:       rcpt.AccountName,
		Institution:       rcpt.Institution,
		Memo:              rcpt.Memo,
		ProviderID:        rcpt.ProviderID,
		Nonce:             settlement.NewNonce(),
		Metadata:          map[string]string{"apiKey": d.conf.SettlementSenderAPIKeyID},
	}
	messageHash, err := settlement.EncryptRecipient(recipient, pem)
	if err != nil {
		return fmt.Errorf("encrypt recipient: %w", err)
	}

	aggregatorAddr := ethcommon.HexToAddress(d.conf.BaseAggregatorAddress)
	// Approve amount + senderFee in one go.
	total := new(big.Int).Add(amount, senderFee)
	if _, err := d.evm.USDC().Approve(ctx, d.evm.Config().GatewayAddr, total); err != nil {
		return fmt.Errorf("approve USDC: %w", err)
	}

	// Submit createOrder + wait for receipt + parse orderId.
	result, err := d.evm.Gateway().CreateOrder(ctx, evm.CreateOrderParams{
		Token:              d.evm.Config().USDCAddr,
		Amount:             amount,
		Rate:               rate,
		SenderFeeRecipient: aggregatorAddr, // we collect our own skim
		SenderFee:          senderFee,
		RefundAddress:      aggregatorAddr, // refunds bounce back to us; ops handles reverse-bridge
		MessageHash:        messageHash,
	})
	if err != nil {
		return fmt.Errorf("createOrder: %w", err)
	}

	orderID := strings.ToLower(result.OrderID.Hex())
	if _, err := order.Update().
		SetGatewayOrderID(orderID).
		SetGatewayChainID(uint64(d.conf.BaseChainID)).
		SetSenderFeeSubunit(senderFeeDec).
		SetBridgeTxDest(result.TxHash.Hex()).
		SetBridgeStatus(routeaorder.BridgeStatusDispatching).
		Save(ctx); err != nil {
		return fmt.Errorf("persist dispatching: %w", err)
	}
	logger.Infof("route-a: createOrder submitted order=%d orderId=%s tx=%s gas=%d",
		order.ID, orderID, result.TxHash.Hex(), result.GasUsed)
	return nil
}

// dispatchTreasury triggers a fiat payout to the merchant from our centralized
// treasury via the BaaS partner. Requires BaaS partner selection +
// integration in services/baas/. Returns ErrTreasuryDispatcherNotWired
// until that lands.
func (d *RouteADispatcher) dispatchTreasury(_ context.Context, _ *ent.RouteAOrder) error {
	return ErrTreasuryDispatcherNotWired
}

// advanceDispatching polls settlement's /v1/orders/:chainId/:orderId for every
// order in `dispatching` state and transitions on terminal status. Live
// (non-terminal) status updates are persisted to settlement_status AND
// fanned out through SenderEventBus so the merchant app's
// /v1/sender/me/payments/stream sees them in real time.
func (d *RouteADispatcher) advanceDispatching(ctx context.Context) error {
	if d.settlement == nil {
		return nil
	}
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithSenderProfile()
		}).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		if order.GatewayOrderID == "" || order.GatewayChainID == 0 {
			continue
		}
		info, err := d.settlement.FetchOrderStatus(ctx, int64(order.GatewayChainID), order.GatewayOrderID)
		if err != nil {
			logger.Errorf("route-a: settlement status %d: %v", order.ID, err)
			continue
		}
		// Only publish on actual change so every 30s poll doesn't re-fire
		// the same state into the SSE stream.
		statusChanged := order.SettlementStatus != string(info.Status)
		now := time.Now()
		upd := order.Update().
			SetSettlementStatus(string(info.Status)).
			SetSettlementPolledAt(now)

		var (
			bridgeFlipped routeaorder.BridgeStatus
			poFlipped     paymentorder.Status
		)
		switch info.Status {
		case settlement.StatusSettled:
			upd = upd.SetBridgeStatus(routeaorder.BridgeStatusSettled)
			bridgeFlipped = routeaorder.BridgeStatusSettled
			poFlipped = paymentorder.StatusSettled
			if order.Edges.PaymentOrder != nil {
				if _, perr := order.Edges.PaymentOrder.Update().
					SetStatus(paymentorder.StatusSettled).
					Save(ctx); perr != nil {
					logger.Errorf("route-a: payment_order → settled (%d): %v", order.ID, perr)
				}
			}
		case settlement.StatusRefunded, settlement.StatusExpired:
			upd = upd.SetBridgeStatus(routeaorder.BridgeStatusRefunded)
			bridgeFlipped = routeaorder.BridgeStatusRefunded
			poFlipped = paymentorder.StatusRefunded
			if order.Edges.PaymentOrder != nil {
				if _, perr := order.Edges.PaymentOrder.Update().
					SetStatus(paymentorder.StatusRefunded).
					Save(ctx); perr != nil {
					logger.Errorf("route-a: payment_order → refunded (%d): %v", order.ID, perr)
				}
			}
		}
		if _, err := upd.Save(ctx); err != nil {
			logger.Errorf("route-a: persist dispatching → %s (%d): %v", info.Status, order.ID, err)
		}

		if statusChanged {
			d.publishOrderEvent(order, info.Status, bridgeFlipped, poFlipped)
		}
	}
	return nil
}

// publishOrderEvent fans a settlement status change out to the merchant
// SSE stream via the existing SenderEventBus. Coarse event names
// (payment.processing / .settled / .refunded) keep the protocol stable
// while the `settlement_status` field in the payload carries the
// fine-grained sub-state settlement emitted (pending / fulfilling /
// validated / settling / refunding / etc.).
func (d *RouteADispatcher) publishOrderEvent(
	order *ent.RouteAOrder,
	pcStatus settlement.OrderStatus,
	bridgeFlipped routeaorder.BridgeStatus,
	poFlipped paymentorder.Status,
) {
	if order.Edges.PaymentOrder == nil || order.Edges.PaymentOrder.Edges.SenderProfile == nil {
		return
	}
	senderID := order.Edges.PaymentOrder.Edges.SenderProfile.ID

	eventName := "payment.processing"
	switch {
	case bridgeFlipped == routeaorder.BridgeStatusSettled:
		eventName = "payment.settled"
	case bridgeFlipped == routeaorder.BridgeStatusRefunded:
		eventName = "payment.refunded"
	}

	payload := map[string]any{
		"order_id":         order.Edges.PaymentOrder.ID.String(),
		"route_a_order_id": order.ID,
		"bridge_status":    string(order.BridgeStatus),
		"settlement_status":  string(pcStatus),
		"gateway_order_id": order.GatewayOrderID,
		"dest_tx_hash":     order.BridgeTxDest,
		"chain_id":         order.GatewayChainID,
		"updated_at":       time.Now().UTC().Format(time.RFC3339),
	}
	if poFlipped != "" {
		payload["payment_status"] = string(poFlipped)
	}
	Bus().Publish(senderID, eventName, payload)
}

// CheckNativeBalance reads the aggregator wallet's native-token (ETH on
// Base) balance and logs an error when it drops below
// BASE_NATIVE_LOW_THRESHOLD_WEI. Runs on a 5-minute cron from tasks.go.
// Logged at Error so existing log-shipper hooks surface it; when a
// dedicated notifications package lands, swap for a direct Slack push.
func (d *RouteADispatcher) CheckNativeBalance(ctx context.Context) error {
	if d.evm == nil {
		return nil
	}
	thresholdStr := d.conf.BaseNativeLowThresholdWei
	if thresholdStr == "" {
		return nil
	}
	threshold, ok := new(big.Int).SetString(thresholdStr, 10)
	if !ok {
		return fmt.Errorf("BASE_NATIVE_LOW_THRESHOLD_WEI is not a valid big.Int: %q", thresholdStr)
	}
	bal, err := d.evm.BalanceNative(ctx)
	if err != nil {
		return fmt.Errorf("query native balance: %w", err)
	}
	if bal.Cmp(threshold) < 0 {
		logger.Errorf(
			"route-a: Base aggregator wallet LOW on native (ETH) — balance=%s wei, threshold=%s wei, address=%s. Top up before dispatches stall.",
			bal.String(), threshold.String(), d.evm.From().Hex(),
		)
	}
	return nil
}

// decimalToSubunit converts a decimal-denominated USDC value into the
// big.Int subunit representation expected by Solidity. Handles BSC's
// 18-decimal Binance-Peg USDC vs e.g. mainnet ETH's 6-decimal native USDC.
func decimalToSubunit(d decimal.Decimal, decimals int) *big.Int {
	shifted := d.Shift(int32(decimals)).Truncate(0).String()
	n, _ := new(big.Int).SetString(shifted, 10)
	if n == nil {
		return big.NewInt(0)
	}
	return n
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
