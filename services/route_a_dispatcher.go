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
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/services/cctp"
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

// ErrAwaitingDepositAtAggregator is returned by startBridge when the
// aggregator wallet doesn't yet hold the source-coin balance the bridge
// would spend. This is the *expected* state between order creation and
// the indexer's SelfSettleToAggregator completing — NOT a failure.
//
// advancePending recognizes this sentinel and silently skips the order
// (no retry counter bump, no FAILED transition). Once the indexer (or
// the deposit-reconciliation cron) self-settles the funds, the next
// tick proceeds normally.
//
// Without this guard, indexer hiccups manifest as
// "InsufficientCoinBalance in command N" reverts on the bridge tx —
// silent failures that look like LiFi outages. See
// docs/incidents/2026-05-25-route-a-stuck-deposit.md.
//
// Implements the SkipSentinel interface (route_a_events.go) so the
// audit log records this as `skipped`, not `failed`.
type awaitingDepositErr struct{ msg string }

func (e *awaitingDepositErr) Error() string { return e.msg }
func (e *awaitingDepositErr) skipSentinel() {}

var ErrAwaitingDepositAtAggregator error = &awaitingDepositErr{
	msg: "rails: aggregator wallet hasn't received the deposit yet — waiting for indexer/self-settle",
}

const (
	// maxQuoteRetries is the number of consecutive LiFi quote failures
	// before an order is marked failed. Prevents infinite log spam.
	maxQuoteRetries = 10

	// uncertainRecoveryWindow is how long the late-arrival poller will
	// re-check a `bridge_uncertain` order before giving up and marking
	// it `failed`. Sized for the longest realistic bridge tail.
	uncertainRecoveryWindow = 24 * time.Hour
)

// bridgeStaleTimeouts is how long an order can stay in 'bridging' with
// LiFi /status returning "not found" before we transition it to
// `bridge_uncertain`. Per-tool because bridges have wildly different
// SLAs — Allbridge can take 30-45 min on a normal day, CCTP is sub-10.
// The unknown/default bucket is generous (45 min) so we err toward
// "wait a bit more" instead of "mark failed too early" — see
// docs/incidents/2026-05-25-route-a-stuck-deposit.md.
var bridgeStaleTimeouts = map[string]time.Duration{
	"allbridge": 60 * time.Minute,
	"wormhole":  30 * time.Minute,
	"cctp":      20 * time.Minute,
	"mayan":     15 * time.Minute,
	"stargate":  25 * time.Minute,
	"":          45 * time.Minute, // unknown tool
}

func bridgeStaleTimeoutFor(tool string) time.Duration {
	if d, ok := bridgeStaleTimeouts[strings.ToLower(tool)]; ok {
		return d
	}
	return bridgeStaleTimeouts[""]
}

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
	evm        *evm.Client
	settlement *settlement.Client

	// quoteFailCounts tracks consecutive LiFi quote failures per order
	// ID in memory. Reset on success; orders are marked failed once the
	// count exceeds maxQuoteRetries.
	quoteFailMu     sync.Mutex
	quoteFailCounts map[int]int

	// Direct-CCTP bridge fallback (services/route_a_cctp.go). Resolved
	// once at boot; cctpNetOK=false simply means the fallback never
	// engages — the LiFi path is unaffected either way.
	cctpNet   cctp.Network
	cctpNetOK bool
	cctpIris  *cctp.Iris
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

	// CCTP fallback wiring — constants resolved from the destination
	// chain id; nothing else in the dispatcher changes when this is
	// absent or disabled.
	if net, ok := cctp.ForBaseChainID(conf.BaseChainID); ok {
		d.cctpNet = net.WithIrisURL(conf.CCTPIrisURL)
		d.cctpNetOK = true
		d.cctpIris = cctp.NewIris(d.cctpNet.IrisBaseURL)
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
			logger.Errorf("❌ route-a: EVM client init failed: %v", err)
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
		logger.Errorf("❌ route-a: advance pending: %v", err)
	}
	if err := d.advanceBridging(ctx); err != nil {
		logger.Errorf("❌ route-a: advance bridging: %v", err)
	}
	if err := d.advanceUncertain(ctx); err != nil {
		logger.Errorf("❌ route-a: advance uncertain: %v", err)
	}
	if err := d.advanceBridged(ctx); err != nil {
		logger.Errorf("❌ route-a: advance bridged: %v", err)
	}
	if err := d.advanceDispatching(ctx); err != nil {
		logger.Errorf("❌ route-a: advance dispatching: %v", err)
	}
	return nil
}

