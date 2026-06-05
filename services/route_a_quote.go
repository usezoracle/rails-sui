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

// QuoteSuiToNgn is a backward-compatible wrapper for SUI-specific off-ramp quoting.
func QuoteSuiToNgn(
	ctx context.Context,
	lifiClient *lifi.Client,
	settlementClient *settlement.Client,
	conf *config.OrderConfiguration,
	suiAggregatorAddress string,
	requestedSui decimal.Decimal,
) (*SuiCompositeRate, error) {
	return QuoteSuiTokenToNgn(ctx, lifiClient, settlementClient, conf, suiAggregatorAddress, requestedSui, NativeSuiCoinType, NativeSuiDecimals)
}

// QuoteSuiTokenToNgn composes a off-ramp rate for any Sui token (e.g. SUI, USDC)
// to fiat (NGN) by chaining the cross-chain bridge quote (Sui -> Base USDC) and the
// Paycrest NGN/USDC rate quote. This factors in all bridge fees, gas reservations, and slippage.
func QuoteSuiTokenToNgn(
	ctx context.Context,
	lifiClient *lifi.Client,
	settlementClient *settlement.Client,
	conf *config.OrderConfiguration,
	suiAggregatorAddress string,
	requestedAmount decimal.Decimal,
	fromToken string,
	fromDecimals int32,
) (*SuiCompositeRate, error) {
	if suiAggregatorAddress == "" {
		return nil, fmt.Errorf("sui aggregator address not configured (SUI_AGGREGATOR_PRIVATE_KEY)")
	}
	if conf.BaseUSDCContract == "" || conf.BaseAggregatorAddress == "" || conf.BaseChainID == 0 {
		return nil, fmt.Errorf("base destination not configured (BASE_USDC_CONTRACT / BASE_AGGREGATOR_ADDRESS / BASE_CHAIN_ID)")
	}

	var bridgedAmount decimal.Decimal
	var fromAmount string

	if IsNativeSui(fromToken) {
		// Reserve gas for native SUI bridging
		gasReservation := decimal.NewFromInt(NativeSuiGasReservation).Shift(-int32(NativeSuiDecimals))
		bridgedAmount = requestedAmount.Sub(gasReservation)
		if bridgedAmount.Sign() <= 0 {
			return nil, fmt.Errorf("amount %s SUI is below gas reservation %s SUI", requestedAmount, gasReservation)
		}
		fromAmount = bridgedAmount.Shift(int32(NativeSuiDecimals)).Truncate(0).String()
	} else {
		// USDC/stablecoin has no gas reservation on the token itself
		bridgedAmount = requestedAmount
		fromAmount = bridgedAmount.Shift(int32(fromDecimals)).Truncate(0).String()
	}

	qreq := lifi.QuoteRequest{
		FromChain:   strconv.FormatInt(lifi.SuiChainID, 10),
		ToChain:     strconv.FormatInt(conf.BaseChainID, 10),
		FromToken:   fromToken,
		ToToken:     conf.BaseUSDCContract,
		FromAmount:  fromAmount,
		FromAddress: suiAggregatorAddress,
		ToAddress:   conf.BaseAggregatorAddress,
		Slippage:    0.01, // Standard 1% slippage for stable-stable / general path
	}
	if IsNativeSui(fromToken) {
		qreq.Slippage = NativeSuiSlippage()
	}

	quote, err := lifiClient.GetQuote(ctx, qreq)
	if err != nil {
		return nil, fmt.Errorf("lifi bridge quote: %w", err)
	}

	// ToAmountMin is the floor LiFi guarantees; quoting off the floor ensures the user is not surprised by under-delivery
	usdcSubunit, ok := new(big.Int).SetString(quote.Estimate.ToAmountMin, 10)
	if !ok || usdcSubunit.Sign() <= 0 {
		return nil, fmt.Errorf("lifi returned no usable ToAmountMin (got %q)", quote.Estimate.ToAmountMin)
	}
	usdcEquivalent := decimal.NewFromBigInt(usdcSubunit, -int32(conf.BaseUSDCDecimals))

	// Factor in the platform sender fee (BaseSenderFeeBPS) charged during settlement on Base.
	// This ensures the merchant receives exactly the expected fiat amount.
	feeBpsDec := decimal.NewFromInt(conf.BaseSenderFeeBPS)
	feeFactor := decimal.NewFromInt(1).Sub(feeBpsDec.Div(decimal.NewFromInt(10000)))
	usdcNet := usdcEquivalent.Mul(feeFactor)

	paycrest, err := settlementClient.FetchRate(ctx, "base", "USDC", usdcNet, "NGN")
	if err != nil {
		return nil, fmt.Errorf("paycrest usdc/ngn rate: %w", err)
	}

	// NGN per Token = (USDC Net per Token) × (NGN per USDC).
	usdcPerToken := usdcNet.Div(bridgedAmount)
	composite := usdcPerToken.Mul(paycrest.Rate)

	return &SuiCompositeRate{
		Rate:                 composite,
		UsdcEquivalent:       usdcEquivalent, // Keep original bridged amount for transparency
		UsdcToNgnRate:        paycrest.Rate,
		BridgedSuiAmount:     bridgedAmount, // Holds the actual bridged token amount (net of gas reservation if SUI)
		ProviderIDs:          paycrest.ProviderIDs,
		OrderType:            paycrest.OrderType,
		RefundTimeoutMinutes: paycrest.RefundTimeoutMinutes,
	}, nil
}

// QuoteSuiTokenAmountForFiat resolves the source-token amount needed to deliver
// a target fiat amount through Route A. The Route-A quote is amount-sensitive
// because LiFi fees/slippage and Paycrest rates depend on size, so callers
// cannot safely use fiat / market_rate. We bootstrap with fallbackRate, quote,
// then re-quote once using the derived token amount.
func QuoteSuiTokenAmountForFiat(
	ctx context.Context,
	lifiClient *lifi.Client,
	settlementClient *settlement.Client,
	conf *config.OrderConfiguration,
	suiAggregatorAddress string,
	targetFiat decimal.Decimal,
	fallbackRate decimal.Decimal,
	fromToken string,
	fromDecimals int32,
) (*SuiCompositeRate, decimal.Decimal, error) {
	if !targetFiat.IsPositive() {
		return nil, decimal.Zero, fmt.Errorf("target fiat amount must be positive")
	}
	if !fallbackRate.IsPositive() {
		return nil, decimal.Zero, fmt.Errorf("fallback rate must be positive")
	}

	sourceAmount := targetFiat.Div(fallbackRate).Round(fromDecimals)
	var quote *SuiCompositeRate
	var err error
	for i := 0; i < 2; i++ {
		quote, err = QuoteSuiTokenToNgn(
			ctx,
			lifiClient,
			settlementClient,
			conf,
			suiAggregatorAddress,
			sourceAmount,
			fromToken,
			fromDecimals,
		)
		if err != nil {
			return nil, decimal.Zero, err
		}
		if !quote.Rate.IsPositive() {
			return nil, decimal.Zero, fmt.Errorf("route-a quote returned non-positive rate")
		}
		sourceAmount = targetFiat.Div(quote.Rate).Round(fromDecimals)
	}
	return quote, sourceAmount, nil
}
