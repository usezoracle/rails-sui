# 2026-05-25 — Route A deposit stuck at receive address

**Severity:** P1 (user funds stranded; manually recoverable).
**Status:** Resolved by manual refund + safety-net patches (see "Fix" below).

## What happened (timeline)

| Time (UTC+1)     | Event                                                                                                                                                                                                                                                                                                                              |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 21:52:00         | User `0x75fb42b6…` initiates payment order `ddfdcc35-7aa6-4507-93bf-7078e80336f1` (1.428571 USDC, mode `lp`, merchant Ogundele Olumide Silas). Rails generates receive address `0xdd5a89f7…` + a `route_a_orders` row in `bridge_status = pending`.                                                                                |
| 21:52:52         | User deposits exactly 1,428,571 µUSDC to the receive address. Tx digest `HEHr4pN9…`, sponsored by aggregator, **status: success**. USDC now owned by `0xdd5a89f7…`.                                                                                                                                                                |
| Some time after  | Rails Sui event indexer **does not pick up** this deposit. `create_order` is never called on the Gateway. The 1.428571 USDC sits at the receive address untouched.                                                                                                                                                                 |
| ~21:54–22:09     | `RouteADispatcher.advancePending` ticks. The `route_a_order` row is still `pending` (because nothing transitions it past `pending` based on actual deposit status — see Root cause 2). The dispatcher requests a LiFi quote (route: Allbridge) for bridging 1,430,000 µUSDC from the aggregator's wallet → Base USDC.               |
| 21:53–22:09 (×N) | Dispatcher submits the LiFi-generated bridge PTB. Sui returns `status: failure, error: InsufficientCoinBalance in command 3` — the PTB tries to peel 1,430,000 µUSDC off the aggregator wallet, which only holds 50,000 µUSDC. Tx reverts atomically; only ~0.000185 SUI in gas is consumed per attempt. No funds move to Allbridge. |
| 22:09:00         | After 15 min of LiFi `/status` returning "not found" (because the tx failed on-chain and was never bridged), `bridgingStaleTimeout` trips. The row is marked `bridge_status = failed`, `failure_reason = "LiFi status returned 'not found' for tx DtrKaEFt… after 15m0s"`.                                                          |
| 22:15            | User screenshots the Tapp PWA showing "Not enough USDC. Your balance is 0.02 USDC" **and** "Payment on chain — notifying merchant…" simultaneously, reports to ops.                                                                                                                                                                |
| (later)          | Manual refund via `scripts/sweep-receive` (sponsored mode) returns 1.428571 USDC to the user.                                                                                                                                                                                                                                       |

## Where the money was at each step

- **User's Tapp wallet:** went from `1.43 + 0.02 = 1.45` USDC to `0.02` USDC after the deposit tx.
- **Receive address `0xdd5a89f7…`:** received 1.428571 USDC at 21:52:52 and **never gave it up**. This is where the money sat through the entire incident.
- **Aggregator `0x396ca7f2…`:** USDC balance unchanged at `0.05`. SUI balance debited only for the failed bridge gas (~0.000185 SUI × number of attempts).
- **Allbridge contracts / LiFi:** nothing. The bridge PTB never executed successfully, so no funds escaped Sui.
- **Base aggregator wallet:** nothing. No bridged USDC ever arrived.

Confirmed via:

- `suix_getAllBalances` on `0xdd5a89f7…` → `1428571 µUSDC, 1 coin object 0x8babc609…`.
- `sui_getTransactionBlock(DtrKaEFt…)` → `status: failure`, `balanceChanges: []` for USDC (only `-184892 MIST` SUI gas).
- `suix_getBalance(aggregator, USDC)` → `50000 µUSDC` (the small coin that was version-bumped by the failed PTB but not balance-changed).

## Root cause (from first principles)

The dispatcher fired a bridge transaction for funds the aggregator did not hold. There are **two** independent failures behind that — fixing either one in isolation would have prevented the incident; we have neither.

### Root cause 1: Deposit watcher missed the deposit

