package cards

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/ent"
)

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q: %v", s, err)
	}
	return d
}

// The "0.15 vs 1.37" fix: a ₦1,500 charge at the live rate must debit the
// correct USDC value (~1.37 USDC), NOT the raw kobo figure read as micro
// (which gave 0.15 USDC).
func TestUsdcSubunitFromNGN_ChargeIsCorrect(t *testing.T) {
	got := usdcSubunitFromNGN(dec(t, "1500"), dec(t, "1097.3"))

	// 1500 / 1097.3 = 1.366991… USDC → 1,366,991 micro (truncated).
	const wantApprox = 1_366_991
	if got < wantApprox-5 || got > wantApprox+5 {
		t.Fatalf("₦1500 → %d micro USDC; want ~%d (~1.37 USDC)", got, wantApprox)
	}
	// Regression guard: the old bug shoved kobo (150000) into the USDC slot.
	if got == 150_000 {
		t.Fatalf("regression: NGN kobo passed as USDC micro (the 0.15 bug)")
	}
}

func TestUsdcSubunitFromNGN_NeverZero(t *testing.T) {
	// Sub-cent charges must floor to 1, never 0 (Move rejects EZeroAmount).
	if got := usdcSubunitFromNGN(dec(t, "0.0001"), dec(t, "1097.3")); got == 0 {
		t.Fatalf("got 0 micro; want >= 1")
	}
}

func TestUsdcFromNGN_OrderAmount(t *testing.T) {
	// The order is denominated in USDC so the Route A dispatcher bridges the
	// right amount. ₦1,500 @ 1097.3 ≈ 1.367 USDC.
	got := usdcFromNGN(dec(t, "1500"), dec(t, "1097.3"))
	lo, hi := dec(t, "1.36"), dec(t, "1.37")
	if got.LessThan(lo) || got.GreaterThan(hi) {
		t.Fatalf("order USDC amount = %s; want between %s and %s", got, lo, hi)
	}
}

// The on-chain debit amount and the recorded order amount must agree (both
// derived from the same NGN→USDC conversion), or the dispatcher would bridge a
// different amount than was actually debited from the cap.
func TestDebitAndOrderAmountAgree(t *testing.T) {
	amount, rate := dec(t, "1500"), dec(t, "1097.3")
	debitMicro := usdcSubunitFromNGN(amount, rate)
	orderMicro := usdcFromNGN(amount, rate).
		Mul(decimal.NewFromInt(1_000_000)).BigInt().Uint64()
	if debitMicro != orderMicro {
		t.Fatalf("debit %d micro != order %d micro — dispatcher would bridge the wrong amount",
			debitMicro, orderMicro)
	}
}

// Tier resolution stays in NGN kobo (off-chain enforcement) — the PIN /
// step-up gates must trip at the right thresholds.
func TestResolveTier(t *testing.T) {
	card := &ent.TappCard{
		PerTapLimitSubunit:     200_000,   // ₦2,000 — below this: no PIN
		StepUpThresholdSubunit: 1_500_000, // ₦15,000 — above this: step-up
	}
	cases := []struct {
		name   string
		amount uint64 // NGN kobo
		want   string
	}{
		{"below per-tap → none", 100_000, "none"},        // ₦1,000
		{"at per-tap → pin", 200_000, "pin"},             // ₦2,000 (not < perTap)
		{"mid → pin", 500_000, "pin"},                    // ₦5,000
		{"at step-up → step_up", 1_500_000, "step_up"},   // ₦15,000
		{"above step-up → step_up", 2_000_000, "step_up"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveTier(card, c.amount); got != c.want {
				t.Errorf("resolveTier(%d) = %s; want %s", c.amount, got, c.want)
			}
		})
	}
}

// A card with zero limits (pre-configuration) must fall back to the documented
// defaults rather than letting everything through as "none".
func TestResolveTier_Defaults(t *testing.T) {
	card := &ent.TappCard{} // all zero → defaults apply
	if got := resolveTier(card, 100_000); got != "none" { // ₦1,000
		t.Errorf("default none: got %s", got)
	}
	if got := resolveTier(card, 1_000_000); got != "pin" { // ₦10,000
		t.Errorf("default pin: got %s", got)
	}
	if got := resolveTier(card, 2_000_000); got != "step_up" { // ₦20,000
		t.Errorf("default step_up: got %s", got)
	}
}

// The card balance lives only on-chain in CardSpendingCap.balance, which a Sui
// node serializes as a BARE scalar string — the bug we fixed (old code
// expected {value: …} and fell through to "0", so every card showed 0).
func TestParseCapBalanceField(t *testing.T) {
	if got := ParseCapBalanceField(map[string]any{"balance": "1000000"}); got != "1000000" {
		t.Errorf("scalar balance: got %q want 1000000", got)
	}
	// Defensive: still handle a {value: …} wrapper if a node ever returns it.
	if got := ParseCapBalanceField(map[string]any{"balance": map[string]any{"value": "500000"}}); got != "500000" {
		t.Errorf("wrapped balance: got %q want 500000", got)
	}
	if got := ParseCapBalanceField(map[string]any{}); got != "0" {
		t.Errorf("missing balance: got %q want 0", got)
	}
	if got := ParseCapBalanceField(nil); got != "0" {
		t.Errorf("nil fields: got %q want 0", got)
	}
}
