# Backend changes for the LP dashboard — rails-native spec

Status: **DRAFT v1**
Last updated: 2026-06-08
Companion to: `docs/lp-dashboard-prd.md`, `docs/mfb-integration.md`
Scope: the Go backend changes required to make the LP dashboard's promises real, **written to match how this codebase already does things** (ent codegen, gin route groups, `u.APIResponse`, stateless service constructors, gocron, viper config). Every change cites the existing pattern it copies.

## Implementation status (2026-06-08)

**Done and building** (`go build ./...` + `go vet` clean):
- **BaaS adapter** — provider-agnostic `services/baas` (interface, neutral types, normalised `TransferStatus`, registry). the BaaS provider now an adapter (`services/baas/mfb/adapter.go`) selected by `BAAS_PROVIDER` in `main.go`. All consumers (admin funding, funding actions, auto-pay, reconcile, balance, webhook dispatch) depend only on `baas.Default()`. **Swapping providers = add one adapter + one `main.go` case.**
- **C1/C2 auto-pay** — `services/order/fiat_payout.go` debits the assigned LP's sub-account (`NameEnquiry → Transfer`, idempotent `routeB-<id>`), hooked as a goroutine after settle commit in `sui_event_indexer.go`. New `LockPaymentOrder` fields (`fiat_payout_{reference,session_id,status,error}`) + `ent` regenerated.
- **C3 webhook** — route-B/C branch converges `fiat_payout_status` (`controllers/mfb_webhook.go`).
- **C4 reconcile** — `tasks.ReconcileFiatPayouts` cron (every 2m) polls `TransferStatus` as the webhook backstop.
- **C5/C6/C7 reads** — `GET /v1/provider/balance` (Naira float + USDC positions); `/stats` extended with status + payout breakdowns; payout status now on the orders list response.

**Not done (deferred, see §4):** C8 order-assignment SSE, C9 self-serve sub-account provisioning. **Migration:** local auto-migrates (`storage/database.go:62`, env=local); **production needs `atlas migrate diff` for the new columns** before deploy.

**Pre-existing, not introduced here:** two `mfb_webhook_test.go` cases fail locally because `.env` sets `ENVIRONMENT=production` and those tests don't override it (they assert the dev path). Verified failing on a clean tree too.

> **Architecture rule for this work:** do not invent new layers. New read endpoints go on the existing `providerRoutes` group; new money-movement logic hooks into the existing Sui settle path and the existing the BaaS provider client; new periodic work is a gocron job in `tasks.StartCronJobs`; new persisted state is an ent field/entity regenerated with `make gen-ent`. Match `u.APIResponse`, the `"provider"` context key, and the `storage.Client` query style verbatim.

---

## 0. What's already built (do NOT rebuild)

The first audit understated this. Confirmed present:

- **the BaaS provider client is wired at boot** — `main.go:40-49` constructs it via `mfb.NewClientFromCredentials(...)` and registers it with `mfb.SetDefault`. Global access: `mfb.Default()` (`services/baas/mfb/setup.go`). Methods `Transfer`, `NameEnquiry`, `TransferStatus`, `GetAccount`, `ListAccounts`, `CreateSubAccount`, `InitiateIdentity`, `ValidateIdentity` all implemented (`services/baas/mfb/client.go`).
- **Webhook route is registered** — `POST /v1/safehaven/webhook` (`routers/index.go:69`), with HMAC-SHA256 signature verify already written (`controllers/mfb_webhook.go:90-95`). Only the route-B/C **branch body** is a TODO (`:81`).
- **Config exists** — `config.MFBConfig()` (`config/mfb.go`) with `ClientID`, `BaseURL`, `WebhookSecret`, `MaxTransferNGN`, debit account, etc.
- **LP sub-account fields exist** — `safehaven_account_number`, `safehaven_account_id` on `ProviderProfile` (`ent/schema/providerprofile.go:54-57`).
- **On-chain settle path exists** — `handleOrderSettled` → `updateOrderStatusSettled` sets `LockPaymentOrder.status=settled`, commits, fires integrator webhook + SSE (`services/sui_event_indexer.go:593-754`).
- **SSE already exists** — `updateOrderStatusSettled` publishes `payment.settled` over SSE; the dashboard can ride this rather than inventing a transport.
- **Per-token rates already editable** — `PATCH /v1/settings/provider` accepts `tokens[]` with rate/range (`controllers/accounts/profile.go`); the dashboard's Rates page is mostly a UI over an existing contract (G5).

