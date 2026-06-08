// Package arbitrage implements a read-only cross-venue opportunity scanner for
// binary prediction markets (Polymarket on Polygon, Limitless on Base).
//
// Design intent (first principles): this package is deliberately read-only and
// side-effect free. It fetches public market data, normalizes it into a common
// shape, matches markets that *appear* to describe the same event, and reports
// the price spread / dutch-book edge between venues.
//
// It does NOT decide that two markets are truly the same claim. Resolution-
// criteria equivalence is a semantic judgement that must be confirmed by a
// human before any capital is committed — every reported opportunity is a
// CANDIDATE, not a confirmed risk-free arbitrage. See Opportunity.NeedsReview.
package arbitrage

import "time"

// Venue identifies a prediction-market platform.
type Venue string

const (
	VenuePolymarket Venue = "polymarket"
	VenueLimitless  Venue = "limitless"
)

// Market is a normalized view of a single binary (YES/NO) prediction market.
// Prices are expressed as probabilities in [0,1]; YesPrice is the cost in USDC
// of one YES share (which pays $1 if the event resolves YES, else $0).
type Market struct {
	Venue Venue
	// ID is the venue-native identifier (condition id / market id).
	ID string
	// Title is the human-readable question text.
	Title string
	// Slug / URL for a human to open and inspect resolution criteria.
	URL string

	// YesPrice and NoPrice are the current implied probabilities in [0,1].
	// For a healthy binary market YesPrice+NoPrice ≈ 1, but they can diverge
	// with the spread; we keep both as quoted.
	YesPrice float64
	NoPrice  float64

	// Liquidity and Volume are in USDC (best-effort; 0 if unavailable). They
	// gate how much size an opportunity can actually absorb.
	Liquidity float64
	Volume    float64

	// ExpiresAt is when the market closes/resolves (zero if unknown). Drives
	// the capital-lockup component of return.
	ExpiresAt time.Time
}

// HasUsablePrices reports whether the quoted prices are in a sane range.
func (m Market) HasUsablePrices() bool {
	return m.YesPrice > 0 && m.YesPrice < 1 && m.NoPrice > 0 && m.NoPrice < 1
}

// Opportunity is a candidate cross-venue relationship between two markets that
// our text matcher believes describe the same event.
type Opportunity struct {
	A Market // market on venue A
	B Market // market on venue B

	// Similarity is the title-match score in [0,1] that linked A and B.
	Similarity float64

	// Spread is the absolute YES-price difference between the two venues — the
	// raw directional signal (buy cheap YES, that's the better entry).
	Spread float64

	// DutchBookCost is the cost of buying YES on the cheaper-YES venue plus NO
	// on the cheaper-NO venue. If < 1, exactly one leg pays $1 at resolution,
	// so (1 - cost) is the GROSS, pre-fee, pre-slippage locked edge.
	DutchBookCost float64

	// GrossEdge is max(0, 1 - DutchBookCost).
	GrossEdge float64

	// NetEdge is GrossEdge minus the configured fee+slippage buffer. Positive
	// NetEdge is a candidate worth a human resolution-criteria review.
	NetEdge float64

	// NeedsReview is always true: NetEdge>0 means the *prices* line up, never
	// that the two markets are the same claim. A human must confirm resolution
	// criteria, oracle, and cutoff time before any capital is committed.
	NeedsReview bool
}

// Leg describes one side of the dutch book for reporting.
type Leg struct {
	Venue Venue
	Side  string // "YES" or "NO"
	Price float64
}

// Legs returns the two buy legs that realize DutchBookCost.
func (o Opportunity) Legs() (yes Leg, no Leg) {
	// Buy YES wherever YES is cheaper.
	if o.A.YesPrice <= o.B.YesPrice {
		yes = Leg{o.A.Venue, "YES", o.A.YesPrice}
	} else {
		yes = Leg{o.B.Venue, "YES", o.B.YesPrice}
	}
	// Buy NO wherever NO is cheaper.
	if o.A.NoPrice <= o.B.NoPrice {
		no = Leg{o.A.Venue, "NO", o.A.NoPrice}
	} else {
		no = Leg{o.B.Venue, "NO", o.B.NoPrice}
	}
	return yes, no
}
