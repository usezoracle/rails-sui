//go:build ignore

// arb_scan is a read-only CLI that scans Polymarket and Limitless for candidate
// cross-venue arbitrage opportunities and prints a ranked report.
//
// It places no orders and touches no funds. Every printed opportunity is a
// CANDIDATE: the prices line up, but a human must confirm that the two markets
// share the same resolution criteria, oracle, and cutoff before any capital is
// committed.
//
// Usage:
//
//	go run scripts/arb_scan.go                 # all matched candidates
//	go run scripts/arb_scan.go -top 20         # show top 20
//	go run scripts/arb_scan.go -min-sim 0.6 -buffer 0.03 -min-liq 500
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/usezoracle/rails-sui/services/arbitrage"
)

func main() {
	minSim := flag.Float64("min-sim", 0.55, "minimum title similarity to pair two markets")
	buffer := flag.Float64("buffer", 0.02, "fee+slippage buffer subtracted from gross edge")
	minLiq := flag.Float64("min-liq", 0, "minimum USDC liquidity per market (0 = no filter)")
	top := flag.Int("top", 0, "show only the top N candidates (0 = all)")
	flag.Parse()

	cfg := arbitrage.Config{
		MinSimilarity:     *minSim,
		FeeSlippageBuffer: *buffer,
		MinLiquidity:      *minLiq,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	report, err := arbitrage.RunScan(ctx, arbitrage.NewClient(), cfg, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(report.Render(*top))
}