So the real backend work is **wiring + a handful of read endpoints + one cron**, not green-field.

---

## 1. Change summary

| # | Change | Type | Maps to PRD gap | Priority | Files |
|---|---|---|---|---|---|
| C1 | Auto-pay: fire the BaaS provider `Transfer` from the LP's sub-account after on-chain settle | wiring | G2b (blocker) | **P0** | `services/sui_event_indexer.go`, new `services/order/fiat_payout.go`, `ent/schema/lockpaymentorder.go` |
| C2 | Persist payout state (sessionId, status, reference, fees, error) | schema | G2b/G3 | **P0** | `ent/schema/lockpaymentorder.go` (+`make gen-ent`) |
| C3 | Implement webhook route-B/C branch (mark payout success/failed, idempotent) | wiring | G3 | **P0** | `controllers/mfb_webhook.go` |
| C4 | Reconcile cron: poll `TransferStatus` for in-flight payouts (webhook backstop) | cron | G3 | **P1** | `tasks/tasks.go` |
| C5 | `GET /v1/provider/balance` — LP's Naira float + USDC positions | endpoint | G2c | **P0** | `controllers/provider/provider.go`, `routers/index.go` |
| C6 | `GET /v1/provider/transactions` — unified ledger | endpoint | G7 | **P1** | `controllers/provider/provider.go`, `routers/index.go` |
| C7 | Extend `GET /v1/provider/stats` — status breakdown, period, float runway | endpoint | G6 | **P1** | `controllers/provider/provider.go` |
| C8 | Provider order-assignment SSE topic (real-time feed) | wiring | G1 | **P2** | wherever assignment happens (`services/priority_queue.go`) + existing SSE publisher |
| C9 | Self-serve sub-account provisioning (Identity → CreateSubAccount) | endpoints | G2 | **P2** | `controllers/provider/` (new onboarding handlers), `routers/index.go` |

---

## 2. P0 — Auto-pay loop (C1 + C2 + C3)

This is the blocker: the route is meaningless until a settled order automatically debits the LP's sub-account.

### 2.1 C2 — Schema: payout state on `LockPaymentOrder`

Follow the existing convention of carrying payout-adjacent fields directly on `LockPaymentOrder` (it already holds `tx_hash`, `institution`, `account_identifier`, `account_name`). Add to `ent/schema/lockpaymentorder.go` `Fields()`:

```go
// the BaaS provider fiat payout (Route B) — set when we debit the LP sub-account.
field.String("fiat_payout_reference").Optional(),          // idempotency key: "routeB-<id>"
field.String("fiat_payout_session_id").Optional(),         // sessionId from Transfer; cleared when terminal
field.Enum("fiat_payout_status").
    Values("none", "pending", "success", "failed").
    Default("none"),
field.String("fiat_payout_error").Optional(),
```

Then **`make gen-ent`** (`Makefile:25` → `go run -mod=mod entgo.io/ent/cmd/ent generate ./ent/schema/`). Never hand-edit generated files. Apply the migration the same way the repo already does (atlas / `seed-db` flow per `atlas.hcl`).

> **Alternative (note, not chosen):** a dedicated `FiatPayout` entity 1:1 with the order, mirroring `LockOrderFulfillment` (`ent/schema/lockorderfulfillment.go`). Cleaner if payouts grow their own lifecycle (retries, partials, multiple attempts). For v1, flat fields are fewer moving parts and easier to debug — preferred. Revisit if payouts need history.

### 2.2 C1 — Service: `FiatPayoutService`

New file `services/order/fiat_payout.go`, following the stateless-constructor pattern (`services/api_key.go:16-22`) and the `mfb.Default()` access pattern. Keep the money-movement logic out of the indexer — the indexer just calls this service (matches how `provider.go:429` calls `orderService.NewOrderSui().SettleOrder`).

