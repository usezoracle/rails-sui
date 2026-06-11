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
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	suiconst "github.com/block-vision/sui-go-sdk/constant"
	suimodels "github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/baas/fintava"
	"github.com/usezoracle/rails-sui/services/baas/korapay"
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

	// maxDispatchShortfallBPS caps how much quote→dispatch rate drift
	// dispatchLP will absorb from the ops float instead of stranding an
	// order in `bridged`. 200 bps covers normal NGN/USDC movement over
	// the minutes an order is in flight; anything bigger is a quoting
	// bug that should surface, not be papered over.
	maxDispatchShortfallBPS = 200

	// stuckClaimAlertAfter is how long an order may sit claimed
	// (bridging without a digest / dispatching without a gateway id)
	// before the sweep starts shouting. Normal claims resolve within
	// seconds; anything past this is a crash or ambiguous submit that
	// needs a human + an explorer.
	stuckClaimAlertAfter = 10 * time.Minute

	// minLifiPollGap throttles LiFi /status polls per order. Burst
	// mode ticks every 3s, which is friendlier than LiFi's rate
	// limits appreciate; Circle (CCTP) has no such sensitivity.
	minLifiPollGap = 10 * time.Second
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

// claimForBridging CAS-claims a pending/awaiting_funds order into
// `bridging` (no tx digest yet) BEFORE the funds-moving submit. The
// claim is the double-submit lock: even two writers racing the same
// order resolve to exactly one winner, because only one UPDATE can
// match the from-state. Returns false when another writer already
// claimed it.
func (d *RouteADispatcher) claimForBridging(ctx context.Context, order *ent.RouteAOrder, provider, tool string) (bool, error) {
	_, err := order.Update().
		Where(routeaorder.BridgeStatusIn(
			routeaorder.BridgeStatusPending,
			routeaorder.BridgeStatusAwaitingFunds,
		)).
		SetBridgeProvider(provider).
		SetLifiTool(tool).
		SetBridgeStatus(routeaorder.BridgeStatusBridging).
		Save(ctx)
	if err != nil {
		if isStaleTransition(err) {
			logStaleTransition(order.ID, "bridging (claim)")
			return false, nil
		}
		return false, fmt.Errorf("claim for bridging: %w", err)
	}
	return true, nil
}

// revertBridgingClaim returns a claimed order to `pending` after a
// submit failure that is KNOWN to have left no tx on-chain. The
// BridgeTxSui-is-empty guard makes it impossible to revert an order
// whose tx hash was already persisted.
func (d *RouteADispatcher) revertBridgingClaim(ctx context.Context, orderID int) {
	if _, err := db.Client.RouteAOrder.Update().
		Where(
			routeaorder.IDEQ(orderID),
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridging),
			routeaorder.Or(routeaorder.BridgeTxSuiIsNil(), routeaorder.BridgeTxSuiEQ("")),
		).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		Save(ctx); err != nil {
		logger.Errorf("❌ route-a: revert bridging claim for %d: %v", orderID, err)
	}
}

// isStaleTransition reports whether a guarded status update didn't
// apply because the row had already moved to a different state (the
// UpdateOne.Where precondition failed → ent NotFound). Every
// bridge_status write carries its legal from-states so a writer
// holding a stale view skips instead of clobbering — see the
// 2026-06-11 incident where `bridging` was overwritten by
// `awaiting_funds` four seconds after a CCTP burn went on-chain.
func isStaleTransition(err error) bool { return ent.IsNotFound(err) }