// advancePending finds RouteAOrder rows in 'pending' or 'awaiting_funds' state,
// fetches a LiFi quote, submits the Sui-side bridge tx, and transitions to 'bridging'.
// Orders in 'awaiting_funds' are re-checked every tick until the aggregator wallet
// has received the self-settled deposit.
func (d *RouteADispatcher) advancePending(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusIn(
			routeaorder.BridgeStatusPending,
			routeaorder.BridgeStatusAwaitingFunds,
		)).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithToken()
		}).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		if err := d.startBridgeForProvider(ctx, order); err != nil {
			// Awaiting-funds: the deposit hasn't self-settled to the aggregator yet.
			// Persist awaiting_funds so the DB reflects reality, skip the
			// retry counter — we recheck every tick until funds arrive.
			if errors.Is(err, ErrAwaitingDepositAtAggregator) {
				if order.BridgeStatus != routeaorder.BridgeStatusAwaitingFunds {
					if _, uerr := order.Update().
						SetBridgeStatus(routeaorder.BridgeStatusAwaitingFunds).
						Save(ctx); uerr != nil {
						logger.Errorf("❌ route-a: persist awaiting_funds for %d: %v", order.ID, uerr)
					} else {
						logger.Infof("🤔 route-a: order %d → awaiting_funds (aggregator deposit not yet settled)", order.ID)
					}
				}
				continue
			}
			d.quoteFailMu.Lock()
			d.quoteFailCounts[order.ID]++
			count := d.quoteFailCounts[order.ID]
			d.quoteFailMu.Unlock()

			logger.Errorf("❌ route-a: start bridge for order %d (attempt %d/%d): %v",
				order.ID, count, maxQuoteRetries, err)

			// LiFi keeps failing — hand eligible orders to the direct-
			// CCTP fallback (services/route_a_cctp.go) before the retry
			// counter death-marches to FAILED. Ineligible or failed
			// fallback attempts fall through to the original behavior.
			// Orders already on the CCTP rail are failing in CCTP
			// itself; re-trying the same rail here would be circular.
			if count >= cctpFallbackAfter && order.BridgeProvider != routeAProviderCCTP {
				if ferr := d.tryCCTPFallback(ctx, order, count); ferr == nil {
					d.quoteFailMu.Lock()
					delete(d.quoteFailCounts, order.ID)
					d.quoteFailMu.Unlock()
					continue
				} else {
					logger.Errorf("❌ route-a: CCTP fallback for order %d: %v", order.ID, ferr)
				}
			}

			if count >= maxQuoteRetries {
				reason := fmt.Sprintf("bridge start (%s) failed %d consecutive times: %v",
					order.BridgeProvider, count, err)
				if _, uerr := order.Update().
					SetBridgeStatus(routeaorder.BridgeStatusFailed).
					SetFailureReason(reason).
					Save(ctx); uerr != nil {
					logger.Errorf("❌ route-a: persist quote-fail for %d: %v", order.ID, uerr)
				} else {
					logger.Errorf("❌ route-a: order %d marked FAILED after %d quote retries", order.ID, count)
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
//
// Audit-log instrumentation: writes started + terminal rows under
// step=bridge_submit so the operator timeline shows every attempt
// (including silent skips when the aggregator hasn't received funds
// yet — that's the case ErrAwaitingDepositAtAggregator covers, which
// satisfies SkipSentinel and is recorded as `skipped`).
func (d *RouteADispatcher) startBridge(ctx context.Context, order *ent.RouteAOrder) (err error) {
	timer := Time(ctx, order.ID, StepBridgeSubmit, ActorDispatcher)
	defer timer.End(&err)

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
	//
	// Native SUI: reserve a small amount for the aggregator's bridge-tx
	// gas. The reservation matches the one baked into the order-time
	// composite rate (services/route_a_quote.go) so the user-facing
	// quote and the actual bridge stay in sync.
	bridgeAmount := po.Amount
	if IsNativeSui(tok.ContractAddress) {
		gasReserve := decimal.NewFromInt(NativeSuiGasReservation).Shift(-int32(NativeSuiDecimals))
		bridgeAmount = po.Amount.Sub(gasReserve)
		if bridgeAmount.Sign() <= 0 {
			return fmt.Errorf("sui amount %s below gas reservation %s", po.Amount, gasReserve)
		}
	}
	fromAmount := bridgeAmount.Shift(int32(tok.Decimals)).Truncate(0).String()

	// Pre-flight: aggregator must already hold enough of the source
	// coin to satisfy this bridge. Without this guard, an indexer
	// hiccup (deposit landed at receive_address but never self-settled
	// to the aggregator) manifests downstream as a cryptic
	// "InsufficientCoinBalance in command N" revert on the LiFi PTB,
	// which then gets misreported as "LiFi tx not found" 15 minutes
	// later — and the order is marked permanently FAILED with the
	// user's funds stranded at the receive_address. See
	// docs/incidents/2026-05-25-route-a-stuck-deposit.md.
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
	need, ok := new(big.Int).SetString(fromAmount, 10)
	if !ok {
		return fmt.Errorf("parse fromAmount %q", fromAmount)
	}
	if order.BridgeStatus == routeaorder.BridgeStatusAwaitingFunds &&
		!hasSuccessfulRouteAEvent(ctx, order.ID, StepSelfSettle) {
		timer.With("aggregator_have", have.String()).
			With("need", need.String()).
			With("coin_type", tok.ContractAddress)
		return ErrAwaitingDepositAtAggregator
	}
	if have.Cmp(need) < 0 {
		haveDec := decimal.NewFromBigInt(have, 0).Shift(-int32(tok.Decimals))
		needDec := decimal.NewFromBigInt(need, 0).Shift(-int32(tok.Decimals))
		logger.Infof("🤔 route-a: order %d awaiting funds — aggregator has %s, need %s %s; will recheck next tick",
			order.ID, haveDec.String(), needDec.String(), tok.Symbol)
		timer.With("aggregator_have", have.String()).
			With("need", need.String()).
			With("coin_type", tok.ContractAddress)
		return ErrAwaitingDepositAtAggregator
	}
	timer.With("from_amount", fromAmount).
		With("coin_type", tok.ContractAddress).
		With("aggregator_have", have.String())

	qr := lifi.QuoteRequest{
		FromChain:   strconv.FormatInt(lifi.SuiChainID, 10),
		ToChain:     strconv.FormatInt(d.conf.BaseChainID, 10),
		FromToken:   tok.ContractAddress,
		ToToken:     d.conf.BaseUSDCContract,
		FromAmount:  fromAmount,
		FromAddress: d.signer.Address,
		ToAddress:   d.conf.BaseAggregatorAddress,
	}
	if IsNativeSui(tok.ContractAddress) {
		qr.Slippage = NativeSuiSlippage()
	}
	quote, err := d.lifi.GetQuote(ctx, qr)
	if err != nil {
		logger.Errorf("❌ route-a: quote request details order=%d fromChain=%s toChain=%s fromToken=%s toToken=%s fromAmount=%s fromAddr=%s toAddr=%s",
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
	timer.With("lifi_quote_id", quote.ID).
		With("lifi_tool", quote.Tool).
		With("bridge_tx_sui", resp.Digest).
		With("estimated_to_amount", quote.Estimate.ToAmount).
		With("estimated_to_amount_min", quote.Estimate.ToAmountMin)
	logger.Infof("✅ route-a: bridge initiated order=%d tool=%s tx=%s", order.ID, quote.Tool, resp.Digest)
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
		if order.BridgeProvider == routeAProviderCCTP {
			d.pollOneCCTP(ctx, order)
			continue
		}
		d.pollOneBridge(ctx, order)
	}
	return nil
}

// pollOneBridge handles one /status poll for a single bridging order.
// Outcomes:
//   - LiFi DONE      → advance to `bridged` (with dest tx + amount)
//   - LiFi FAILED    → terminal `failed`
//   - "not found" within per-tool timeout → keep polling next tick
//   - "not found" past per-tool timeout   → transition to
//     `bridge_uncertain`; the late-arrival poller takes over
//   - other transport error                → log + retry next tick
//
// Every poll writes a `bridge_poll` event so the timeline shows
// exactly how long each upstream call took and what status came back.
func (d *RouteADispatcher) pollOneBridge(ctx context.Context, order *ent.RouteAOrder) {
	var err error
	timer := Time(ctx, order.ID, StepBridgePoll, ActorDispatcher).
		With("bridge_tx_sui", order.BridgeTxSui).
		With("lifi_tool", order.LifiTool)
	defer timer.End(&err)

	status, lerr := d.lifi.GetStatus(ctx, lifi.StatusRequest{
		TxHash:    order.BridgeTxSui,
		Bridge:    order.LifiTool,
		FromChain: strconv.FormatInt(lifi.SuiChainID, 10),
		ToChain:   strconv.FormatInt(d.conf.BaseChainID, 10),
	})
	if lerr != nil {
		logger.Errorf("❌ route-a: status poll for %d (tx=%s tool=%s): %v", order.ID, order.BridgeTxSui, order.LifiTool, lerr)
		timer.With("upstream_error", lerr.Error())

		// "not found" past the per-tool stale window → bridge_uncertain.
		// We do NOT mark failed here anymore — the late-arrival poller
		// may yet observe the bridged USDC at the Base aggregator. See
		// 2026-05-25 incident.
		staleAfter := bridgeStaleTimeoutFor(order.LifiTool)
		if strings.Contains(lerr.Error(), "not found") && time.Since(order.UpdatedAt) > staleAfter {
			reason := fmt.Sprintf("LiFi /status 'not found' for tx %s after %s — transitioned to bridge_uncertain",
				order.BridgeTxSui, staleAfter)
			if _, uerr := order.Update().
				SetBridgeStatus(routeaorder.BridgeStatusBridgeUncertain).
				SetFailureReason(reason).
				Save(ctx); uerr != nil {
				logger.Errorf("❌ route-a: persist bridge_uncertain for %d: %v", order.ID, uerr)
				err = uerr
				return
			}
			logger.Infof("🤔 route-a: order %d → bridge_uncertain (tool=%s stale_after=%s)",
				order.ID, order.LifiTool, staleAfter)
			LogOnce(ctx, order.ID, StepBridgeUncertain, StatusStarted, ActorDispatcher,
				map[string]any{
					"reason":      reason,
					"stale_after": staleAfter.String(),
					"lifi_tool":   order.LifiTool,
					"bridge_tx":   order.BridgeTxSui,
				}, "", "")
			timer.With("transitioned", "bridge_uncertain").
				With("stale_after", staleAfter.String())
			return
		}
		err = lerr
		return
	}

	timer.With("lifi_status", status.Status).With("lifi_substatus", status.Substatus)

	switch status.Status {
	case "DONE":
		update := order.Update().SetBridgeStatus(routeaorder.BridgeStatusBridged)
		if status.Receiving != nil {
			if status.Receiving.TxHash != "" {
				update = update.SetBridgeTxDest(status.Receiving.TxHash)
				timer.With("bridge_tx_dest", status.Receiving.TxHash)
			}
			if status.Receiving.Amount != "" {
				if amt, ok := parseAmountToDecimal(status.Receiving.Amount, status.Receiving.Token.Decimals); ok {
					update = update.SetBridgedAmount(amt)
					timer.With("bridged_amount", amt.String())
				}
			}
		}
		if _, uerr := update.Save(ctx); uerr != nil {
			logger.Errorf("❌ route-a: persist DONE for %d: %v", order.ID, uerr)
			err = uerr
			return
		}
		logger.Infof("✅ route-a: bridge DONE order=%d", order.ID)
		// Distinct `bridge_done` row so the timeline shows the
		// completion moment cleanly (the poll row is just the call).
		LogOnce(ctx, order.ID, StepBridgeDone, StatusSucceeded, ActorDispatcher,
			map[string]any{
				"bridge_tx_dest": order.BridgeTxDest,
			}, "", "")

	case "FAILED":
		reason := status.SubstatusMsg
		if reason == "" {
			reason = status.Substatus
		}
		if _, uerr := order.Update().
			SetBridgeStatus(routeaorder.BridgeStatusFailed).
			SetFailureReason(reason).
			Save(ctx); uerr != nil {
			logger.Errorf("❌ route-a: persist FAILED for %d: %v", order.ID, uerr)
			err = uerr
			return
		}
		logger.Infof("❌ route-a: bridge FAILED order=%d reason=%s", order.ID, reason)
		err = fmt.Errorf("lifi FAILED: %s", reason)

	default:
		// PENDING / NOT_FOUND inside the stale window — keep polling.
		// `bridge_poll / succeeded` row records the call.
	}
}

// advanceUncertain handles `bridge_uncertain` orders — those whose
// LiFi /status has been "not found" past the per-tool stale timeout.
// Two independent recovery paths run in parallel each tick:
//
//  1. Re-query LiFi /status. Sometimes their indexer just catches up
//     late (especially under load) and reports DONE/FAILED after we
//     already gave up. If we get a terminal status, advance the order.
//  2. Read the Base aggregator wallet's USDC balance delta. If a new
//     USDC transfer arrived to the wallet that matches the expected
//     bridge amount within tolerance, the bridge has clearly completed
//     even if LiFi never indexed it. Mark `bridged` and move on.
//
// After uncertainRecoveryWindow (24h) elapses with no recovery, the
// order is finally marked `failed` so refund logic can pick it up.
//
// Triggered from Tick() every minute. Every order touched writes a
// `bridge_uncertain` event so the timeline shows recovery attempts.
func (d *RouteADispatcher) advanceUncertain(ctx context.Context) error {
	orders, err := db.Client.RouteAOrder.
		Query().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridgeUncertain)).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		if order.BridgeProvider == routeAProviderCCTP {
			d.recoverUncertainCCTP(ctx, order)
			continue
		}

		var loopErr error
		timer := Time(ctx, order.ID, StepBridgeUncertain, ActorDispatcher).
			With("bridge_tx_sui", order.BridgeTxSui).
			With("lifi_tool", order.LifiTool).
			With("age", time.Since(order.UpdatedAt).String())

		// Path 1: re-query LiFi.
		status, lerr := d.lifi.GetStatus(ctx, lifi.StatusRequest{
			TxHash:    order.BridgeTxSui,
			Bridge:    order.LifiTool,
			FromChain: strconv.FormatInt(lifi.SuiChainID, 10),
			ToChain:   strconv.FormatInt(d.conf.BaseChainID, 10),
		})
		if lerr == nil {
			timer.With("lifi_status", status.Status)
			switch status.Status {
			case "DONE":
				if d.markBridgedFromStatus(ctx, order, status) {
					timer.With("recovered_via", "lifi_late_done")
					timer.End(&loopErr)
					continue
				}
			case "FAILED":
				reason := status.SubstatusMsg
				if reason == "" {
					reason = status.Substatus
				}
				if _, uerr := order.Update().
					SetBridgeStatus(routeaorder.BridgeStatusFailed).
					SetFailureReason("late LiFi FAILED: " + reason).
					Save(ctx); uerr != nil {
					logger.Errorf("❌ route-a: persist late FAILED for %d: %v", order.ID, uerr)
				}
				timer.With("recovered_via", "lifi_late_failed")
				timer.End(&loopErr)
				continue
			}
		} else {
			timer.With("lifi_error", lerr.Error())
		}

		// Path 2: check destination wallet for incoming USDC the
		// indexer might not have surfaced. Implementation deferred —
		// requires the EVM client (lazy-init below) and a fairly
		// careful matching heuristic (look at recent transfers, match
		// by amount within tolerance, dedupe across orders). Tracked
		// in docs/route-a-hardening.md Phase 2 follow-ups.
		// For now we just keep retrying LiFi.

		// Window expired → mark failed so the refund flow can run.
		if time.Since(order.UpdatedAt) > uncertainRecoveryWindow {
			reason := fmt.Sprintf("uncertain past %s window; LiFi still not found", uncertainRecoveryWindow)
			if _, uerr := order.Update().
				SetBridgeStatus(routeaorder.BridgeStatusFailed).
				SetFailureReason(reason).
				Save(ctx); uerr != nil {
				logger.Errorf("❌ route-a: persist window-expired FAILED for %d: %v", order.ID, uerr)
			}
			timer.With("recovered_via", "window_expired_to_failed")
		}
		timer.End(&loopErr)
	}
	return nil
}

// markBridgedFromStatus applies a LiFi DONE response to an order
// row, advancing it to `bridged`. Returns true on success. Used by
// both the normal `bridging` poller (pollOneBridge) and the late-
// arrival path (advanceUncertain).
func (d *RouteADispatcher) markBridgedFromStatus(
	ctx context.Context, order *ent.RouteAOrder, status *lifi.StatusResponse,
) bool {
	update := order.Update().
		SetBridgeStatus(routeaorder.BridgeStatusBridged).
		SetFailureReason("") // clear the uncertain marker
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
		logger.Errorf("❌ route-a: persist late DONE for %d: %v", order.ID, err)
		return false
	}
	LogOnce(ctx, order.ID, StepBridgeDone, StatusSucceeded, ActorDispatcher,
		map[string]any{
			"recovered_via":  "late_lifi_done",
			"bridge_tx_dest": order.BridgeTxDest,
		}, "", "")
	return true
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
		// dispatchLP needs the PaymentOrder + its Recipient (for the
		// settlement messageHash) and Token (to branch the rate
		// conversion for native-SUI orders); eager-load them here so
		// the dispatcher doesn't have to re-query per order.
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithRecipient()
			poq.WithToken()
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
			logger.Errorf("❌ route-a: dispatch order=%d mode=%s: %v", order.ID, order.Mode, dispatchErr)
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
func (d *RouteADispatcher) dispatchLP(ctx context.Context, order *ent.RouteAOrder) (err error) {
	timer := Time(ctx, order.ID, StepEvmCreateOrder, ActorDispatcher).
		With("base_chain_id", d.conf.BaseChainID)
	defer timer.End(&err)

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

	// Fetch fresh rate quote from the aggregator to ensure we don't submit an expired/stale rate
	// that LPs will reject.
	liveQuote, err := d.settlement.FetchRate(ctx, "base", "USDC", *order.BridgedAmount, "NGN")
	if err != nil {
		return fmt.Errorf("fetch live the aggregator rate: %w", err)
	}

	// The merchant expects to receive exactly the quoted fiat amount: po.Amount * po.Rate
	targetNGN := po.Amount.Mul(po.Rate)

	// Calculate the USDC amount needed to pay the targetNGN at the fresh live rate.
	amountDec := targetNGN.Div(liveQuote.Rate)
	amount := decimalToSubunit(amountDec, d.conf.BaseUSDCDecimals)

	// Sender fee (our platform profit) is 0.5% (BaseSenderFeeBPS) of the payout amount.
	senderFee := new(big.Int).Quo(
		new(big.Int).Mul(amount, big.NewInt(d.conf.BaseSenderFeeBPS)),
		big.NewInt(10_000),
	)
	total := new(big.Int).Add(amount, senderFee)
	senderFeeDec := decimal.NewFromBigInt(senderFee, -int32(d.conf.BaseUSDCDecimals))

	// Ensure the bridged amount is sufficient to cover the payout + sender fee.
	// If the rate has moved against us or there was high slippage, total may exceed bridged.
	if total.Cmp(bridged) > 0 {
		return fmt.Errorf("insufficient bridged USDC: have %s, need %s (live rate %s)", bridged.String(), total.String(), liveQuote.Rate.String())
	}

	logger.Infof("📈 route-a: fetched fresh the aggregator rate for order %d: NGN/USDC=%s (original NGN/USDC was %s). Payout adjusted: amount=%s USDC, senderFee=%s USDC, bridged=%s USDC",
		order.ID, liveQuote.Rate.String(), po.Rate.String(), amountDec.String(), senderFeeDec.String(), order.BridgedAmount.String())

	rateScaled := liveQuote.Rate.Mul(decimal.NewFromInt(100)).Truncate(0)
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
	timer.With("amount_subunit", amount.String()).
		With("sender_fee_subunit", senderFee.String()).
		With("rate_scaled", rate.String())

	approveErr := func() (aerr error) {
		atimer := Time(ctx, order.ID, StepEvmApprove, ActorDispatcher).
			With("approve_total", total.String()).
			With("spender", d.evm.Config().GatewayAddr.Hex())
		defer atimer.End(&aerr)
		_, aerr = d.evm.USDC().Approve(ctx, d.evm.Config().GatewayAddr, total)
		return
	}()
	if approveErr != nil {
		return fmt.Errorf("approve USDC: %w", approveErr)
	}

	// Submit createOrder + wait for receipt + parse orderId.
	result, cerr := d.evm.Gateway().CreateOrder(ctx, evm.CreateOrderParams{
		Token:              d.evm.Config().USDCAddr,
		Amount:             amount,
		Rate:               rate,
		SenderFeeRecipient: aggregatorAddr, // we collect our own skim
		SenderFee:          senderFee,
		RefundAddress:      aggregatorAddr, // refunds bounce back to us; ops handles reverse-bridge
		MessageHash:        messageHash,
	})
	if cerr != nil {
		return fmt.Errorf("createOrder: %w", cerr)
	}

	orderID := strings.ToLower(result.OrderID.Hex())
	if _, perr := order.Update().
		SetGatewayOrderID(orderID).
		SetGatewayChainID(uint64(d.conf.BaseChainID)).
		SetSenderFeeSubunit(senderFeeDec).
		SetBridgeTxDest(result.TxHash.Hex()).
		SetBridgeStatus(routeaorder.BridgeStatusDispatching).
		Save(ctx); perr != nil {
		return fmt.Errorf("persist dispatching: %w", perr)
	}
	logger.Infof("✅ route-a: createOrder submitted order=%d orderId=%s tx=%s gas=%d",
		order.ID, orderID, result.TxHash.Hex(), result.GasUsed)
	timer.With("gateway_order_id", orderID).
		With("evm_tx_hash", result.TxHash.Hex()).
		With("gas_used", result.GasUsed)
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
			logger.Errorf("❌ route-a: settlement status %d: %v", order.ID, err)
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
					logger.Errorf("❌ route-a: payment_order → settled (%d): %v", order.ID, perr)
				}
			}
		case settlement.StatusRefunded, settlement.StatusExpired:
			// Query the number of times we've tried evm_create_order
			attempts, qerr := order.QueryEvents().
				Where(
					routeaevent.StepEQ(routeaevent.StepEvmCreateOrder),
					routeaevent.StatusEQ(routeaevent.StatusStarted),
				).
				Count(ctx)
			if qerr != nil {
				logger.Errorf("❌ route-a: query attempts count (%d): %v", order.ID, qerr)
				attempts = 1 // default fallback
			}

			if attempts < 3 {
				// Retry by moving the status back to bridged so dispatchLP runs on next tick.
				// We also clear the gateway_order_id so we do not query the old expired one again.
				upd = upd.SetBridgeStatus(routeaorder.BridgeStatusBridged).
					ClearSettlementStatus().
					ClearGatewayOrderID()

				LogOnce(ctx, order.ID, StepSettlementTerminal, StatusRetrying, ActorDispatcher, map[string]any{
					"reason":         string(info.Status),
					"attempt":        attempts,
					"old_gateway_id": order.GatewayOrderID,
				}, fmt.Sprintf("Order was %s. Retrying with a new rate quote (attempt %d).", info.Status, attempts+1), "")

				logger.Infof("🔄 route-a: order %d was %s. Resetting state to bridged to retry (attempt %d/3)", order.ID, info.Status, attempts+1)
			} else {
				// Retry limit reached, mark as refunded
				upd = upd.SetBridgeStatus(routeaorder.BridgeStatusRefunded)
				bridgeFlipped = routeaorder.BridgeStatusRefunded
				poFlipped = paymentorder.StatusRefunded
				if order.Edges.PaymentOrder != nil {
					if _, perr := order.Edges.PaymentOrder.Update().
						SetStatus(paymentorder.StatusRefunded).
						Save(ctx); perr != nil {
						logger.Errorf("❌ route-a: payment_order → refunded (%d): %v", order.ID, perr)
					}
				}
				logger.Warnf("❌ route-a: order %d reached maximum retry attempts (%d). Marking refunded.", order.ID, attempts)
			}
		}
		if _, err := upd.Save(ctx); err != nil {
			logger.Errorf("❌ route-a: persist dispatching → %s (%d): %v", info.Status, order.ID, err)
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
		"order_id":          order.Edges.PaymentOrder.ID.String(),
		"route_a_order_id":  order.ID,
		"bridge_status":     string(order.BridgeStatus),
		"settlement_status": string(pcStatus),
		"gateway_order_id":  order.GatewayOrderID,
		"dest_tx_hash":      order.BridgeTxDest,
		"chain_id":          order.GatewayChainID,
		"updated_at":        time.Now().UTC().Format(time.RFC3339),
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
		balDec := decimal.NewFromBigInt(bal, 0).Shift(-18)
		thresholdDec := decimal.NewFromBigInt(threshold, 0).Shift(-18)
		logger.Errorf(
			"❌ route-a: Base aggregator wallet LOW on native (ETH) — balance=%s ETH, threshold=%s ETH, address=%s. Top up before dispatches stall.",
			balDec.String(), thresholdDec.String(), d.evm.From().Hex(),
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
