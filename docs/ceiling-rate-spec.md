# Dynamic Ceiling Rate — Spec

## Purpose

Replace Paycrest's fixed 0.5 BPS slippage tolerance (`services/priority_queue.go:291`) with a dynamic rate ceiling tied to off-chain spot prices. Prevents LPs from setting "vibes-based" rates while letting them compete inside a defined band.

**Rule:** An LP's quoted rate for a fiat currency is acceptable only if `lp_rate ≤ spot_median × (1 + ceiling_band)`. Default `ceiling_band = 0.01` (1% above spot, per PRD).

Enforcement happens in the matching engine, not on-chain. The Move Gateway accepts whatever rate is encoded in the order; the matching engine refuses to assign LPs whose rate exceeds the ceiling.

---

## Architecture

```
┌────────────────────────────────────────────────────────────┐
│                  Spot Rate Pollers (tasks/)                │
│                                                            │
│  Binance P2P  Quidax  CoinGecko  + per-market providers    │
└──────────┬────────┬────────┬────────────────┬──────────────┘
           │        │        │                │
           ▼        ▼        ▼                ▼
       Write to Redis keys:
       spot:{currency}:{provider} → {rate, timestamp}
                       │
                       ▼
┌────────────────────────────────────────────────────────────┐
│              services/ceiling_rate.go                      │
│                                                            │
│   GetSpotMedian(currency) → median across fresh providers  │
│   GetCeilingRate(currency) → median * (1 + band)           │
│   GetCeilingBand(currency) → from config, admin overridable│
└──────────┬─────────────────────────────────────────────────┘
           │
           ▼
┌────────────────────────────────────────────────────────────┐
│              services/priority_queue.go:291                │
│                                                            │
│   replaces:                                                │
│     rate.Sub(order.Rate).Abs() <= 0.5                      │
│   with:                                                    │
│     lp_rate <= ceiling_rate(currency)                      │
└────────────────────────────────────────────────────────────┘
```

---

## Spot Rate Sources

| Provider | Coverage | Polling cadence | Notes |
|---|---|---|---|
| **Binance P2P** | NGN, IDR, KES (limited), and most major EM currencies | 30s | Best emerging-market coverage. Pull median of top-5 ads per direction. Paycrest already polls (`tasks/tasks.go:919`). |
| **Quidax** | NGN, KES, GHS | 30s | African corridor specialist. Paycrest already polls. |
| **CoinGecko** | All major + tokens | 60s | Sanity check / fallback. Less accurate for P2P-driven EM markets. |
| **Per-market local** | e.g. Indodax (IDR), Bitvavo (EUR) | 60s | Add per launch market. |

Each provider writes to Redis: `SET spot:{currency}:{provider} '{"rate": 1530.5, "ts": 1700000000}' EX 120`.

The TTL (120s) is the freshness window. If a provider hasn't refreshed in 120s, its key expires and it drops out of the median.

---

## Median Algorithm

```go
// services/ceiling_rate.go
func GetSpotMedian(ctx context.Context, currency string) (decimal.Decimal, error) {
    pattern := fmt.Sprintf("spot:%s:*", currency)
    keys, err := redis.Keys(ctx, pattern).Result()
    if err != nil { return decimal.Zero, err }

    rates := []decimal.Decimal{}
    for _, k := range keys {
        raw, err := redis.Get(ctx, k).Result()
        if err == redis.Nil { continue }
        if err != nil { return decimal.Zero, err }

        var entry struct {
            Rate decimal.Decimal `json:"rate"`
            Ts   int64           `json:"ts"`
        }
        if err := json.Unmarshal([]byte(raw), &entry); err != nil { continue }

        // Reject stale (defensive — TTL should already kill them)
        if time.Since(time.Unix(entry.Ts, 0)) > 120*time.Second { continue }

        rates = append(rates, entry.Rate)
    }

    if len(rates) < 2 {
        return decimal.Zero, ErrInsufficientSpotSources
    }

    sort.Slice(rates, func(i, j int) bool { return rates[i].LessThan(rates[j]) })

    mid := len(rates) / 2
    if len(rates)%2 == 0 {
        return rates[mid-1].Add(rates[mid]).Div(decimal.NewFromInt(2)), nil
    }
    return rates[mid], nil
}

func GetCeilingRate(ctx context.Context, currency string) (decimal.Decimal, error) {
    spot, err := GetSpotMedian(ctx, currency)
    if err != nil { return decimal.Zero, err }

    band, err := GetCeilingBand(ctx, currency)
    if err != nil { return decimal.Zero, err }

    return spot.Mul(decimal.NewFromInt(1).Add(band)), nil
}
```

**Minimum sources:** require at least 2 fresh providers for a median. If only one provider is live, return `ErrInsufficientSpotSources` — matching is paused for that currency until sources recover. Better to pause matching than accept a manipulated single source.

---

## Ceiling Band Configuration

Stored per-currency in a new Ent schema:

```go
// ent/schema/currency_config.go
type CurrencyConfig struct{}
func (CurrencyConfig) Fields() []ent.Field {
    return []ent.Field{
        field.String("currency").Unique(),       // "NGN", "KES", "IDR"
        field.Float("ceiling_band").Default(0.01),
        field.Bool("matching_paused").Default(false),
        field.Time("updated_at"),
        field.String("updated_by"),              // admin user id
    }
}
```

Admin endpoint:
```http
PATCH /admin/v1/currencies/{currency}/ceiling
{ "ceiling_band": 0.015 }   // 1.5%
```

Audit logged. Change takes effect on next matching cycle (no caching beyond Redis pollers).

---

## Matching Engine Integration

Modify `services/priority_queue.go:291` (in `AssignLockPaymentOrder`):

**Before:**
```go
// rate slippage tolerance check
if rate.Sub(order.Rate).Abs().GreaterThan(decimal.NewFromFloat(0.5)) {
    continue
}
```

**After:**
```go
ceiling, err := ceilingrate.GetCeilingRate(ctx, order.FiatCurrency)
if err != nil {
    // Insufficient spot sources — pause matching for this order
    logger.Warnf("ceiling unavailable for %s: %v", order.FiatCurrency, err)
    return // requeue order for retry
}

lpRate, err := GetProviderRate(ctx, provider, order.TokenSymbol)
if err != nil { continue }

if lpRate.GreaterThan(ceiling) {
    continue  // LP's rate exceeds ceiling, skip
}
```

The order's intrinsic `order.Rate` (encoded at create time) is no longer used as the comparison baseline — the ceiling is. This is a semantic change: an order's rate represents what the *sender* committed to receive in fiat terms; the matching engine ensures the *LP* doesn't quote worse than `ceiling`. As long as `lp_rate ≤ ceiling` AND `lp_rate >= order.Rate` (LP can match or beat the sender's expected rate), the match is valid.

---

## Edge Cases

| Case | Behavior |
|---|---|
| All spot providers down | Matching paused for that currency. Orders in queue retry on next cycle. Operators alerted. |
| One provider returning bad data (e.g., decimal point off) | Median dampens its effect. If only 2 providers and one is bad, median = (good+bad)/2 → likely still within ceiling band's tolerance. If 3+ providers, true median ignores outliers. |
| LP's rate is BETTER than ceiling | Always accepted. The ceiling is a maximum, not a target. LPs competing on price is desirable. |
| Spot rate spikes 5% in 30s | First poll after spike pushes ceiling up. LPs with prior rates may now look favorable; matching proceeds. If LPs haven't updated rates, they win the trade at old rates. (Acceptable — LPs accept rate risk.) |
| Currency added but no providers configured yet | `GetSpotMedian` returns `ErrInsufficientSpotSources`. Matching disabled. Operator must wire up providers before going live. |

---

## Governance Override

Per-currency emergency override via admin endpoint (above). Use cases:
- Volatile event (e.g., currency devaluation): widen band to keep flow going.
- Market manipulation suspected: pause matching entirely (`matching_paused = true`).
- Long-tail currency without enough sources: temporarily widen band to allow single-provider matching (requires unsetting min-2-sources rule via separate flag — `single_source_allowed`).

All overrides logged with `updated_by` for audit.

---

## Tasks Changes

Extend pollers in `tasks/tasks.go:919`:

```go
// Existing:
// - PollBinanceP2P (writes to Postgres FiatCurrency.market_rate)
// - PollQuidax (writes to Postgres)

// Replace with:
// - PollBinanceP2P → writes to Redis (spot:{currency}:binance)
// - PollQuidax → writes to Redis (spot:{currency}:quidax)
// - Add: PollCoinGecko → writes to Redis (spot:{currency}:coingecko)
// - Add: per-market local providers as we onboard markets
```

Postgres `FiatCurrency.market_rate` can be removed once all consumers switch to the Redis median. Migration plan: keep both writes for one release, then drop the column.

---

## Testing

- **Unit:**
  - Median of 3 values, of 4 values (even count), of single value (error).
  - Stale entry rejection.
  - Ceiling math at edge values (band = 0, band = 1).
  - Missing currency config (default band fallback).

- **Integration:**
  - Mock Redis with N providers; verify matching accepts/rejects based on LP rate vs ceiling.
  - Provider going stale mid-test → median recomputes correctly.
  - Currency `matching_paused = true` → matching engine skips orders for that currency.

- **Live shadow run before cutover:**
  - Deploy ceiling engine in shadow mode for 1 week (computes ceiling, logs decision, but does not block matching).
  - Compare what the new ceiling would have rejected vs what the old 0.5 slippage rule rejected.
  - Tune band per currency if needed before flipping to enforcing mode.

---

## Open Items

- **Median weighting:** Should Binance P2P count more than CoinGecko (since it's transacted, not just listed)? v1: equal weight. Add weights if v1 data shows skew.
- **Volume-aware ceilings:** Big orders may need wider bands (LPs price in execution risk). v1: flat band per currency. Add volume-tiered bands if needed.
- **Multi-coin pricing:** Spot is fetched per fiat currency, but expressed against which coin? v1 assumes USDC = $1 stable; if USDT depegs, ceiling math is wrong. Track per-coin peg via a separate stablecoin oracle.