// logStaleTransition standardizes the skip log so these show up
// grep-ably in observability.
func logStaleTransition(orderID int, to string) {
	logger.Warnf("⚠️ route-a: order %d: skipped stale transition to %s (row already moved)", orderID, to)
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

	// instanceID identifies this process in the dispatcher lease —
	// exactly one dispatcher may tick per database (see acquireLease;
	// 2026-06-11 incident: a stale local dev server raced the Railway
	// dispatcher and clobbered an in-flight order's state).
	instanceID string

	// tickMu makes ticks non-overlapping within this process: at a
	// 10-second cadence a slow tick (waitMined on Base can take
	// longer) would otherwise stack concurrent runs that double-
	// process the same orders before any CAS guard can apply.
	tickMu sync.Mutex

	// Burst mode: when an order makes progress (tap debited, burn
	// landed, mint done, dispatch submitted) the dispatcher ticks
	// every burstTickInterval until burstUntil, so the pipeline
	// chains hop-to-hop without waiting for the cron boundary. Every
	// successful advance extends the window, so a single in-flight
	// order is chased continuously from tap to settled; the 10s cron
	// remains the crash-recovery backstop.
	burstMu    sync.Mutex
	burstUntil time.Time
	burstWake  chan struct{}

	// lifiPollAt rate-limits LiFi /status calls per order (see
	// minLifiPollGap) — burst ticks are faster than LiFi likes.
	lifiPollMu sync.Mutex
	lifiPollAt map[int]time.Time

	// treasuryRail is the BaaS provider Route C pays merchants from —
	// Korapay (built from KORAPAY_* config) when configured, else
	// whatever baas.Default() is. Injected directly so the float rail
	// is independent of the BAAS_PROVIDER selection; tests substitute
	// a fake.
	treasuryRail baas.Provider

	// koraVBA resolves the platform's float virtual account from
	// Korapay (the source of truth — no env, no DB). Cached below.
	koraVBA       *korapay.Client
	floatAcctMu   sync.Mutex
	floatAcct     *floatAccount
	floatAcctAt   time.Time
	lastFloatWarn time.Time
}

// activeRouteADispatcher lets API handlers (which never construct the
// dispatcher) nudge the worker's instance. Nil in API-only processes
// (DISABLE_BACKGROUND_JOBS) — KickRouteA degrades to a no-op there and
// the lease-holding worker's cron picks the order up within 10s.
var activeRouteADispatcher atomic.Pointer[RouteADispatcher]

const (
	burstWindow       = 90 * time.Second
	burstTickInterval = 3 * time.Second
)

// KickRouteA asks the in-process dispatcher (if any) to start/extend
// burst ticking. Called after events that create dispatcher work:
// card debits, deposit self-settles.
func KickRouteA() {
	if d := activeRouteADispatcher.Load(); d != nil {
		d.ExtendBurst()
	}
}

// ExtendBurst (re)opens the burst window and wakes the burst loop.
func (d *RouteADispatcher) ExtendBurst() {
	d.burstMu.Lock()
	d.burstUntil = time.Now().Add(burstWindow)
	d.burstMu.Unlock()
	select {
	case d.burstWake <- struct{}{}:
	default: // loop already awake
	}
}

// runBurstLoop ticks every burstTickInterval while the burst window is
// open. Tick itself is lease-guarded, CAS-guarded, and non-overlapping,
// so burst ticks compose safely with the cron's.
func (d *RouteADispatcher) runBurstLoop() {
	for range d.burstWake {
		for {
			d.burstMu.Lock()
			until := d.burstUntil
			d.burstMu.Unlock()
			if time.Now().After(until) {
				break
			}
			if err := d.Tick(context.Background()); err != nil {
				logger.Errorf("❌ route-a: burst tick: %v", err)
			}
			time.Sleep(burstTickInterval)
		}
	}
}

// Dispatcher singleton lease. Whoever holds the Redis key ticks;
// everyone else skips. TTL > tick interval so the leader renews each
// tick; if the leader dies, another instance takes over within 90s.
const (
	dispatcherLeaseKey = "rails:route_a:dispatcher_lease"
	dispatcherLeaseTTL = 90 * time.Second
)