The deposit watcher (`services/sui_deposit_watcher.go::findMatchingDeposit`,
not the event indexer — they're separate) is supposed to:

1. Watch for incoming USDC transfers to receive addresses listed in `sui_receive_addresses`.
2. On detection, mark the row `status = deposited` and trigger `OrderSui.CreateOrder(orderID)`, which calls the Gateway's `create_order` Move function to move USDC into escrow.
3. Wait for the `OrderCreated` event from that Move call, then trigger `OrderSui.SelfSettleToAggregator`, which releases the escrow to the aggregator wallet.

For this deposit, step 1 never fired. The user's deposit tx (`HEHr4pN9…`) was a perfectly normal sponsored coin transfer — should have been within the indexer's detection scope.

**Why? — likely a strict equality / rounding mismatch.** The watcher's match
condition (`sui_deposit_watcher.go:158`) is:

```go
if balance >= addr.ExpectedAmount {
```

For this order, `expected_amount` was computed from `payload.Amount × 10^6`
where `payload.Amount` was the *display-rounded* value from the Tapp PWA
(likely `1.43` → `1,430,000 µUSDC`). The user's deposit was for the
*precise* USDC amount derived from `1500 NGN / 1050 NGN-per-USDC = 1.428571 USDC`
= `1,428,571 µUSDC`. The check is then `1,428,571 >= 1,430,000` → **false** →
silently skipped, never retried.

This is a precision round-trip bug between two layers that compute the
USDC amount independently:
- The Tapp PWA computes the display amount (rounded to 2 decimals).
- The user's pay PTB computes the actual transfer amount (full precision).
- Rails' watcher stores `expected_amount` from the rounded display value.

When the user pays the precise value, the rounded `expected_amount` is
*larger* than what actually lands, so the strict `>=` check fails.

Other plausible contributors we can't rule out without logs from that window:

- WebSocket / RPC reliability on the public fullnode (transient).
- Watcher process restarted mid-window and the catch-up logic missed
  an in-flight tick.
- Filter shape difference (e.g., expecting a single `Coin<USDC>` of
  exact size, got a `SplitCoins`-derived child with a slightly
  different shape).

The watcher is **the only mechanism** that advances orders from "deposit
awaited" to "funds at aggregator." If it fails silently, *nothing else
notices* — until the dispatcher fires the bridge against an empty
aggregator (root cause 2).

**Concrete fixes for root cause 1 (follow-ups, not in this patch):**

- Reconcile `payment_orders.amount` and `sui_receive_addresses.expected_amount`
  to the same source of truth (the precise BigInt subunit), not the
  PWA's rounded display value.
- Add a small detection tolerance in the watcher (e.g., `balance >=
  expected * 0.99`) to absorb future precision drift.
- Add a "late-deposit reconciliation" cron: every 5 min, scan addresses
  in `unused` with `created_at > 2 min ago` and check on-chain balance
  for **any** non-zero amount in the expected coin type. If present,
  alert ops + log a manual-recovery hint (don't auto-advance to
  `deposited` — operator confirms whether the amount is acceptable).

### Root cause 2: Dispatcher has no funding pre-flight

`RouteADispatcher.advancePending` (`services/route_a_dispatcher.go:188`) loads every row in `bridge_status = pending` and fires `startBridge` on it. **It does not verify the aggregator actually holds the USDC the bridge will spend.**

This made root cause 1 visible as a different (and misleading) failure mode. Without this gap, root cause 1 would have been silently caught — the order would stay in `pending` until ops noticed it'd been there too long, but no false bridge attempts, no `failed` status, no "LiFi not found" red herring.

`bridge_status = pending` is set at the moment the row is created (in `controllers/sender/sender.go::InitiateRouteAOrder`), which is *before* the user has even seen the receive address. That state is semantically "exists, awaiting deposit," not "ready to bridge." The dispatcher treats it as the latter.

### Root cause 3 (secondary): timeout + irrevocable failure marking

15 min `bridgingStaleTimeout` is short for Allbridge (typical 20–40 min). Even when the bridge eventually succeeds late, the row is already `failed`. No process picks up the late-arriving Base USDC. Not the trigger for this incident (the bridge tx genuinely failed, not just slow), but it would have masked recovery for a different failure mode where the tx had succeeded.

### Root cause 4 (UX): order page state machine race

The user's screenshot shows three mutually-exclusive states rendered at once: "insufficient balance," "payment on chain," and the loading spinner. This is purely a UI bug — the state machine isn't gated on a single source of truth (the SSE stream updates one flag, the wallet poll updates another, neither cleans up the others). Not a money-loss vector but it makes incident reports harder to triage.

## The design assumption that broke

Implicit in the original Route A pipeline: **every state transition has exactly one writer, and that writer is reliable.** Specifically the indexer is the sole writer of "deposit observed → funds at aggregator." When it fails, the system has no second source of truth.

The dispatcher *also* implicitly assumes the indexer ran — by treating `pending` as "ready to bridge" rather than "awaiting deposit." This compounded a recoverable indexer miss into a bridge attempt + a `failed` status that hides the user's funds from ops dashboards.

We need redundancy at every step where money state is observed, not just where it is written.

## Fix

Refund + three patches:

1. **Manual refund (now).** `scripts/sweep-receive/main.go` extended with sponsored-gas mode (aggregator pays gas + co-signs, source wallet needs no SUI of its own). Run for this specific receive address; mark `payment_orders`, `route_a_orders`, `sui_receive_addresses` refunded.

2. **Dispatcher pre-flight balance check.** Before `startBridge` requests a LiFi quote, query `aggregator.balance(po.Token)`. If less than `po.Amount`, log + skip — the next tick retries. Pure additive change; doesn't fix the indexer, but ensures indexer misses don't manifest as cryptic bridge failures with money stuck mid-flight. Five-line patch.

3. **Deposit reconciliation cron.** Every N minutes, scan `sui_receive_addresses` in `unused`/`pending` state, query on-chain USDC balance for each, and if `actual >= expected`, kick off `CreateOrder` directly (catches indexer misses). Logs an alert when this path fires so we can fix the indexer rather than rely on the reconciler permanently.

4. **Stuck-bridging policy change.** Instead of `failed` after 15 min of "not found," move to a new `bridge_status = uncertain`. A separate poller re-checks for 24h, escalates to `failed` only after that. If LiFi eventually returns DONE / FAILED, route normally.

5. **(Out of this incident's scope, but tracked):** Tapp order-page state-machine cleanup. Single source of truth for the pay phase (`compose | waiting_for_chain | sent | declined`), not three flags.

Patches (2) and (3) are in this PR. (4) and (5) are follow-ups tracked separately.

## What to do operationally next time

Until patch (3) ships:

- If a user reports a Route A payment "stuck" or "failed" with their balance debited, **the first place to look is the receive address on Sui**, not the bridge or the aggregator.
  ```
  curl https://fullnode.mainnet.sui.io:443 -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"suix_getAllBalances","params":["<receive_address>"]}'
  ```
- If the USDC is there, run `scripts/sweep-receive` in sponsored mode pointing at the user's wallet.
- If the USDC is *not* there but `sui_receive_addresses.status = unused`, check the Gateway escrow next (an `OrderObject` keyed by the payment order UUID reference field).
- Mark all three rows refunded in the DB before closing the incident.
