// route_a_quote.go composes the per-source-token quote logic for Route A.
//
// USDC orders quote directly off Paycrest (NGN/USDC); native SUI orders
// don't have a direct SUI/NGN venue, so we chain two quotes: LiFi gives
// us the SUI→USDC effective rate (after bridge fees + slippage), then
// Paycrest gives us USDC→NGN. The composite is the rate the user
// actually receives end-to-end.
package services

import (
	"context"
	"fmt"
	"math/big"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/services/settlement"
)

// NativeSuiCoinType is the Move type string for native SUI. Used to
// branch the quote/dispatch path; if you add another non-stable source
// token in the future, branch on its contract address the same way.
const NativeSuiCoinType = "0x2::sui::SUI"

// NativeSuiDecimals is the decimal precision of native SUI (per the Sui
// framework). All other tokens in this codebase pull their decimals
// from the Token table; SUI is hardcoded because the quote helper runs
// before the Token row is loaded.
const NativeSuiDecimals = 9

// NativeSuiGasReservation is the MIST amount we hold back from each
// SUI bridge so the aggregator wallet always has gas for the bridge tx
// itself. 10_000_000 MIST = 0.01 SUI, ~$0.04 at typical prices —
// enough for ~50 LiFi quote-and-bridge txs at the current reference
// gas price. The reservation is baked into both the user-facing quote
// and the dispatcher's LiFi fromAmount so the two stay in sync.
const NativeSuiGasReservation int64 = 10_000_000

// NativeSuiSlippageBPS is the slippage tolerance passed to LiFi for
// SUI→USDC bridges. 50 bps (0.5%) vs LiFi's 30 bps (0.3%) default —
// SUI/USDC has meaningful price impact at our typical order sizes,
// where USDC→USDC bridging is effectively zero-slip. Per-order
// overrides can come later if specific integrators need them.
const NativeSuiSlippageBPS = 50

// IsNativeSui reports whether a Token.ContractAddress refers to native
// SUI. Centralized here so future callers don't grow string compares.
func IsNativeSui(contractAddress string) bool {
	return contractAddress == NativeSuiCoinType
}

// NativeSuiSlippage is NativeSuiSlippageBPS as a float in the form LiFi
// expects (0.005 = 0.5%).
func NativeSuiSlippage() float64 {
	return float64(NativeSuiSlippageBPS) / 10_000.0
}

// SuiCompositeRate is the resolved Route-A rate for a native-SUI order:
// what the user effectively gets in NGN per SUI, broken down so the
// caller can persist or display each leg.
type SuiCompositeRate struct {
	// Rate is NGN per SUI — composite of LiFi (SUI/USDC) × Paycrest
	// (NGN/USDC). This is what we store on PaymentOrder.Rate for SUI
	// orders.
	Rate decimal.Decimal

	// UsdcEquivalent is what LiFi quoted us as the USDC delivered on
	// Base for the bridged SUI amount (already net of LiFi fees +
	// slippage tolerance). Used both for display and for the Paycrest
	// rate query (which is amount-sensitive).
	UsdcEquivalent decimal.Decimal

	// UsdcToNgnRate is the underlying Paycrest NGN/USDC rate at the
	// quoted USDC size. Lets us reconstruct what the EVM-side
	// createOrder call uses without re-querying.
	UsdcToNgnRate decimal.Decimal

	// BridgedSuiAmount is the SUI we actually quote to LiFi —
	// (requested amount − gas reservation). Returned so the dispatcher
	// can reuse the same number at bridge time without re-deriving it.
	BridgedSuiAmount decimal.Decimal

	// ProviderIDs / OrderType / RefundTimeoutMinutes mirror the
	// settlement RateQuote — same fields the USDC path already
	// surfaces in logs.
	ProviderIDs          []string
	OrderType            string
	RefundTimeoutMinutes int
}