// acquireLease reports whether this instance may run the tick. Fails
// OPEN when Redis is unavailable — a single-instance deployment must
// not halt payments because Redis blipped; the lease protects against
// the multi-instance case, which always implies working shared infra.
func (d *RouteADispatcher) acquireLease(ctx context.Context) bool {
	rdb := db.RedisClient
	if rdb == nil {
		return true
	}
	ok, err := rdb.SetNX(ctx, dispatcherLeaseKey, d.instanceID, dispatcherLeaseTTL).Result()
	if err != nil {
		logger.Errorf("❌ route-a: dispatcher lease check failed (proceeding open): %v", err)
		return true
	}
	if ok {
		return true
	}
	holder, err := rdb.Get(ctx, dispatcherLeaseKey).Result()
	if err == nil && holder == d.instanceID {
		rdb.Expire(ctx, dispatcherLeaseKey, dispatcherLeaseTTL)
		return true
	}
	return false
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
		instanceID:      uuid.NewString(),
		burstWake:       make(chan struct{}, 1),
		lifiPollAt:      make(map[int]time.Time),
	}
	go d.runBurstLoop()
	activeRouteADispatcher.Store(d)

	// CCTP fallback wiring — constants resolved from the destination
	// chain id; nothing else in the dispatcher changes when this is
	// absent or disabled.
	if net, ok := cctp.ForBaseChainID(conf.BaseChainID); ok {
		d.cctpNet = net.WithIrisURL(conf.CCTPIrisURL)
		d.cctpNetOK = true
		d.cctpIris = cctp.NewIris(d.cctpNet.IrisBaseURL)
	}

	// Route B/C float rail, selected by FLOAT_RAIL: "korapay"
	// (default), "fintava", or "default" (= whatever BAAS_PROVIDER
	// registered). The Korapay client is kept alongside regardless —
	// it resolves the Route C reload destination (the platform VBA),
	// which lives on Korapay independent of which rail pays out.
	bc := config.BaaSConfig()
	if bc.KorapaySecretKey != "" {
		kc := korapay.New(
			bc.KorapaySecretKey, bc.KorapayPublicKey, bc.KorapayBaseURL,
			bc.KorapayPayoutEmail, bc.KorapayVBABankCode,
		)
		d.koraVBA = kc
		if strings.EqualFold(bc.FloatRail, "korapay") || bc.FloatRail == "" {
			d.treasuryRail = korapay.NewAdapter(kc)
		}
	}
	if strings.EqualFold(bc.FloatRail, "fintava") && bc.FintavaAPIKey != "" {
		d.treasuryRail = fintava.NewAdapter(fintava.New(
			bc.FintavaAPIKey, bc.FintavaWebhookSecret, bc.FintavaBaseURL,
		))
	}
	// FloatRail "default" (or an unconfigured selection) leaves
	// treasuryRail nil → floatRail() falls back to baas.Default().

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
	// Previous tick still running → skip; the next 10s tick catches up.
	if !d.tickMu.TryLock() {
		return nil
	}
	defer d.tickMu.Unlock()
	// A panic anywhere in a tick must never take down the process —
	// this loop runs ~20×/min in burst mode and shares the process
	// with the API. Orders are crash-safe by design (CAS + claim-first
	// + reconciliation), so swallowing one tick is always recoverable.
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("❌ route-a: tick PANIC recovered: %v\n%s", r, debug.Stack())
		}
	}()
	if !d.acquireLease(ctx) {
		logger.Infof("🤝 route-a: another dispatcher instance holds the lease — skipping tick")
		return nil
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
	// Route C phases (services/route_a_treasury.go): confirm in-flight
	// float payouts, then run the off-critical-path float reload.
	if err := d.advanceTreasuryPayouts(ctx); err != nil {
		logger.Errorf("❌ route-a: advance treasury payouts: %v", err)
	}
	if err := d.advanceFloatReload(ctx); err != nil {
		logger.Errorf("❌ route-a: advance float reload: %v", err)
	}
	// Route B phases (services/route_a_lp_network.go): match + claim
	// new fills, then confirm payouts / deliver USDC / settle.
	if err := d.advanceLpNetwork(ctx); err != nil {
		logger.Errorf("❌ route-a: advance lp network: %v", err)
	}
	if err := d.advanceLpNetworkPayouts(ctx); err != nil {
		logger.Errorf("❌ route-a: advance lp network payouts: %v", err)
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
		Where(
			routeaorder.BridgeStatusIn(
				routeaorder.BridgeStatusPending,
				routeaorder.BridgeStatusAwaitingFunds,
			),
			// Route B orders never bridge — advanceLpNetwork owns them.
			routeaorder.ModeNEQ(routeaorder.ModeLpNetwork),
		).
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
						Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusPending)).
						SetBridgeStatus(routeaorder.BridgeStatusAwaitingFunds).
						Save(ctx); uerr != nil {
						if isStaleTransition(uerr) {
							logStaleTransition(order.ID, "awaiting_funds")
						} else {
							logger.Errorf("❌ route-a: persist awaiting_funds for %d: %v", order.ID, uerr)
						}
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

			// The primary rail keeps failing — try the other one
			// (services/route_a_cctp.go) before the retry counter
			// death-marches to FAILED. CCTP-primary orders fall back
			// to LiFi behind a fit-guard; LiFi-primary orders fall
			// back to CCTP behind its coin-type eligibility check.
			// Ineligible or failed fallback attempts fall through to
			// the original retry-then-fail behavior.
			if count >= bridgeFallbackAfter {
				var ferr error
				if order.BridgeProvider == routeAProviderCCTP || d.cctpPrimaryEligible(order) {
					ferr = d.tryLiFiFallback(ctx, order, count)
				} else {
					ferr = d.tryCCTPFallback(ctx, order, count)
				}
				if ferr == nil {
					d.quoteFailMu.Lock()
					delete(d.quoteFailCounts, order.ID)
					d.quoteFailMu.Unlock()
					continue
				}
				logger.Errorf("❌ route-a: bridge fallback for order %d: %v", order.ID, ferr)
			}

			if count >= maxQuoteRetries {
				reason := fmt.Sprintf("bridge start (%s) failed %d consecutive times: %v",
					order.BridgeProvider, count, err)
				if _, uerr := order.Update().
					Where(routeaorder.BridgeStatusIn(
						routeaorder.BridgeStatusPending,
						routeaorder.BridgeStatusAwaitingFunds,
					)).
					SetBridgeStatus(routeaorder.BridgeStatusFailed).
					SetFailureReason(reason).
					Save(ctx); uerr != nil {
					if isStaleTransition(uerr) {
						logStaleTransition(order.ID, "failed")
					} else {
						logger.Errorf("❌ route-a: persist quote-fail for %d: %v", order.ID, uerr)
					}
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
	timer := TimeSampled(ctx, order.ID, StepBridgeSubmit, ActorDispatcher)
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

	// CLAIM before the funds-moving submit: bridging with no digest.
	// One CAS winner per order — racing writers and crash-replays can
	// never double-submit this bridge.
	claimed, err := d.claimForBridging(ctx, order, routeAProviderLiFi, quote.Tool)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
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
		d.revertBridgingClaim(ctx, order.ID) // nothing submitted — safe
		return fmt.Errorf("sign bridge tx: %w", err)
	}
	resp, err := d.suiClient.SuiExecuteTransactionBlock(ctx, suimodels.SuiExecuteTransactionBlockRequest{
		TxBytes:     quote.TransactionRequest.Data,
		Signature:   []string{signed.Signature},
		Options:     suimodels.SuiTransactionBlockOptions{ShowEffects: true},
		RequestType: "WaitForLocalExecution",
	})
	if err != nil || resp.Digest == "" {
		// Ambiguous — the signed tx may still land. Leave the order
		// claimed (bridging, no digest): re-submitting would risk a
		// double bridge; advanceBridging surfaces stuck claims loudly.
		logger.Errorf("❌ route-a: order %d LiFi bridge submit ambiguous (parked in bridging, no digest): err=%v digest=%q",
			order.ID, err, resp.Digest)
		return fmt.Errorf("submit bridge tx (ambiguous; order parked): %w", err)
	}

	if _, err := order.Update().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridging)).
		SetLifiQuoteID(quote.ID).
		SetBridgeTxSui(resp.Digest).
		Save(ctx); err != nil {
		logger.Errorf("❌ route-a: order %d bridged via LiFi (tx=%s) but digest persist failed: %v — sweep will surface",
			order.ID, resp.Digest, err)
		return fmt.Errorf("persist bridge digest: %w", err)
	}
	timer.With("lifi_quote_id", quote.ID).
		With("lifi_tool", quote.Tool).
		With("bridge_tx_sui", resp.Digest).
		With("estimated_to_amount", quote.Estimate.ToAmount).
		With("estimated_to_amount_min", quote.Estimate.ToAmountMin)
	logger.Infof("✅ route-a: bridge initiated order=%d tool=%s tx=%s", order.ID, quote.Tool, resp.Digest)
	d.ExtendBurst() // chase this order through polling without tick waits
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
			// Claimed but no digest persisted — either a submit is in
			// flight right now, or a crash/ambiguous submit parked it.
			// Old claims need a human (the tx may or may not exist
			// on-chain; only Suiscan can say).
			if time.Since(order.UpdatedAt) > stuckClaimAlertAfter {
				logger.Errorf("🚨 route-a: order %d stuck in bridging with NO digest for %s — "+
					"verify the aggregator's recent Sui txs and either set bridge_tx_sui or revert to pending",
					order.ID, time.Since(order.UpdatedAt).Round(time.Minute))
			}
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
	// Burst ticks run every 3s; don't hammer LiFi's /status faster
	// than minLifiPollGap per order.
	d.lifiPollMu.Lock()
	if last, ok := d.lifiPollAt[order.ID]; ok && time.Since(last) < minLifiPollGap {
		d.lifiPollMu.Unlock()
		return
	}
	d.lifiPollAt[order.ID] = time.Now()
	d.lifiPollMu.Unlock()

	var err error
	timer := TimePollSampled(ctx, order.ID, StepBridgePoll, ActorDispatcher).
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
				Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridging)).
				SetBridgeStatus(routeaorder.BridgeStatusBridgeUncertain).
				SetFailureReason(reason).
				Save(ctx); uerr != nil {
				if isStaleTransition(uerr) {
					logStaleTransition(order.ID, "bridge_uncertain")
					return
				}
				logger.Errorf("❌ route-a: persist bridge_uncertain for %d: %v", order.ID, uerr)
				err = uerr
				return
			}
			timer.Milestone()
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
		timer.Milestone()
		update := order.Update().
			Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridging)).
			SetBridgeStatus(routeaorder.BridgeStatusBridged)
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
			if isStaleTransition(uerr) {
				logStaleTransition(order.ID, "bridged")
				return
			}
			logger.Errorf("❌ route-a: persist DONE for %d: %v", order.ID, uerr)
			err = uerr
			return
		}
		logger.Infof("✅ route-a: bridge DONE order=%d", order.ID)
		d.ExtendBurst() // dispatch on the next burst tick, not the next cron
		// Distinct `bridge_done` row so the timeline shows the
		// completion moment cleanly (the poll row is just the call).
		LogOnce(ctx, order.ID, StepBridgeDone, StatusSucceeded, ActorDispatcher,
			map[string]any{
				"bridge_tx_dest": order.BridgeTxDest,
			}, "", "")

	case "FAILED":
		timer.Milestone()
		reason := status.SubstatusMsg
		if reason == "" {
			reason = status.Substatus
		}
		if _, uerr := order.Update().
			Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridging)).
			SetBridgeStatus(routeaorder.BridgeStatusFailed).
			SetFailureReason(reason).
			Save(ctx); uerr != nil {
			if isStaleTransition(uerr) {
				logStaleTransition(order.ID, "failed")
				return
			}
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
		timer := TimePollSampled(ctx, order.ID, StepBridgeUncertain, ActorDispatcher).
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
					timer.Milestone().With("recovered_via", "lifi_late_done")
					timer.End(&loopErr)
					continue
				}
			case "FAILED":
				reason := status.SubstatusMsg
				if reason == "" {
					reason = status.Substatus
				}
				if _, uerr := order.Update().
					Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridgeUncertain)).
					SetBridgeStatus(routeaorder.BridgeStatusFailed).
					SetFailureReason("late LiFi FAILED: " + reason).
					Save(ctx); uerr != nil {
					if isStaleTransition(uerr) {
						logStaleTransition(order.ID, "failed")
					} else {
						logger.Errorf("❌ route-a: persist late FAILED for %d: %v", order.ID, uerr)
					}
				}
				timer.Milestone().With("recovered_via", "lifi_late_failed")
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
				Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridgeUncertain)).
				SetBridgeStatus(routeaorder.BridgeStatusFailed).
				SetFailureReason(reason).
				Save(ctx); uerr != nil {
				if isStaleTransition(uerr) {
					logStaleTransition(order.ID, "failed")
				} else {
					logger.Errorf("❌ route-a: persist window-expired FAILED for %d: %v", order.ID, uerr)
				}
			}
			timer.Milestone().With("recovered_via", "window_expired_to_failed")
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
		Where(routeaorder.BridgeStatusIn(
			routeaorder.BridgeStatusBridging,
			routeaorder.BridgeStatusBridgeUncertain,
		)).
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
		if isStaleTransition(err) {
			logStaleTransition(order.ID, "bridged")
			return false
		}
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
	timer := TimeSampled(ctx, order.ID, StepEvmCreateOrder, ActorDispatcher).
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

	// The merchant's payout amount is non-negotiable; our skim is.
	// When the bridged USDC can't cover amount+fee (rates drift between
	// quote and dispatch — and card-tap orders collect exactly
	// target/quote-rate with zero cushion), first trim the skim to
	// whatever fits, down to zero.
	if new(big.Int).Add(amount, senderFee).Cmp(bridged) > 0 {
		senderFee = new(big.Int).Sub(bridged, amount)
		if senderFee.Sign() < 0 {
			senderFee = big.NewInt(0)
		}
	}

	// True shortfall — even fee-less the bridged amount doesn't reach
	// the payout. Absorb small drift from the wallet's ops float
	// (stranding a customer order over basis points costs more than
	// the float), but refuse big gaps: those mean a quoting bug, not
	// market movement, and need a human.
	if amount.Cmp(bridged) > 0 {
		shortfall := new(big.Int).Sub(amount, bridged)
		maxShort := new(big.Int).Quo(
			new(big.Int).Mul(amount, big.NewInt(maxDispatchShortfallBPS)),
			big.NewInt(10_000),
		)
		if shortfall.Cmp(maxShort) > 0 {
			return fmt.Errorf("insufficient bridged USDC: have %s, need %s (live rate %s; shortfall exceeds %d bps tolerance)",
				bridged.String(), amount.String(), liveQuote.Rate.String(), maxDispatchShortfallBPS)
		}
		walletBal, berr := d.evm.USDC().BalanceOf(ctx, d.evm.From())
		if berr != nil {
			return fmt.Errorf("check wallet balance for drift absorption: %w", berr)
		}
		if walletBal.Cmp(amount) < 0 {
			return fmt.Errorf("insufficient bridged USDC and wallet float can't absorb drift: wallet %s, need %s",
				walletBal.String(), amount.String())
		}
		logger.Warnf("⚠️ route-a: order %d absorbing %s USDC-subunit rate drift from ops float (bridged %s, payout %s)",
			order.ID, shortfall.String(), bridged.String(), amount.String())
		timer.With("drift_absorbed_subunit", shortfall.String())
	}

	total := new(big.Int).Add(amount, senderFee)
	senderFeeDec := decimal.NewFromBigInt(senderFee, -int32(d.conf.BaseUSDCDecimals))

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

	// Allowance: grant the Gateway a one-time max approval instead of
	// re-approving per order — our own settlement contract, and each
	// approve tx costs a receipt wait (~5-10s) on the hot path. The
	// allowance check makes this a free no-op on every dispatch after
	// the first.
	approveErr := func() (aerr error) {
		var current *big.Int
		current, aerr = d.evm.USDC().Allowance(ctx, d.evm.From(), d.evm.Config().GatewayAddr)
		if aerr != nil || current.Cmp(total) >= 0 {
			return
		}
		atimer := Time(ctx, order.ID, StepEvmApprove, ActorDispatcher).
			With("approve_total", "max").
			With("spender", d.evm.Config().GatewayAddr.Hex())
		defer atimer.End(&aerr)
		_, aerr = d.evm.USDC().Approve(ctx, d.evm.Config().GatewayAddr, ethmath.MaxBig256)
		return
	}()
	if approveErr != nil {
		return fmt.Errorf("approve USDC: %w", approveErr)
	}

	// CLAIM before the funds-moving submit: dispatching with no
	// gateway order id yet. One CAS winner — racing writers can never
	// double-createOrder (= double payout). advanceDispatching skips
	// rows with an empty gateway id, and surfaces them loudly once
	// they're old enough to be a stuck claim.
	if _, cerr := order.Update().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusBridged)).
		SetBridgeStatus(routeaorder.BridgeStatusDispatching).
		Save(ctx); cerr != nil {
		if isStaleTransition(cerr) {
			logStaleTransition(order.ID, "dispatching (claim)")
			return nil
		}
		return fmt.Errorf("claim for dispatching: %w", cerr)
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
		var submitted *evm.SubmittedError
		if errors.As(cerr, &submitted) {
			// The tx is in the mempool and may mine — leave the claim
			// in place (re-running would double-pay) and record where
			// to look. The stuck-claim sweep keeps this visible.
			if _, uerr := order.Update().
				Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
				SetFailureReason(fmt.Sprintf("createOrder submitted but unconfirmed (tx %s) — verify on Basescan before any retry", submitted.TxHash.Hex())).
				Save(ctx); uerr != nil {
				logger.Errorf("❌ route-a: persist ambiguous-dispatch marker for %d: %v", order.ID, uerr)
			}
			logger.Errorf("❌ route-a: order %d createOrder ambiguous (parked in dispatching): %v", order.ID, cerr)
			return fmt.Errorf("createOrder (ambiguous; order parked): %w", cerr)
		}
		// Known-not-submitted — return the claim and retry next tick.
		if _, uerr := order.Update().
			Where(
				routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching),
				routeaorder.Or(routeaorder.GatewayOrderIDIsNil(), routeaorder.GatewayOrderIDEQ("")),
			).
			SetBridgeStatus(routeaorder.BridgeStatusBridged).
			Save(ctx); uerr != nil {
			logger.Errorf("❌ route-a: revert dispatch claim for %d: %v", order.ID, uerr)
		}
		return fmt.Errorf("createOrder: %w", cerr)
	}

	orderID := strings.ToLower(result.OrderID.Hex())
	if _, perr := order.Update().
		Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
		SetGatewayOrderID(orderID).
		SetGatewayChainID(uint64(d.conf.BaseChainID)).
		SetSenderFeeSubunit(senderFeeDec).
		SetBridgeTxDest(result.TxHash.Hex()).
		Save(ctx); perr != nil {
		// createOrder IS on-chain at this point — surface loudly; ops
		// resolves with the gateway order id.
		return fmt.Errorf("persist gateway order id (order %s already on-chain!): %w", orderID, perr)
	}
	logger.Infof("✅ route-a: createOrder submitted order=%d orderId=%s tx=%s gas=%d",
		order.ID, orderID, result.TxHash.Hex(), result.GasUsed)
	d.ExtendBurst() // keep chasing through settlement polling
	timer.With("gateway_order_id", orderID).
		With("evm_tx_hash", result.TxHash.Hex()).
		With("gas_used", result.GasUsed)
	return nil
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
		Where(
			routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching),
			// Treasury and lp_network orders in `dispatching` are mid
			// PAYOUT on the BaaS rail — Routes B/C own their own
			// pollers; Paycrest settlement must never drive their state.
			routeaorder.ModeEQ(routeaorder.ModeLp),
		).
		WithPaymentOrder(func(poq *ent.PaymentOrderQuery) {
			poq.WithSenderProfile()
		}).
		All(ctx)
	if err != nil {
		return err
	}

	for _, order := range orders {
		if order.GatewayOrderID == "" || order.GatewayChainID == 0 {
			// Claimed for dispatch but no gateway id — see the
			// equivalent bridging sweep; an old claim means an
			// ambiguous createOrder that needs Basescan verification.
			if time.Since(order.UpdatedAt) > stuckClaimAlertAfter {
				logger.Errorf("🚨 route-a: order %d stuck in dispatching with NO gateway id for %s — "+
					"verify the aggregator's recent Base txs (failure_reason may hold the tx hash)",
					order.ID, time.Since(order.UpdatedAt).Round(time.Minute))
			}
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
			Where(routeaorder.BridgeStatusEQ(routeaorder.BridgeStatusDispatching)).
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
			if isStaleTransition(err) {
				logStaleTransition(order.ID, string(info.Status))
				continue
			}
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
