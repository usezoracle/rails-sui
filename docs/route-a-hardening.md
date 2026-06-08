# Route A Hardening Spec

> Goal: turn Route A from "happy path works, failures leave funds
> stranded" into "every step is observable, every failure is
> recoverable, no silent dead ends."
>
> Prompted by the 2026-05-25 incident — see
> [docs/incidents/2026-05-25-route-a-stuck-deposit.md](./incidents/2026-05-25-route-a-stuck-deposit.md).

## Where we are today

Route A end-to-end has ~9 distinct steps, each with independent
failure modes:

```
1. User → receive_address          (sponsored Sui transfer)
2. SuiDepositWatcher detects        (cron, strict balance >= expected)
3. OrderSui.CreateOrder              (Move call, USDC → Gateway escrow)
4. SuiEventIndexer sees OrderCreated (WebSocket subscription)
5. OrderSui.SelfSettleToAggregator   (Move call, escrow → aggregator)
6. RouteADispatcher.startBridge      (LiFi quote → Allbridge/Wormhole PTB)
7. Dispatcher polls LiFi /status     (HTTP, 15min stale timeout)
8. Dispatcher.dispatchLP on Base     (approve + createOrder, EVM)
9. Dispatcher.advanceDispatching     (the aggregator /orders polling)
   → settled | refunded
```

External dependencies we don't control: **Sui RPC, Sui WebSocket, LiFi
HTTP API, Allbridge relayer, Base RPC, the aggregator HTTP API, BaaS partner
(treasury mode)**. Any one being slow or wrong can strand a user's
funds today.

## Principles

1. **Every state transition is logged with timestamp + actor + payload.**
   An operator looking at one order's audit trail should be able to
   reconstruct exactly what happened, when, and why.
2. **No silent failures.** Every "skipped," "missed," or "didn't
   match" path writes a row explaining why. Today's strict-match
   watcher skip is invisible — that has to end.
3. **Idempotency everywhere.** Re-running any step is safe. Reference
   IDs / unique constraints prevent double-execution.
4. **Defense in depth.** No single watcher / indexer / dispatcher is
   load-bearing — every advancing step has a reconciler that catches
   misses.
5. **Graceful degradation.** External outage downgrades, not fails. A
   timed-out bridge stays in "uncertain" until a 24h poller resolves
   it; a stuck the aggregator doesn't auto-mark refunded.
6. **Operator override.** Every state can be force-advanced or
   force-refunded via admin tooling with a logged justification.

## Six-phase plan

Each phase is independently shippable. We can stop at any phase and
have an improvement; or run the whole thing.

### Phase 1 — Per-order audit log (highest leverage)

**Why first:** an audit log makes every other phase debuggable. Without
it we keep diagnosing incidents via on-chain spelunking + RPC curl.

**Schema:** new `route_a_events` table (Ent).

```
route_a_events
  id            uuid pk
  order_id      fk → route_a_orders.id (indexed)
  step          enum: deposit_detected, create_order, self_settle,
                       bridge_quote, bridge_submit, bridge_poll,
                       evm_approve, evm_create_order, settlement_poll,
                       refund, manual_override, ...
  status        enum: started, succeeded, failed, skipped, retrying
  actor         enum: watcher, indexer, dispatcher, operator, system
  at            timestamptz (indexed)
  duration_ms   int (nullable; populated on terminal status)
  payload       jsonb (request/response context: tx digests, amounts,
                       quote IDs, upstream HTTP status, balances seen,
                       error class)
  error_msg     text (nullable)
  correlation_id text (request id for log grep)
```

**Code surface:** a single `events.Log(ctx, orderID, step, status, payload)`
helper called at the entry + exit of every step in the watcher /
indexer / dispatcher. Pattern:

```go
defer events.Time(ctx, orderID, events.StepBridgeSubmit)(&err)
```

Returns a closer that writes the started+succeeded/failed rows and
captures duration. Errors are wrapped, not swallowed.

**Observability deliverable:**
- New admin endpoint: `GET /admin/orders/route-a/:id/events` → JSON of
  all events for an order.
- CLI: `go run scripts/order-status/main.go <order_id>` → pretty-printed
  timeline.

**Acceptance:** for any order touching the pipeline post-deploy, an
operator can run one command and see the complete chronological
story without touching the chain.

### Phase 2 — State machine hardening

Document and tighten the bridge_status state machine. Today:

```
pending → bridging → bridged → dispatching → settled | refunded | failed
```

Adds + changes:

- **New state `awaiting_funds`** between `pending` and `bridging`.
  Set by the dispatcher's pre-flight balance check (already
  shipped — `ErrAwaitingDepositAtAggregator`) and the reconciler.
  Distinguishes "deposit not in aggregator yet" from "order genuinely
  ready to bridge." Keeps the dashboard clean.
- **New state `bridge_uncertain`** between `bridging` and
  `failed`. Triggered when LiFi `/status` returns `not found` past
  the per-tool timeout (Allbridge: 60min, Wormhole: 30min, CCTP:
  20min) but before we want to declare it dead. A separate poller
  retries this state for up to 24h.
- **Late-arrival recovery.** When in `bridge_uncertain`, also poll
  the Base aggregator wallet's USDC balance. If we observe an
  incoming USDC transfer matching the bridge's expected amount,
  advance to `bridged` and continue dispatch as normal.
- **Refund as first-class.** Refund flow gets its own state machine
  (`refund_pending`, `refund_executing`, `refunded` / `refund_failed`)
  so a half-refunded order is visible, not silent.

**Per-tool bridge timeouts** in config:

