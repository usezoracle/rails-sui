package tasks

import (
	"fmt"
	"os"
	"testing"

	"github.com/shopspring/decimal"
)

// TestLiveRate hits the real external rate sources and prints each one plus the
// aggregated median. It's a manual diagnostic, not a CI test — gated behind
// RUN_LIVE_RATE so it never runs (or flakes) in the normal suite.
//
//	RUN_LIVE_RATE=1 go test ./tasks/ -run TestLiveRate -v
func TestLiveRate(t *testing.T) {
	if os.Getenv("RUN_LIVE_RATE") == "" {
		t.Skip("set RUN_LIVE_RATE=1 to hit live rate sources")
	}

	currency := os.Getenv("RATE_CURRENCY")
	if currency == "" {
		currency = "NGN"
	}

	fmt.Printf("\n== live USDT/%s rate sources ==\n", currency)
	for _, src := range []struct {
		name string
		fn   func(string) (decimal.Decimal, error)
	}{
		{"paycrest", fetchPaycrestRate},
		{"binance", fetchBinanceP2PRate},
		{"quidax", fetchQuidaxRate},
	} {
		r, err := src.fn(currency)
		if err != nil {
			fmt.Printf("  %-9s -> miss (%v)\n", src.name, err)
			continue
		}
		fmt.Printf("  %-9s -> %s\n", src.name, r.StringFixed(2))
	}

	agg, err := fetchExternalRate(currency)
	if err != nil {
		t.Fatalf("fetchExternalRate(%s): %v", currency, err)
	}
	fmt.Printf("  --------\n  MEDIAN    -> %s %s per 1 USDT\n\n", agg.StringFixed(2), currency)
}