```go
package order

type FiatPayoutService struct{}

func NewFiatPayoutService() *FiatPayoutService { return &FiatPayoutService{} }

// PayoutForSettledOrder debits the LP's delegated the BaaS provider sub-account to pay
// the order's recipient. Idempotent on fiat_payout_reference. Never returns an
// error that should roll back the on-chain settle — payout failure is recorded
// on the order and reconciled by webhook/cron, not by failing the settle.
func (s *FiatPayoutService) PayoutForSettledOrder(ctx context.Context, lockOrderID uuid.UUID) {
    sh := mfb.Default()
    if sh == nil {
        logger.Warnf("fiat payout %s: the BaaS provider not configured; skipped", lockOrderID)
        return
    }

    order, err := storage.Client.LockPaymentOrder.Query().
        Where(lockpaymentorder.IDEQ(lockOrderID)).
        WithProvider().
        Only(ctx)
    if err != nil { logger.Errorf("fiat payout %s: load: %v", lockOrderID, err); return }

    // Idempotency: already attempted?
    if order.FiatPayoutStatus != lockpaymentorder.FiatPayoutStatusNone {
        return
    }

    p := order.Edges.Provider
    if p == nil || p.MFBAccountNumber == "" {
        s.mark(ctx, order, "failed", "", "", "provider sub-account not configured")
        return
    }
    if order.Institution == "" || order.AccountIdentifier == "" {
        s.mark(ctx, order, "failed", "", "", "missing recipient bank details")
        return
    }

    ref := fmt.Sprintf("routeB-%s", order.ID)

    // name-enquiry → transfer (the the BaaS provider primitive)
    ne, err := sh.NameEnquiry(ctx, order.Institution, order.AccountIdentifier)
    if err != nil { s.mark(ctx, order, "failed", ref, "", "name enquiry: "+err.Error()); return }

    // amount: NGN payout = crypto amount × rate (see priority_queue.go:347 for the same product)
    amountNGN := order.Amount.Mul(order.Rate).RoundBank(0)

    tr, err := sh.Transfer(ctx, mfb.TransferRequest{
        NameEnquiryReference: ne.SessionID,
        DebitAccountNumber:   p.MFBAccountNumber,
        BeneficiaryBankCode:  order.Institution,
        BeneficiaryAccount:   order.AccountIdentifier,
        Amount:               amountNGN,
        Narration:            fmt.Sprintf("Order %s payout", order.ID),
        PaymentReference:     ref,
    })
    if err != nil { s.mark(ctx, order, "failed", ref, "", "transfer: "+err.Error()); return }

    s.mark(ctx, order, "pending", ref, tr.SessionID, "") // terminal status comes from webhook/cron
}
```

`mark` is a small helper that does `order.Update().SetFiatPayoutStatus(...).SetFiatPayoutReference(...).SetFiatPayoutSessionID(...).SetFiatPayoutError(...).Save(ctx)`.

### 2.3 C1 — Hook point in the indexer

Per the explicit decision in `docs/mfb-integration.md:94-106` (**settle-after-finality** for Route B — LP funds at stake), trigger the payout **after** `updateOrderStatusSettled` commits the `settled` status. In `services/sui_event_indexer.go`, after `tx.Commit()` (≈`:728`) and alongside the existing webhook/SSE fan-out:

```go
// Route B: debit the LP's the BaaS provider sub-account to pay the recipient.
// Async + self-contained: payout failure must not affect on-chain settle state.
go order.NewFiatPayoutService().PayoutForSettledOrder(context.Background(), lockOrder.ID)
```

Rationale (first-principles / debuggability): the on-chain settle is the source of truth; fiat payout is a downstream effect with its own recorded status. Decoupling them (goroutine + status field) means a the BaaS provider outage never corrupts settlement, and every payout's state is inspectable on the order row.

### 2.4 C3 — Webhook route-B/C branch

Replace the TODO at `controllers/mfb_webhook.go:81`. Idempotent, derives the order from the reference:

```go
case strings.HasPrefix(p.PaymentReference, "routeB-"), strings.HasPrefix(p.PaymentReference, "routeC-"):
    id := strings.TrimPrefix(strings.TrimPrefix(p.PaymentReference, "routeB-"), "routeC-")
    oid, err := uuid.Parse(id)
    if err != nil { logger.Warnf("[mfb] bad ref %s", p.PaymentReference); break }

    var status lockpaymentorder.FiatPayoutStatus
    switch p.Status {
    case "success", "completed": status = lockpaymentorder.FiatPayoutStatusSuccess
    case "failed", "rejected", "cancelled": status = lockpaymentorder.FiatPayoutStatusFailed
    default:
        logger.Infof("[mfb] payout pending ref=%s status=%s", p.PaymentReference, p.Status)
        break
    }
    _, err = storage.Client.LockPaymentOrder.Update().
        Where(lockpaymentorder.IDEQ(oid)).
        SetFiatPayoutStatus(status).
        ClearFiatPayoutSessionID().
        Save(ctx)
    if err != nil { logger.Errorf("[mfb] payout update %s: %v", oid, err) }
```

(Handler already reads raw body + verifies signature + parses `baasWebhookPayload` at `:40-87`; only this branch changes.)

---

## 3. P1 — Read endpoints + reconcile (C4, C5, C6, C7)

### 3.1 C5 — `GET /v1/provider/balance`

The dashboard's most important data. New handler on `ProviderController` (`controllers/provider/provider.go`), registered in `providerRoutes` (`routers/index.go:162-177`) as `v1.GET("balance", providerCtrl.GetBalance)` — inherits `DynamicAuthMiddleware` + `OnlyProviderMiddleware` for free.

```go
func (ctrl *ProviderController) GetBalance(ctx *gin.Context) {
    providerCtx, ok := ctx.Get("provider")           // standard pattern (provider.go:52)
    if !ok { u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid API key or token", nil); return }
    provider := providerCtx.(*ent.ProviderProfile)

    resp := types.ProviderBalanceResponse{} // new DTO in types/

    // Naira float from the BaaS provider (read-only GetAccount)
    if sh := mfb.Default(); sh != nil && provider.MFBAccountID != "" {
        if acct, err := sh.GetAccount(ctx, provider.MFBAccountID); err == nil {
            resp.NairaAvailable = acct.AccountBalance
            resp.NairaLedger    = acct.LedgerBalance
            resp.NairaAccountNumber = acct.AccountNumber
            resp.NairaStatus    = acct.Status
        } else {
            logger.Warnf("GetBalance %s: mfb: %v", provider.ID, err)
        }
    }
    // USDC positions: from order_tokens addresses + settled totals (reuse stats query)
    // ... populate resp.SettlementAddresses, resp.SettledUSDC ...

    u.APIResponse(ctx, http.StatusOK, "success", "Balance retrieved", resp)
}
```

Note: don't block the response if the BaaS provider is down — return what you have with the status, matching the indexer's tolerant style.

### 3.2 C6 — `GET /v1/provider/transactions`

Unified ledger. Mirror `GetLockPaymentOrders` exactly (pagination via `u.Paginate`, status filter, `WithProvider().WithToken()`, `types.ProviderLockOrderList`-shaped response) but project payout direction + the new `fiat_payout_status`. Register `v1.GET("transactions", providerCtrl.GetTransactions)`. If a richer ledger than orders is later needed, back it with `TransactionLog` (`ent/schema/transactionlog.go`) — but orders-derived is sufficient and consistent for v1.

### 3.3 C7 — Extend `GET /v1/provider/stats`

Keep the existing totals; add optional `?from=&to=` and a status breakdown computed with grouped counts/sums (ent aggregation, same `storage.Client.LockPaymentOrder.Query()...` style). Add `floatRunwayDays` = current Naira float ÷ trailing daily payout velocity (derive velocity from settled volume in the window). Label indicative (the rate TODO at `provider.go:653` still applies).

### 3.4 C4 — Reconcile cron

Webhook backstop. Add a function in `tasks/tasks.go` and register it in `StartCronJobs()` next to the Route A pollers (`tasks.go:806-822`), every 2 minutes:

