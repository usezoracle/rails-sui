package arbitrage

import (
	"math"
	"sort"
	"strings"
)

// Config tunes the scanner's thresholds. Defaults are conservative — they err
// toward surfacing candidates for human review rather than auto-trusting them.
type Config struct {
	// MinSimilarity is the title-match score required to pair two markets.
	MinSimilarity float64
	// FeeSlippageBuffer is subtracted from the gross edge to get NetEdge. It
	// stands in for trading fees, gas on two chains, and execution slippage.
	FeeSlippageBuffer float64
	// MinLiquidity (USDC) filters out markets too thin to execute against.
	MinLiquidity float64
}

// DefaultConfig returns sane starting thresholds. Tune against real data.
func DefaultConfig() Config {
	return Config{
		MinSimilarity:     0.55,
		FeeSlippageBuffer: 0.02, // ~2 cents of fees+slippage across both legs
		MinLiquidity:      0,    // 0 = don't filter; raise once measured
	}
}

// stopwords are dropped before comparing titles — they carry no event-identity
// signal and inflate similarity between unrelated questions.
var stopwords = map[string]bool{
	"will": true, "the": true, "a": true, "an": true, "be": true, "to": true,
	"of": true, "in": true, "on": true, "at": true, "by": true, "for": true,
	"is": true, "are": true, "this": true, "that": true, "market": true,
	"resolve": true, "yes": true, "no": true, "or": true, "and": true,
}

// tokenize lowercases, strips punctuation, and drops stopwords, returning the
// remaining content tokens. Numbers and tickers (e.g. "0.21652", "btc") are
// kept — they're often the discriminating part of a market question.
func tokenize(title string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == ' ':
			b.WriteRune(r)
		case r == ',':
			// Drop commas entirely so digit grouping ("100,000") collapses to a
			// single token matching the un-grouped form ("100000").
		default:
			b.WriteRune(' ')
		}
	}
	var out []string
	for _, tok := range strings.Fields(b.String()) {
		tok = strings.Trim(tok, ".")
		if tok == "" || stopwords[tok] {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// Similarity returns a [0,1] Jaccard score over the content-token sets of two
// titles. It is intentionally simple and transparent: you can read a pair and
// understand exactly why it matched. Swap in embeddings later if recall is low,
// but keep the human review gate regardless.
func Similarity(titleA, titleB string) float64 {
	a := toSet(tokenize(titleA))
	b := toSet(tokenize(titleB))
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for tok := range a {
		if b[tok] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func toSet(toks []string) map[string]bool {
	s := make(map[string]bool, len(toks))
	for _, t := range toks {
		s[t] = true
	}
	return s
}

// evaluate computes the spread / dutch-book economics for an already-paired
// (a, b). It assumes both have usable prices.
func evaluate(a, b Market, cfg Config) Opportunity {
	// Dutch book: buy each side wherever it's cheapest across the two venues.
	dutchCost := math.Min(a.YesPrice, b.YesPrice) + math.Min(a.NoPrice, b.NoPrice)
	gross := math.Max(0, 1-dutchCost)
	o := Opportunity{
		A:             a,
		B:             b,
		Similarity:    Similarity(a.Title, b.Title),
		Spread:        math.Abs(a.YesPrice - b.YesPrice),
		DutchBookCost: dutchCost,
		GrossEdge:     gross,
		NetEdge:       gross - cfg.FeeSlippageBuffer,
		NeedsReview:   true,
	}
	return o
}

// Scan pairs every market on venue A with the best-matching market on venue B
// (above the similarity threshold) and returns the resulting opportunities,
// sorted by NetEdge descending. Pure function: no network, no side effects.
//
// Matching is greedy-best per A-market: each A is linked to the single highest-
// similarity B. That keeps results easy to reason about; a fancier global
// assignment isn't worth the opacity for a review-gated scanner.
func Scan(aMarkets, bMarkets []Market, cfg Config) []Opportunity {
	var out []Opportunity
	for _, a := range aMarkets {
		if !a.HasUsablePrices() || a.Liquidity < cfg.MinLiquidity {
			continue
		}
		bestSim := 0.0
		var best *Market
		for i := range bMarkets {
			b := bMarkets[i]
			if !b.HasUsablePrices() || b.Liquidity < cfg.MinLiquidity {
				continue
			}
			sim := Similarity(a.Title, b.Title)
			if sim > bestSim {
				bestSim = sim
				best = &bMarkets[i]
			}
		}
		if best == nil || bestSim < cfg.MinSimilarity {
			continue
		}
		out = append(out, evaluate(a, *best, cfg))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].NetEdge > out[j].NetEdge
	})
	return out
}
