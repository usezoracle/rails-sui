package arbitrage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Report is the output of one full scan cycle.
type Report struct {
	GeneratedAt      time.Time
	PolymarketCount  int
	LimitlessCount   int
	Opportunities    []Opportunity // sorted by NetEdge desc
	PositiveEdgeOnly bool          // whether Opportunities was filtered
}

// RunScan fetches both venues and returns matched candidate opportunities.
// It is read-only end to end. `now` is injected so callers/tests control the
// timestamp (Date.now() equivalents are non-deterministic).
func RunScan(ctx context.Context, c *Client, cfg Config, now time.Time) (*Report, error) {
	poly, err := c.FetchPolymarket(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch polymarket: %w", err)
	}
	lim, err := c.FetchLimitless(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch limitless: %w", err)
	}
	return &Report{
		GeneratedAt:     now,
		PolymarketCount: len(poly),
		LimitlessCount:  len(lim),
		Opportunities:   Scan(poly, lim, cfg),
	}, nil
}

// PositiveEdge returns only opportunities whose NetEdge > 0 — the ones worth a
// human resolution-criteria review.
func (r *Report) PositiveEdge() []Opportunity {
	var out []Opportunity
	for _, o := range r.Opportunities {
		if o.NetEdge > 0 {
			out = append(out, o)
		}
	}
	return out
}

// Render produces a human-readable summary for the CLI / logs.
func (r *Report) Render(topN int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Arb scan @ %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "  polymarket markets: %d   limitless markets: %d\n", r.PolymarketCount, r.LimitlessCount)
	pos := r.PositiveEdge()
	fmt.Fprintf(&b, "  matched candidates: %d   positive-net-edge: %d\n\n", len(r.Opportunities), len(pos))

	shown := r.Opportunities
	if topN > 0 && len(shown) > topN {
		shown = shown[:topN]
	}
	for i, o := range shown {
		yes, no := o.Legs()
		fmt.Fprintf(&b, "#%d  net %+.3f  (gross %.3f, spread %.3f, sim %.2f)\n",
			i+1, o.NetEdge, o.GrossEdge, o.Spread, o.Similarity)
		fmt.Fprintf(&b, "    A %-10s %s\n", o.A.Venue, truncate(o.A.Title, 70))
		fmt.Fprintf(&b, "    B %-10s %s\n", o.B.Venue, truncate(o.B.Title, 70))
		fmt.Fprintf(&b, "    legs: buy %s@%s %.3f  +  buy %s@%s %.3f  = %.3f\n",
			yes.Side, yes.Venue, yes.Price, no.Side, no.Venue, no.Price, o.DutchBookCost)
		fmt.Fprintf(&b, "    ⚠ CANDIDATE — confirm resolution criteria/oracle/cutoff before trading\n")
		fmt.Fprintf(&b, "      %s\n      %s\n", o.A.URL, o.B.URL)
	}
	if len(pos) == 0 {
		fmt.Fprintf(&b, "No positive-net-edge candidates this cycle.\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