```go
func ReconcileFiatPayouts() error {
    ctx := context.Background()
    sh := mfb.Default()
    if sh == nil { return nil }
    orders, err := storage.Client.LockPaymentOrder.Query().
        Where(
            lockpaymentorder.FiatPayoutStatusEQ(lockpaymentorder.FiatPayoutStatusPending),
            lockpaymentorder.FiatPayoutSessionIDNEQ(""),
        ).All(ctx)
    if err != nil { return fmt.Errorf("ReconcileFiatPayouts: %w", err) }
    for _, o := range orders {
        tr, err := sh.TransferStatus(ctx, o.FiatPayoutSessionID)
        if err != nil { logger.Warnf("reconcile %s: %v", o.ID, err); continue }
        switch tr.Status {
        case "success", "completed":
            o.Update().SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusSuccess).ClearFiatPayoutSessionID().Save(ctx)
        case "failed", "rejected", "cancelled":
            o.Update().SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusFailed).ClearFiatPayoutSessionID().Save(ctx)
        }
    }
    return nil
}
```

Register: `scheduler.Cron("*/2 * * * *").Do(ReconcileFiatPayouts)` (cron-string form per `tasks.go:763`, or `scheduler.Every(2).Minute()` form per `:807` — match whichever the neighbouring jobs use).

---

## 4. P2 — Real-time + onboarding (C8, C9)

### 4.1 C8 — Order-assignment SSE

`updateOrderStatusSettled` already publishes SSE; reuse that publisher to emit a `provider.order_assigned` (and `provider.payout_status`) topic scoped to the provider when `sendOrderRequest` assigns an order (`services/priority_queue.go:341-372`). The dashboard subscribes per-provider and drops polling. Keep polling as the fallback so the UI degrades gracefully (matches PRD §6). This is the clean fix for G1 without a new transport.

### 4.2 C9 — Self-serve sub-account provisioning

The methods exist (`CreateSubAccount`, `InitiateIdentity`, `ValidateIdentity`). Add an onboarding sub-resource under the provider group:

- `POST /v1/provider/onboarding/identity/initiate` → `InitiateIdentity` (BVN/NIN; returns verification id + OTP target).
- `POST /v1/provider/onboarding/identity/validate` → `ValidateIdentity` (OTP) → on success `CreateSubAccount`, then persist `safehaven_account_number` + `safehaven_account_id` on the provider (the fields already exist; today admin sets them via `controllers/admin/config.go:199`).

Guard behind `OnlyProviderMiddleware`. Follow the do-not's in `docs/mfb-integration.md:62-90` — these calls have fees/side-effects, so gate carefully and keep the admin path as fallback. Until shipped, Onboarding stays admin-gated (PRD §5.8).

---

## 5. Sequencing & migration

1. **C2 schema + `make gen-ent`** (unblocks C1/C3/C4).
2. **C1 + C3** together (payout fires + webhook confirms) behind the existing `mfb.Default() == nil` guard so it's a no-op until the BaaS provider creds are set — safe to merge dark.
3. **C5** (balance) — highest dashboard value, independent of C1.
4. **C4** reconcile cron once C1/C3 land.
5. **C6/C7** read endpoints.
6. **C8/C9** as fast-follow.

**Codegen/migration:** every schema change (C2) is `make gen-ent` then the repo's migration path (`atlas.hcl` / `seed-db`). Add any new env keys (none strictly required — `MFBConfig` already has what C1 needs) to `.env.example` per `config/mfb.go`.

**Idempotency invariant (debuggability):** the payout is keyed on `routeB-<orderID>`; `fiat_payout_status != none` short-circuits re-attempts; webhook and cron both converge the same row to a terminal status. One order → at most one transfer, and its state is always readable on the order. That single invariant is what keeps money movement safe and inspectable.

---

## 6. Open decisions

1. **Payout amount source of truth** — confirm `order.Amount × order.Rate` (RoundBank 0) is the NGN to pay (matches `priority_queue.go:347`); verify against any fee/spread the platform takes before debiting the LP.
2. **Flat fields vs `FiatPayout` entity** (§2.1) — flat for v1; switch if retries/partials/history are needed.
3. **Dormant sub-accounts** — `docs/mfb-integration.md:§1` flags accounts as `Dormant`; confirm a dormant sub-account can be debited before first live transfer.
4. **Settle-before-finality for Route C** — Route C (float) may want pay-before-finality; this spec keeps Route B after-finality per the doc. Confirm Route C reuses C1 with the main float as debit account.
5. **Balance read frequency** — `GetBalance` calls the BaaS provider live per request; consider a short cache or a periodic snapshot field if the BaaS provider rate-limits.
