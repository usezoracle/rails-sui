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

// TestQuoteFallsBackToCCTPRate exercises the quote path end-to-end
// against stub HTTP servers: LiFi hard-down, settlement aggregator
// quoting NGN/USDC=1500. A native-USDC quote must succeed via the 1:1
// CCTP rate (tagged QuotedVia="cctp"); a native-SUI quote must keep
// failing — CCTP has no swap leg to price it.
func TestQuoteFallsBackToCCTPRate(t *testing.T) {
	lifiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		t.Fatalf("USDC quote should fall back to CCTP, got: %v", err)
	}
	if got.QuotedVia != routeAProviderCCTP || got.BridgeProvider() != routeAProviderCCTP {
		t.Errorf("QuotedVia = %q, want cctp", got.QuotedVia)
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

	// Kill switch off — USDC quote must fail like before the fallback.
	conf.CCTPFallbackEnabled = false
	if _, err := QuoteSuiTokenToNgn(context.Background(), lifiClient, setClient, conf,
		"0xaggregator-sui", decimal.NewFromInt(100), net.SuiUSDCCoinType, 6); err == nil {
		t.Error("fallback disabled: want error")
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
