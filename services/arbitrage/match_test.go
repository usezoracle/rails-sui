package arbitrage

import (
	"math"
	"testing"
)

func TestSimilarity(t *testing.T) {
	cases := []struct {
		name   string
		a, b   string
		expect func(float64) bool
	}{
		{
			name:   "near-identical questions match high",
			a:      "Will Bitcoin reach $100,000 by December 31, 2026?",
			b:      "Will BTC hit 100000 before Dec 31 2026?",
			expect: func(s float64) bool { return s >= 0.3 },
		},
		{
			name:   "unrelated questions match low",
			a:      "Will the Lakers win the NBA championship?",
			b:      "Will inflation exceed 5% next year?",
			expect: func(s float64) bool { return s < 0.1 },
		},
		{
			name:   "stopwords-only stripped to empty -> 0",
			a:      "Will the market resolve yes or no?",
			b:      "Lakers championship outcome",
			expect: func(s float64) bool { return s == 0 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Similarity(tc.a, tc.b)
			if !tc.expect(got) {
				t.Fatalf("Similarity(%q,%q)=%v failed expectation", tc.a, tc.b, got)
			}
		})
	}
}

func mkt(v Venue, title string, yes, no float64) Market {
	return Market{Venue: v, ID: title, Title: title, YesPrice: yes, NoPrice: no}
}

func TestEvaluateDutchBook(t *testing.T) {
	// YES cheaper on A (0.55), NO cheaper on B (0.40) -> cost 0.95, gross 0.05.
	a := mkt(VenuePolymarket, "Will X happen", 0.55, 0.46)
	b := mkt(VenueLimitless, "Will X happen", 0.62, 0.40)
	cfg := DefaultConfig()
	o := evaluate(a, b, cfg)

	if !approx(o.DutchBookCost, 0.95) {
		t.Fatalf("DutchBookCost=%v want 0.95", o.DutchBookCost)
	}
	if !approx(o.GrossEdge, 0.05) {
		t.Fatalf("GrossEdge=%v want 0.05", o.GrossEdge)
	}
	if !approx(o.NetEdge, 0.05-cfg.FeeSlippageBuffer) {
		t.Fatalf("NetEdge=%v want %v", o.NetEdge, 0.05-cfg.FeeSlippageBuffer)
	}
	if !o.NeedsReview {
		t.Fatal("NeedsReview must always be true")
	}
	yes, no := o.Legs()
	if yes.Venue != VenuePolymarket || no.Venue != VenueLimitless {
		t.Fatalf("legs picked wrong venues: yes=%v no=%v", yes, no)
	}
}

func TestEvaluateNoEdgeWhenOverpriced(t *testing.T) {
	// Both sides expensive -> cost > 1 -> gross edge clamped to 0.
	a := mkt(VenuePolymarket, "q", 0.60, 0.55)
	b := mkt(VenueLimitless, "q", 0.58, 0.50)
	o := evaluate(a, b, DefaultConfig())
	if o.GrossEdge != 0 {
		t.Fatalf("GrossEdge=%v want 0", o.GrossEdge)
	}
}

func TestScanFiltersAndSorts(t *testing.T) {
	poly := []Market{
		mkt(VenuePolymarket, "Will Bitcoin close above 100k in 2026", 0.55, 0.46),
		mkt(VenuePolymarket, "Will Ethereum flip Bitcoin in 2026", 0.30, 0.71),
		mkt(VenuePolymarket, "broken prices", 0, 0), // unusable -> skipped
	}
	lim := []Market{
		mkt(VenueLimitless, "Will Bitcoin close above 100000 in 2026", 0.62, 0.40), // matches #1, edge
		mkt(VenueLimitless, "Will Ethereum flip Bitcoin in 2026", 0.33, 0.69),      // matches #2, no edge
	}
	cfg := DefaultConfig()
	got := Scan(poly, lim, cfg)
	if len(got) != 2 {
		t.Fatalf("expected 2 matched opportunities, got %d", len(got))
	}
	// Sorted by NetEdge desc: the BTC dutch book should rank first.
	if got[0].NetEdge < got[1].NetEdge {
		t.Fatalf("results not sorted by NetEdge desc: %v", got)
	}
	if !contains(got[0].A.Title, "Bitcoin") {
		t.Fatalf("top opportunity should be the BTC pair, got %q", got[0].A.Title)
	}
}

func TestScanRespectsMinSimilarity(t *testing.T) {
	poly := []Market{mkt(VenuePolymarket, "Will the Lakers win the title", 0.5, 0.5)}
	lim := []Market{mkt(VenueLimitless, "Will inflation exceed five percent", 0.5, 0.5)}
	cfg := DefaultConfig()
	cfg.MinSimilarity = 0.55
	if got := Scan(poly, lim, cfg); len(got) != 0 {
		t.Fatalf("unrelated markets should not match, got %d", len(got))
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
