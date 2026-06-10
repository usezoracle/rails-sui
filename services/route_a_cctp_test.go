package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/services/cctp"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/services/settlement"
)

func TestUsdcSubunitsUint64(t *testing.T) {
	cases := []struct {
		amount  string
		want    uint64
		wantErr bool
	}{
		{"25.5", 25_500_000, false},      // typical card debit
		{"0.000001", 1, false},           // 1 subunit
		{"0.0000001", 0, true},           // truncates to zero → reject
		{"0", 0, true},                   // zero → reject
		{"-3", 0, true},                  // negative → reject
		{"123456789.123456", 123_456_789_123_456, false},
	}
	for _, tc := range cases {
		got, err := usdcSubunitsUint64(decimal.RequireFromString(tc.amount), 6)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got %d", tc.amount, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.amount, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.amount, got, tc.want)
		}
	}
}

// TestQuoteUSDCViaCCTPPrimary exercises the quote path end-to-end
// against stub HTTP servers: LiFi hard-down, settlement aggregator
// quoting NGN/USDC=1500. Native-USDC quotes price via the 1:1 CCTP
// rail WITHOUT touching LiFi at all (it's the primary); native SUI
// still needs LiFi; the kill switch reverts USDC to the LiFi path.
func TestQuoteUSDCViaCCTPPrimary(t *testing.T) {
	var lifiHits int
	lifiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lifiHits++
		http.Error(w, `{"message":"upstream exploded"}`, http.StatusBadGateway)
	}))
	defer lifiSrv.Close()

	setSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2/rates/base/USDC/") {
			t.Errorf("unexpected settlement path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"sell":{
			"rate":"1500","providerIds":["p1"],"orderType":"fiat","refundTimeoutMinutes":30}}}`))
	}))
	defer setSrv.Close()

	conf := &config.OrderConfiguration{
		CCTPFallbackEnabled:   true,
		BaseChainID:           8453,
		BaseUSDCContract:      "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
		BaseAggregatorAddress: "0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B",
		BaseUSDCDecimals:      6,
		BaseSenderFeeBPS:      50,
	}
	net, _ := cctp.ForBaseChainID(conf.BaseChainID)
	lifiClient := &lifi.Client{BaseURL: lifiSrv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	setClient := settlement.New(setSrv.URL, time.Minute)

	got, err := QuoteSuiTokenToNgn(context.Background(), lifiClient, setClient, conf,
		"0xaggregator-sui", decimal.NewFromInt(100), net.SuiUSDCCoinType, 6)
	if err != nil {
		t.Fatalf("USDC quote should price via CCTP, got: %v", err)
	}
	if got.QuotedVia != routeAProviderCCTP || got.BridgeProvider() != routeAProviderCCTP {
		t.Errorf("QuotedVia = %q, want cctp", got.QuotedVia)
	}
	if lifiHits != 0 {
		t.Errorf("USDC quote hit LiFi %d times — CCTP is primary, LiFi should not be called", lifiHits)
	}
	if !got.UsdcEquivalent.Equal(decimal.NewFromInt(100)) {
		t.Errorf("UsdcEquivalent = %s, want 100 (1:1)", got.UsdcEquivalent)
	}
	// Rate = (100 × (1 − 0.005) / 100) × 1500 = 1492.5 NGN per USDC.
	if want := decimal.RequireFromString("1492.5"); !got.Rate.Equal(want) {
		t.Errorf("Rate = %s, want %s", got.Rate, want)
	}

	// Native SUI cannot be priced by CCTP — the LiFi error must surface.
	if _, err := QuoteSuiTokenToNgn(context.Background(), lifiClient, setClient, conf,
		"0xaggregator-sui", decimal.NewFromInt(100), NativeSuiCoinType, NativeSuiDecimals); err == nil {
		t.Error("native SUI quote with LiFi down: want error")
	}

	// Kill switch off — USDC reverts to the LiFi path (down → error).
	conf.CCTPFallbackEnabled = false
	if _, err := QuoteSuiTokenToNgn(context.Background(), lifiClient, setClient, conf,
		"0xaggregator-sui", decimal.NewFromInt(100), net.SuiUSDCCoinType, 6); err == nil {
		t.Error("CCTP disabled + LiFi down: want error")
	}
	if lifiHits == 0 {
		t.Error("CCTP disabled: USDC quote should have gone to LiFi")
	}
}

// TestLifiCoversQuote pins the fit-guard arithmetic that decides
// whether LiFi may execute an order quoted at CCTP's 1:1 rate.
func TestLifiCoversQuote(t *testing.T) {
	d := decimal.RequireFromString
	// Order: 100 USDC quoted 1:1 at rate 1492.5 NGN/USDC (0.5% fee) →
	// target 149,250 NGN. At live rate 1500, required ≈ 99.5 × 1.005 =
	// 99.9975 USDC.
	target := d("149250")
	if !lifiCoversQuote(target, d("1500"), 50, d("99.9975")) {
		t.Error("exactly-covering delivery should pass")
	}
	if lifiCoversQuote(target, d("1500"), 50, d("99.7")) {
		t.Error("under-delivery should be refused")
	}
	// Rate moved in our favor (NGN/USDC up) — smaller delivery suffices.
	if !lifiCoversQuote(target, d("1530"), 50, d("98.5")) {
		t.Error("favorable rate move should let a smaller delivery pass")
	}
	if lifiCoversQuote(target, d("0"), 50, d("100")) {
		t.Error("non-positive live rate should be refused")
	}
}

// TestCCTPReadyGates verifies the fallback declines to run (with a
// reason) until every prerequisite is in place — the property that
// guarantees a misconfigured fallback degrades to "LiFi-only", never
// to a broken bridge.
func TestCCTPReadyGates(t *testing.T) {
	d := &RouteADispatcher{conf: &config.OrderConfiguration{
		BaseAggregatorAddress: "0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B",
	}}

	d.conf.CCTPFallbackEnabled = false
	if _, ok := d.cctpReady(); ok {
		t.Error("disabled flag: want not ready")
	}

	d.conf.CCTPFallbackEnabled = true
	d.cctpNetOK = false
	if _, ok := d.cctpReady(); ok {
		t.Error("unknown network: want not ready")
	}

	d.cctpNetOK = true
	d.evm = nil
	if _, ok := d.cctpReady(); ok {
		t.Error("nil evm client: want not ready")
	}
}
