# Safe Haven (BaaS) Integration — Infra Map & Build Plan

Safe Haven MFB is our **NGN fiat rail**. It is *not* a liquidity/FX layer — it moves
Naira in and out of the Nigerian banking system. We use it in two places:

| Route | Safe Haven role | Debit account |
|-------|-----------------|---------------|
| **Route B** (decentralized LP) | One **sub-account per LP**; the LP funds it; merchant payout debits *that* LP's sub-account | the LP's sub-account |
| **Route C** (managed liquidity) | We pay merchants directly from our **main account float** | `SAFEHAVEN_DEBIT_ACCOUNT_NUMBER` |

The merchant-payout primitive is identical for both — `name-enquiry → transfer →
status`. The **only** difference is which account number is debited. That single
fact is what keeps this integration simple.

> Route A `mode=lp` stays on Paycrest's EVM Gateway and does **not** touch Safe
> Haven. Route A `mode=treasury` is the same payout primitive as Route C (pay from
> our float after bridging), so it reuses the exact same `Transfer` call.

---

## 1. What is built (done, live-verified)

`services/baas/safehaven/` — self-contained client, no money moved without an
explicit call:

- **`auth.go`** — OAuth2 RS256 client-assertion → token cache + refresh. ✅ live.
- **`client.go`** — authenticated client (`Authorization: Bearer` **and** the
  required `ClientID: <ibs_client_id>` header). Methods:
  - Read-only (safe): `GetBanks`, `ListAccounts`, `GetAccount`, `NameEnquiry`,
    `TransferStatus`. ✅ `GetBanks` (707 banks) and `ListAccounts` live-verified.
  - Money/side-effecting (implemented, **not** auto-invoked): `Transfer`,
    `InitiateIdentity`, `ValidateIdentity`, `CreateSubAccount`.
- **`types.go`** — `Account`, `NameEnquiry`, `TransferRequest`, `Transfer`,
  identity + sub-account types. `Account` shape verified against live JSON.
- **`config/safehaven.go`** + `.env` — credentials + `SAFEHAVEN_DEBIT_ACCOUNT_NUMBER`.

### Live account facts (as observed)
- 3 main accounts under **BLAZE AFRICA LTD**; primary float = `0110890780`
  (others: `…/RENT`, `…/SUDO SETTLEMENT ACCOUNT`). **Confirm which is the Route C
  float** before going live — defaulted to `0110890780`.
- **11 sub-accounts already exist** → LP deposit accounts are already being
  provisioned out-of-band.
- ⚠️ Accounts report `status: "Dormant"` — verify a dormant account can debit, or
  activate it, before the first real transfer.

---

## 2. Infra mapping (where each piece plugs in)

| Need | Existing code | Action |
|------|---------------|--------|
| LP record + onboarding | `ent/schema/providerprofile.go`; `controllers/provider/` | add `safehaven_account_number` (+ optional `safehaven_account_id`) field |
| Recipient bank details | `ent/schema/paymentorderrecipient.go` & `lockpaymentorder.go` (`institution`, `account_identifier`, `account_name`) | `institution` is the CBN/NIP `bankCode` → feeds `NameEnquiry`/`Transfer` directly |
| Route B settle trigger | `services/sui_event_indexer.go` `handleOrderSettled`; `services/priority_queue.go` matching | hook payout **after** on-chain settle (see decision §4) |
| Route A treasury payout | `services/route_a_dispatcher.go` `dispatchTreasury` stub (≈L897); switch in `advanceBridged` (≈L720) | implement stub with `Transfer` from float |
| Webhooks | `routers/index.go` (`kyc/webhook`); `controllers/index.go` `KYCWebhook` w/ signature verify | add `POST /v1/safehaven/webhook` + signature verify |
| Pollers | `tasks/tasks.go` `StartCronJobs`; Route A `Tick` every 1m | add `TransferStatus` reconcile poll |
| Service wiring | `main.go`; `NewRouteADispatcher` (`route_a_dispatcher.go` ≈L143) | construct one `safehaven.Client`, inject |

---

## 3. What to do / not do

### DO (next, low-risk)
1. **Wire construction** — build one `safehaven.Client` at startup, inject into the
   dispatcher and a new payout service. (Mechanical, no money.)
2. **Add `safehaven_account_number` to `ProviderProfile`** (ent field + codegen +
   migration) so Route B knows which sub-account to debit.
3. **Webhook handler** — register `POST /v1/safehaven/webhook`, verify signature,
   flip the order to settled/failed on transfer notification. Mirror `KYCWebhook`.
4. **Idempotency** — derive `TransferRequest.PaymentReference` deterministically
   from the order id (e.g. `routeA-<orderID>` / `routeB-<lockOrderID>`) so retries
   never double-pay. Non-negotiable before any live transfer.
5. **Reconcile poller** — poll `TransferStatus` for in-flight transfers as a
   backstop to the webhook.

### DO NOT (yet)
- **Do not auto-fire `Transfer` on a cron tick** until idempotency + the settle-
  timing decision (§4) are locked. A wired `dispatchTreasury` would otherwise pay
  on the next tick.
- **Do not call `InitiateIdentity`/`CreateSubAccount` casually** — they charge a
  fee and open a *real* bank account against a *real* BVN/NIN. Gate behind explicit
  LP-onboarding intent with consent. (11 sub-accounts already exist — likely don't
  re-create.)
- **Do not rip out Paycrest.** Route A `mode=lp` and the existing Route B matching
  keep working; Safe Haven is added *alongside* as our direct rail, not a rewrite.
- **Do not run live transfers from a `Dormant` account** without confirming it can
  debit.
- **Do not** speculatively change the settlement engine — add hooks, keep diffs
  small and reviewable.

---

## 4. The one real decision: Route B/C settle timing

When do we debit fiat relative to on-chain finality?

- **Settle-after-finality (safe, default):** release fiat only once the Sui escrow
  is confirmed settled to us / the LP. No counterparty risk; slightly slower.
- **Settle-from-float-before-finality (fast, Route C only):** pay the merchant from
  our float immediately, reconcile the crypto async. Fastest UX; we carry the risk
  on a reorg/refund.

Recommendation: **after-finality for Route B** (LP funds are at stake), float
choice configurable for **Route C**. This decision gates DO-item #1's activation.

---

## 5. Open items
- [ ] Confirm Route C float account (`0110890780` vs `…SUDO SETTLEMENT`).
- [ ] Confirm accounts can debit while `Dormant`.
- [ ] Confirm exact request/response field names for `Transfer`/`name-enquiry`/`tqs`
      against a sandbox or a tiny authorized live test (structs are best-effort from
      docs).
- [ ] Get Safe Haven's webhook signature scheme + secret/public key.
- [ ] Decide settle timing (§4).