// QuoteSuiToNgn composes a SUI/NGN rate for the requested SUI amount.
//
// It first asks LiFi for a (bridgedSui SUI → USDC) quote — bridgedSui
// is requested-amount minus the gas reservation. Then it asks Paycrest
// for the NGN/USDC rate at the USDC amount LiFi will deliver. The
// composite SUI/NGN is (USDC/SUI) × (NGN/USDC).
//
// The aggregator's Sui address is required because LiFi's /quote
// endpoint conditions the route (and therefore the rate) on the
// sender; passing a junk address can change which bridges LiFi offers.
//
// Errors surface as-is: a LiFi outage or Paycrest 5xx fails the order
// creation. The caller should return 502 to the user, matching the
// USDC path's behavior on rate-fetch failure.
func QuoteSuiToNgn(
	ctx context.Context,
	lifiClient *lifi.Client,
	settlementClient *settlement.Client,
	conf *config.OrderConfiguration,
	suiAggregatorAddress string,
	requestedSui decimal.Decimal,
) (*SuiCompositeRate, error) {
	if suiAggregatorAddress == "" {
		return nil, fmt.Errorf("sui aggregator address not configured (SUI_AGGREGATOR_PRIVATE_KEY)")
	}
	if conf.BaseUSDCContract == "" || conf.BaseAggregatorAddress == "" || conf.BaseChainID == 0 {
		return nil, fmt.Errorf("base destination not configured (BASE_USDC_CONTRACT / BASE_AGGREGATOR_ADDRESS / BASE_CHAIN_ID)")
	}

	// Reserve gas before quoting. The user-facing rate must reflect
	// what actually gets bridged, not what they deposited.
	gasReservation := decimal.NewFromInt(NativeSuiGasReservation).Shift(-int32(NativeSuiDecimals))
	bridgedSui := requestedSui.Sub(gasReservation)
	if bridgedSui.Sign() <= 0 {
		return nil, fmt.Errorf("amount %s SUI is below gas reservation %s SUI", requestedSui, gasReservation)
	}

	fromAmount := bridgedSui.Shift(int32(NativeSuiDecimals)).Truncate(0).String()
	qreq := lifi.QuoteRequest{
		FromChain:   strconv.FormatInt(lifi.SuiChainID, 10),
		ToChain:     strconv.FormatInt(conf.BaseChainID, 10),
		FromToken:   NativeSuiCoinType,
		ToToken:     conf.BaseUSDCContract,
		FromAmount:  fromAmount,
		FromAddress: suiAggregatorAddress,
		ToAddress:   conf.BaseAggregatorAddress,
		Slippage:    NativeSuiSlippage(),
	}
	quote, err := lifiClient.GetQuote(ctx, qreq)
	if err != nil {
		return nil, fmt.Errorf("lifi sui→usdc quote: %w", err)
	}
	// ToAmountMin is the floor LiFi guarantees at this slippage; quoting
	// off the floor (rather than ToAmount, the optimistic estimate)
	// means the user is never surprised by under-delivery at bridge time.
	usdcSubunit, ok := new(big.Int).SetString(quote.Estimate.ToAmountMin, 10)
	if !ok || usdcSubunit.Sign() <= 0 {
		return nil, fmt.Errorf("lifi returned no usable ToAmountMin (got %q)", quote.Estimate.ToAmountMin)
	}
	usdcEquivalent := decimal.NewFromBigInt(usdcSubunit, -int32(conf.BaseUSDCDecimals))

	paycrest, err := settlementClient.FetchRate(ctx, "base", "USDC", usdcEquivalent, "NGN")
	if err != nil {
		return nil, fmt.Errorf("paycrest usdc/ngn rate: %w", err)
	}

	// NGN per SUI = (USDC per SUI) × (NGN per USDC).
	usdcPerSui := usdcEquivalent.Div(bridgedSui)
	composite := usdcPerSui.Mul(paycrest.Rate)

	return &SuiCompositeRate{
		Rate:                 composite,
		UsdcEquivalent:       usdcEquivalent,
		UsdcToNgnRate:        paycrest.Rate,
		BridgedSuiAmount:     bridgedSui,
		ProviderIDs:          paycrest.ProviderIDs,
		OrderType:            paycrest.OrderType,
		RefundTimeoutMinutes: paycrest.RefundTimeoutMinutes,
	}, nil
}