```go
var bridgeTimeouts = map[string]time.Duration{
    "allbridge":     60 * time.Minute,
    "wormhole":      30 * time.Minute,
    "cctp":          20 * time.Minute,
    "mayan":         15 * time.Minute,
    "":              45 * time.Minute, // unknown / default
}
```

### Phase 3 — Reconciliation crons (no-single-point-of-failure)

Three independent reconcilers, each running every few minutes:

1. **Late-deposit reconciler.** Scans `sui_receive_addresses` in
   `unused` state, queries on-chain USDC balance for each. If any
   non-zero, writes an event row + alerts ops. Does NOT auto-advance
   (operator decides whether the partial amount is acceptable —
   prevents abuse). Catches the precision-mismatch case from the
   incident.
2. **Stuck-order reconciler.** Scans `route_a_orders` whose
   `updated_at < now() - 30min` AND `bridge_status NOT IN
   (settled, refunded, failed)`. Logs an event + alerts.
3. **Aggregator-balance reconciler.** Scans incoming USDC transfers
   to the aggregator wallet (Sui + Base) that aren't matched to a
   `route_a_order.bridge_tx_dest`. Surfaces orphan funds.

All three write to `route_a_events` so the same audit trail
captures reconciler activity.

### Phase 4 — External-service resilience

- **Bridge tool fallback chain.** Today the LiFi quote returns one
  tool (Allbridge if it routes best). On failure, retry the quote
  excluding that tool (LiFi supports `denyBridges=<csv>`). Order:
  Allbridge → Wormhole → CCTP → Mayan.
- **Per-tool circuit breaker.** If a tool fails >N times in a
  rolling window, mark it unhealthy for M minutes and skip it on
  new quotes.
- **Exponential backoff with jitter** on every external HTTP call
  (LiFi, the aggregator, Sui RPC, Base RPC). 3 attempts, base 500ms,
  multiplier 2, jitter ±30%.
- **Health-check endpoint** `/v1/admin/health/upstreams` that
  pings each external service and reports last-known status.

### Phase 5 — Observability + alerts

- **Structured logs** with `order_id` field on every line. Existing
  `logger.Errorf("route-a: ...")` becomes `logger.WithField("order_id",
  o.ID).Errorf(...)`.
- **Prometheus metrics** (or whatever metrics system you use):
  - `route_a_step_duration_seconds{step, status}` (histogram)
  - `route_a_step_total{step, status}` (counter)
  - `route_a_orders_in_state{state}` (gauge, sampled every minute)
  - `route_a_external_call_duration_seconds{service, endpoint, status}` (histogram)
- **Alerts:**
  - Any order in `awaiting_funds` > 10 min
  - Any order in `bridge_uncertain` > 2 h
  - Any order in non-terminal state > 30 min
  - Aggregator low-balance (already exists for Base; add Sui)
  - Per-step failure rate > 10% in the last hour

### Phase 6 — Operator tooling

- **Admin endpoints** (mounted at `/admin/route-a/*`, gated by
  `ADMIN_API_TOKEN`):
  - `GET /orders` — filter by state, time range
  - `GET /orders/:id` — full state + events timeline
  - `POST /orders/:id/retry/:step` — re-run a specific step
  - `POST /orders/:id/refund` — initiate operator-driven refund
  - `POST /orders/:id/force-state` — set state (with required
    justification, written to events)
- **CLI scripts** (the Go scripts in `scripts/`):
  - `order-status <id>` — read-only timeline + balances
  - `refund-order <id>` — pick the right refund path based on
    where funds are (extends the work we did today)
  - `retry-step <id> <step>` — manually re-trigger
- **Runbooks** in `docs/runbooks/` for each common failure
  symptom — what to check, what to run.

## Quick wins already shipped this session

- ✅ Pre-flight aggregator balance check in `startBridge`
  (`ErrAwaitingDepositAtAggregator` sentinel).
- ✅ Sponsored-gas support in `scripts/sweep-receive`.
- ✅ `scripts/refund-prefund` for one-shot SUI top-ups.
- ✅ SDK-bug workaround documented (`tx.Pure(string(addr))`,
  `CallArg::Object` resolved coin refs).

## What I propose we build next

**Phase 1 first** (audit log). Reasons:

1. It pays for itself the first time an order goes sideways — no
   more on-chain spelunking to reconstruct what happened.
2. All later phases depend on having an event stream to write to
   anyway. Building it first means Phases 2–6 just plug into it.
3. Small surface — one new table, one new helper, instrumentation
   touches existing code minimally.
4. Estimate: ~1 day for the schema + helper + dispatcher/watcher
   instrumentation + admin endpoint + CLI tool.

Phase 2 (state-machine + late-arrival recovery) is the second-most-leverage
piece because it eliminates the "marked failed but money still in
flight" class of incidents entirely.

## Open questions to confirm before starting

1. **Metrics stack** — do we have Prometheus / Grafana wired today?
   If not, Phase 5 reduces to structured logs + log-shipper alerts
   (which is fine).
2. **Admin auth model** — `ADMIN_API_TOKEN` is the only gate
   today. Is that enough for write endpoints like force-refund?
   Consider scoping a separate `OPERATOR_API_TOKEN` for write ops.
3. **Database migration tooling** — Ent + golang-migrate? Or
   schema sync via `ent generate`? Affects Phase 1 PR shape.
4. **Per-tool LiFi config** — do we want the bridge-tool fallback
   chain (Phase 4) configurable per environment, or hardcoded by
   priority? Hardcoded is simpler; configurable lets you steer
   away from flaky tools without a deploy.
