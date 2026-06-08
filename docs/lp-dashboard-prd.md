# PRD — Liquidity Provider (LP) Management Dashboard

Status: **DRAFT v1**
Owner: Rails / Decentralized Settlement
Last updated: 2026-06-08
Scope: Web dashboard for liquidity providers on the **decentralized route** — entities given a Naira deposit account who settle stablecoins (USDC) on-chain in return.
Design mandate: **strictly follow the `tapp` design style and philosophy** (see §3). Every visual and motion decision in this document is a non-negotiable instruction unless flagged as an open question in §9.

---

## 1. Context & use case

On the decentralized route, a liquidity provider (LP) is the counterparty that fronts fiat liquidity. **The route is fully automated: the system auto-matches orders and auto-pays Naira *from the LP's delegated, pre-funded the BaaS provider sub-account*. The LP runs no paying infrastructure and never pays by hand. There is no human-in-the-loop order queue.**

1. The LP is provisioned a **dedicated the BaaS provider sub-account** and **funds it with Naira** ("float"). They delegate it to the platform; the rails backend holds org-level the BaaS provider OAuth credentials and is authorized to debit that sub-account. (`docs/mfb-integration.md`; one sub-account per LP)
2. The LP configures what the auto-matcher should send them: settlement wallet(s), per-token rate, order-size range, availability, visibility.
3. A merchant/sender creates an on-chain order. The **matching engine auto-assigns** it to an eligible LP from the Redis provision buckets (eligibility + rate + FIFO) — the LP does not browse or pick orders. (`services/priority_queue.go`)
4. The backend **auto-pays** the recipient's NGN bank account by **debiting the LP's delegated sub-account** via the BaaS provider `name-enquiry → transfer → status` (`services/baas/mfb/client.go:90`). No LP node is required for the payout. (A legacy `POST {host_identifier}/new_order` HMAC notification path exists but is secondary to this debit.)
5. On payout confirmation, the aggregator **settles the LP in USDC** on Sui (escrow release). The LP earns the rate spread between the NGN debited and the USDC received.

**This makes the dashboard a float-management + configuration + monitoring console, NOT a manual order-working queue and NOT a node-ops tool.** The platform does the matching and the paying *from the LP's money*; the LP's job — and the dashboard's center of gravity — is to **keep the delegated sub-account funded** (a dry account = failed transfers = the "Insufficient funds" cancel reason = lost volume), tune the rate/range/availability that steer the matcher, and watch auto-pays and settlements reconcile. The dashboard's jobs: (a) **fund & monitor the Naira float** (the dominant operational concern); (b) **configure** rates, ranges, wallets, availability, visibility; (c) **monitor** auto-pays and watch USDC settlements land in real time; (d) **reconcile** NGN debited vs USDC received and surface failed/insufficient-float payouts; (e) complete KYB/onboarding + manage credentials. The `accept/decline/fulfil/cancel` endpoints are primarily the **machine API**; in the dashboard they appear only as **manual override/exception controls**, not the core loop.

This document does two things:
- **§2** audits the backend to confirm what is and isn't ready for this dashboard.
- **§3–§9** specify the dashboard frontend under the tapp design contract.

---

## 2. Backend readiness assessment

Audit of `/Users/mac/rails` (Go + Gin + ent + Redis + Sui). Verdict per subsystem, with the endpoints the dashboard will bind to.

### 2.1 What is READY ✅

| Subsystem | State | Key files / endpoints |
|---|---|---|
| **LP auth** | JWT (Bearer) + HMAC signature both supported. Register with `scope=["provider"]` auto-creates `ProviderProfile` + `APIKey`. Login / refresh / logout present. Email OTP verification (6-digit) on deployed envs. | `controllers/accounts/auth.go:41`; `routers/middleware/auth.go` (`DynamicAuthMiddleware:354`, `OnlyProviderMiddleware:400`, `HMACVerificationMiddleware:104`) |
| **Provider profile** | `GET /v1/settings/provider`, `PATCH /v1/settings/provider`. Returns tradingName, currency, hostIdentifier, isAvailable, tokens[], apiKey, visibilityMode, KYB fields, isKybVerified. | `controllers/accounts/profile.go:216` |
| **Order lifecycle** | `GET /v1/provider/orders` (paginated, status filter); `POST /orders/:id/accept`, `/decline`, `/fulfill`, `/cancel`. Full state machine: pending → processing → fulfilled → validated → settled (or cancelled/refunded). | `controllers/provider/provider.go:40,139,253,305,488` |
| **Rates** | `GET /v1/provider/rates/:token/:fiat` → marketRate, minimumRate, maximumRate (deviation bands). | `controllers/provider/provider.go:634` |
| **Stats** | `GET /v1/provider/stats` → totalOrders, totalFiatVolume (NGN), totalCryptoVolume (USDC). | `controllers/provider/provider.go:678` |
| **Node health** | `GET /v1/provider/node-info` pings LP's `host_identifier` `/health`. | `controllers/provider/provider.go:738` |
| **On-chain settlement** | `SettleOrder` / `RefundOrder` build sponsored Sui PTBs (aggregator signs, Shinami gas). Triggered async on fulfil/refund. | `services/order/sui.go:127+` |
| **Matching engine** | Redis bucket queues `bucket_{currency}_{min}_{max}`; eligibility = isAvailable && isActive && isKybVerified && visibility=public. FIFO assignment, exclude-list on decline. | `services/priority_queue.go:42-121` |
| **Admin LP mgmt** | `GET /v1/admin/config/providers`, `PATCH /v1/admin/config/providers/:id` (set is_active, is_kyb_verified, safehaven_account_number). Audited. | `controllers/admin/config.go:199` |
| **Domain model** | `ProviderProfile`, `LockPaymentOrder`, `LockOrderFulfillment`, `ProviderOrderToken`, `ProviderRating`, `ProvisionBucket`. | `ent/schema/*.go` |

**Conclusion:** the read + transact spine the dashboard needs (auth, profile, order list, accept/decline/fulfil/cancel, rates, stats) **exists today**. An MVP dashboard can be built against current endpoints.

### 2.2 What is MISSING / incomplete ⚠️ (build alongside or before)

| # | Gap | Impact on dashboard | Evidence | Priority |
|---|---|---|---|---|
| G1 | **No order-assignment push.** Orders land in Redis; LP must poll `GET /orders`. No webhook/SSE. | Dashboard must poll on an interval; "new order" is not real-time. Acceptable for MVP, but a poll cadence + a backend SSE/webhook is the right fix. | `services/priority_queue.go` (Redis `order_request_{id}`, no callback) | High |
| G2 | **the BaaS provider sub-account provisioning is out-of-band, not self-serve.** `CreateSubAccount` + `InitiateIdentity`/`ValidateIdentity` (BVN/NIN + OTP) are implemented but not auto-invoked; 11 LP sub-accounts already exist, provisioned manually. | Provisioning *is* happening, just not in-app. Onboarding shows the issued account as admin-gated status; self-serve provisioning is a fast-follow. | `services/baas/mfb/client.go` (`CreateSubAccount`); `docs/mfb-integration.md` §1 | High |
| G2b | **Auto-pay `Transfer` built but NOT auto-invoked.** The debit-LP-sub-account payout primitive exists and is live-tested for reads, but the hook that fires `Transfer` after on-chain settle is not wired. | The "auto-pay from delegated account" loop is **not live end-to-end yet** — this must ship for the route (and dashboard) to mean anything. The dashboard should reflect real payout state, not assume it. | `services/baas/mfb/client.go:90` (`Transfer` "implemented, not auto-invoked"); `docs/mfb-integration.md` §2 (settle-trigger decision) | **Blocker** |
| G3 | **the BaaS provider settlement/transfer webhook + reconcile poll stubbed.** Inbound payout confirmation handler is a TODO; no `TransferStatus` reconcile cron. | Reconciliation (NGN debited vs USDC received) and payout-failure surfacing will have gaps until wired. | `controllers/mfb_webhook.go:37` (`TODO(route-a/b/c)`); `docs/mfb-integration.md` §2 (reconcile poll) | High |
| G2c | **No LP-facing float-balance read.** the BaaS provider `ListAccounts`/`GetAccount` exist server-side; no endpoint exposes the LP's own sub-account balance to the dashboard. | Balances & Float (§5.5) — the dashboard's most important page — needs a `GET /v1/provider/balance` (or similar). Highest-value small addition. | `services/baas/mfb/client.go` (`GetAccount`) | High |
| G4 | **No automated KYB verification.** Fields stored; admin flips `is_kyb_verified` manually. | Onboarding = "submit docs → wait for admin." Dashboard needs a clear pending/under-review/verified status surface, not an instant flow. | `controllers/accounts/profile.go:283-315` | High |
| G5 | **Rate/range per-token edit is registration-time only.** Explicit TODO to move rate+range management "to dashboard." | Settings → Rates page is a *new contract*: confirm `PATCH /settings/provider` accepts post-hoc token rate edits (it does today via `tokens[]`), but UX must make this first-class. | `controllers/accounts/profile.go:449` (`TODO: ... handled per token in dashboard`) | Medium |
| G6 | **Stats are coarse.** Only totals; no status breakdown, no time series, no fee/commission, no payout-velocity. | Overview charts must either be computed client-side from the orders list or wait on a richer `/stats` (e.g. `?groupBy=status&from=&to=`). MVP: derive from orders; flag for a stats v2. | `controllers/provider/provider.go:678` | Medium |
| G7 | **No LP transaction-history endpoint.** `TransactionLog` exists but isn't exposed per-LP. | "Activity / ledger" page leans on the orders + fulfilment data; a dedicated `/v1/provider/transactions` would be cleaner. | `ent/schema/transactionlog.go` (no GET) | Medium |
| G8 | **No dispute/chargeback workflow.** Cancellation reasons stored; no escalation/resolution. | "Disputes" surface is out of MVP scope; cancellation reasons render read-only. | `controllers/provider/provider.go:488` | Low |
| G9 | **Refund not notified to LP.** Refund releases USDC to user, LP not informed. | Settlement page must derive refund state from order status (`refunded`) rather than an event. | `services/order/sui.go` (`RefundOrder`) | Low |
| G10 | **GetMarketRate USD/token ratio TODO.** Rate doesn't yet apply USD/USDC vs USD/USD ratio. | Rate displays are approximate; surface "indicative" labeling on rate cards. | `controllers/provider/provider.go:653` (`TODO: use token to get the token rate`) | Low |

### 2.3 Verdict

**The LP system is ~60% production-ready and sufficient for an MVP dashboard.** The transactional spine (auth, profile, order accept/decline/fulfil/cancel, settlement, rates, stats) is live. The gaps are concentrated in **real-time push (G1), self-serve onboarding/the BaaS provider (G2–G4), and analytics depth (G6–G7)** — none of which block a first dashboard, but G1–G4 should be on the same milestone so the LP experience isn't "poll-and-wait with an admin in the loop."

**Recommended build order:** (1) ship the MVP dashboard against existing endpoints with polling; (2) in parallel, wire the BaaS provider provisioning + settlement webhook (G2/G3) and an SSE order stream (G1); (3) stats v2 + transactions endpoint (G6/G7).

---

## 3. Design mandate — the tapp contract (STRICT)

> **Every line below is binding.** The dashboard must look and move like it was cut from the same sheet as the `tapp` PWA (which itself mirrors `aggregator/zap`). Sources of truth: `tapp/app/globals.css`, `tapp/lib/motion.ts`, `tapp/docs/motion-guidelines.md`, `tapp/docs/ui-rework-spec.md`. When a token or rule below conflicts with a designer's instinct, the token wins.

### 3.0 The one sanctioned extension

The tapp PWA is a **mobile-first 428px column**. This dashboard is a **desktop operational console** and therefore introduces a **persistent left sidebar** ("side tab") — the *only* structural departure from tapp. The sidebar and every surface inside it are styled **entirely from tapp tokens, type, radii, motion, and component patterns** below. Nothing else is invented. The content area sits to the right of the sidebar inside a comfortable max-width and keeps tapp's `grid gap-6` page rhythm — i.e. the dashboard is "tapp pages, wider, with a tapp-styled nav rail instead of a bottom nav."

### 3.1 Tokens (do not add new ones)

- **Brand accent:** royal blue `#0065F5` (`--color-royal` / `text-blue-600 dark:text-blue-500`). The *only* accent. **No gradients anywhere** (tapp D5).
- **Surfaces:** `bg-white` ↔ `bg-neutral-900` (outer). Subtle fill `bg-gray-50` ↔ `bg-white/5`.
- **Borders:** panels `border-gray-200` ↔ `border-white/10`; controls `border-gray-300` ↔ `border-white/20`.
- **Text:** primary `text-neutral-900` ↔ `text-white`; secondary `text-gray-500` ↔ `text-white/50`; placeholder `text-gray-400` ↔ `text-white/30`.
- **Links/CTAs:** `text-blue-600 dark:text-blue-500`. **Form errors are blue, not red** (`text-blue-500`) — Zap convention. Required marker `text-rose-500`.
- **Status semantics** (icon-tinted only, never a loud filled background): success `#30D158`, danger `#FF453A`, warning `#FF9F0A`.
- **Dark mode:** class-based (`<html class="dark">`), `next-themes` style. Every surface ships both light and dark pairs above.

### 3.2 Type & rhythm

- **Font:** Inter (variable, `--font-sans`), antialiased. `.tabular-nums` for all money/amount/rate figures.
- **Screen titles:** `text-xl font-medium` (Zap weight — **not** `font-bold`/`text-2xl`; tapp D4).
- **Section headings:** `text-xs font-medium uppercase tracking-wider text-gray-400 dark:text-white/30`.
- **Body:** `text-sm`. Help text: `text-xs text-gray-400 dark:text-white/40`.
- **Page rhythm:** `grid gap-6 py-10` (tapp's signature). Header blocks use `space-y-1`.

### 3.3 Radii, surfaces, dividers

- **Radii ladder:** `rounded-full` (chips/pills) → `rounded-xl` (CTAs/inputs) → `rounded-2xl` (sub-panels) → `rounded-3xl` (outer cards + modals). Never deviate.
- **Cards:** `rounded-3xl border border-gray-200 dark:border-white/10`. **No `shadow-sm`, no drop shadows** (tapp D8). Subtle inner tint via `bg-gray-50 dark:bg-white/5`.
- **Dividers:** **dashed, not solid** — `border-dashed border-gray-200 dark:border-white/10`. Detail rows use a dashed `<hr>` between entries; split panels use a dashed vertical rule (`w-px border border-dashed`).
- **Detail rows ("TransactionPreview" pattern):** label-left (`text-gray-500`), value-right (`text-neutral-900 dark:text-white tabular-nums`), dashed separator between.
- **Status chips:** `rounded-full bg-gray-50 px-2 py-1 text-xs dark:bg-white/5` with the semantic color applied to the **icon only** (tapp D12). ID/hash chips: same shape, `font-mono`.
- **Info banners:** `flex gap-2.5 rounded-xl border border-gray-200 bg-gray-50 p-3 dark:bg-white/5` with a leading icon (info = `TbInfoSquareRounded`; warning = amber `PiWarningOctagon`).
- **Buttons:** primary = blue CTA (`active:scale-95`); secondary = outline/ambient (`secondaryBtnClasses` shape). **No true ghost button.** Icons from `react-icons` (`pi`, `fi`, `tb`) — **never `lucide-react`** (tapp D14/5.x).

### 3.4 Motion contract (`lib/motion.ts` + motion-guidelines.md)

Framer Motion. **Single source of truth — no magic numbers in components.**

- **Durations:** `fast 0.16` (press/opacity), `normal 0.24` (page/sheet/modal), `slow 0.36` (multi-step), `countUp 1.0` (amount roll-up).
- **Curves:** `easeOut [0.32,0.72,0,1]` for everything user-triggered (default); `inOut [0.65,0,0.35,1]` only for self-animating loops.
- **Springs:** `default {mass .6,damping 18,stiffness 220}` (sheets), `tight {.4,22,320}` (press), `soft {.8,14,140}` (drags).
- **The Kowalski seven (hard rules):**
  1. Routine transitions **< 300ms**.
  2. `easeOut` for user-triggered motion.
  3. `easeInOut` only for already-moving things.
  4. **Animate `transform` + `opacity` only** — never width/height/layout/padding. Height changes via `scaleY` against a fixed container.
  5. **Never animate repeated actions** (scroll, typing, table re-sort).
  6. Test in slow motion (`?slow=1`).
  7. **Respect `prefers-reduced-motion`** (`useMotionPrefs`) — keep haptics + color shifts, drop travel/scale, springs → instant.
- **Action → reaction (3-tier):** acknowledgement < 100ms (press scale via `<PressableScale>` + haptic), effect 100–400ms (animated state change), resolution (success/error tone). Every tappable surface answers all three.
- **Origin-aware:** modals/popovers grow from their trigger (`transformOrigin`), not screen-center. Sidebar active-item indicator morphs via `layoutId` (the desktop analog of tapp's `layoutId="bn-active"` bottom-nav pill).
- **Continuity — move, don't duplicate:** a logical element present on both sides of a navigation **travels** via `layoutId`, it doesn't fade out + in. Order amount in the table row → order detail headline carries `layoutId="order-amount"`.
- **Numbers:** mount **count-up** for server-driven headlines (balances, volume) via `<CountUp>` (skip < 1); in-place updates animate the **delta**, never re-roll. **Format must be stable** across states (never `72.50 USDC` then `72.5 USDC`).
- **Loading vs action:** first-render data gets a **skeleton** (dimensions match final), not a spinner. Spinners are for *actions* only. Use the `.loader` primitive from `globals.css` for in-flight action states; consolidate — no ad-hoc spinners.
- **Money-flow safety:** the displayed amount must persist visually across a transition; identical formatting at every step; never replace an amount with a spinner during a rate refresh (the amount stays, the rate label below updates); **no celebration before the server confirms** — success motion only after the on-chain digest lands.

### 3.5 The "is this a tapp surface?" checklist (apply per page before merge)

1. Card is `rounded-3xl`, gray-200/white-10 border, **no shadow**?
2. Dividers dashed?
3. Status = chip with icon-only color?
4. Title `text-xl font-medium`, sections uppercase-tracked?
5. Money is `.tabular-nums`, stable format, count-up on mount?
6. Every tappable surface has press scale + haptic?
7. State changes morphed, not swapped (`<CrossFade>`)?
8. Durations < 300ms, `transform/opacity` only, curve from `CURVES.easeOut`?
9. Persistent elements travel via `layoutId`?
10. `prefers-reduced-motion` honored?
11. Icons from `react-icons`, **zero `lucide-react`**?
12. **Zero gradients**, single royal accent?

Three "no"s → not ready.

---

## 4. Information architecture — the side tab (sidebar)

The sidebar is the primary nav. Fixed left, full height, `bg-white dark:bg-neutral-900`, **no right border** (tapp Navbar D2 — separation comes from column padding); items separated by space, not lines. Collapsible to an icon rail on narrow viewports. Active item uses a royal pill that **morphs between items via `layoutId="sidebar-active"`**; light haptic on select. Icons: `react-icons` only.

**Top — brand:** Zoracle wordmark + royal dot (the tapp `Logo`).

**Nav groups** (section labels in `text-xs uppercase tracking-wider text-gray-400 dark:text-white/30`):

```
WORKSPACE
  ◆ Overview          /            home metrics + live order feed
  ◆ Orders            /orders      the work queue (accept/decline/fulfil/cancel)
  ◆ Settlements       /settlements on-chain USDC releases + refunds
  ◆ Activity          /activity    unified ledger (Naira in / USDC out / fees)

LIQUIDITY
  ◆ Balances & Float  /balances    delegated Naira sub-account (fund/monitor) + USDC positions  ← primary
  ◆ Rates & Tokens    /rates        per-token rate/range + settlement wallets
  ◆ Availability      /availability online/offline, visibility, provision buckets

ACCOUNT
  ◆ Onboarding/KYB    /onboarding  KYB status, the BaaS provider account, docs  (badge when action needed)
  ◆ Settings          /settings    profile, API key/HMAC, security, node health
```

**Bottom — context cluster:** availability toggle (Online/Offline pill, see §5.7), theme switch (Pi icons, `p-1.5` pill — tapp D3), and an account menu (trading name, email, sign out rendered as a **secondary** pill, not ghost).

A **global top strip** inside the content area (not a bordered navbar) shows: current page title (`text-xl font-medium`), a **float-health indicator** (green/amber/red — the dashboard's most important always-visible signal, linking to Balances & Float), and a poll-status indicator (G1 — "live" vs "updated 12s ago"). It does not carry a bottom border.

---

## 5. Page-by-page specification

Every page: `<Screen>` content wrapper, `grid gap-6 py-10` rhythm, header block `space-y-1` (title `text-xl font-medium` + subtitle `text-sm text-gray-500 dark:text-white/50`). All cards, chips, dividers, motion per §3. Each section below gives **purpose · content · data source · states**.

### 5.1 Overview (`/`)

- **Purpose:** at-a-glance health + the live order feed; the LP's "is there work, and am I healthy?" screen.
- **Content:**
  - **Hero metric row** — three `rounded-3xl` cards, no shadow, dashed vertical rules between sub-figures: **Settled volume (USDC)**, **Naira deployed (NGN)**, **Orders settled**. Numbers `.tabular-nums`, **count-up on mount**, NGN secondary line kept stable.
  - **Live order feed** — compact list of currently-assigned/pending orders (subset of Orders), each row a `<PressableScale>`; amount carries `layoutId` into the order detail. New rows enter with capped stagger (`delay: index*0.04`, ≤5).
  - **Float + availability strip** — delegated sub-account balance with float-health chip (the headline operational signal), online/offline state, eligibility summary ("Funded · Visible · KYB verified · 3 buckets").
  - **Settlement-in-flight card** — orders mid-flow (paid → `validated` → awaiting `settled`), with a subtle pending chip; resolves to success chip only when the digest lands (no early celebration).
- **Data:** `GET /provider/stats` (G6: derive breakdown client-side from orders for now), `GET /provider/orders?status=...`, `GET /provider/node-info`.
- **States:** skeleton heroes (not spinners) on first load; empty feed → `rounded-3xl` centered empty card (`text-sm text-gray-400`), copy "No orders assigned right now. You're online and eligible." Offline → amber info banner.

### 5.2 Orders (`/orders`)

- **Purpose:** a **monitoring** surface — watch what the LP's node is auto-accepting and auto-paying in real time. **Not a manual work queue.** The primary read is "is my automation flowing, and is anything stuck?" Manual controls exist only as exception/override.
- **Content:**
  - **Status tabs** (`TabButton` chips): All · Pending · Processing · Fulfilled · Validated · Settled · Cancelled. Active tab pill morphs via `layoutId`. A **"Needs attention"** filter surfaces stuck/failed/auto-cancelled orders first — this is the page's real job.
  - **Order table/list** — rows show: amount (USDC, `.tabular-nums`, `layoutId`), NGN value @ rate, recipient bank + account name, status chip (icon-only color), **who acted** (node-auto vs manual), age. Rows are `<PressableScale>`. No animation on re-sort (rule 5). Live rows advance their status chip in place (color morph), not a re-mount — the user watches the node work.
  - **Order detail (sheet/modal, grows from row origin)** — TransactionPreview detail rows (label-left/value-right, dashed `<hr>`): gateway_id, amount, rate, recipient institution/account, account name, memo, and a **status timeline** showing each auto-transition with timestamps (pushed to node → accepted → paid/fulfilled → validated → settled), so a failure is pinpointable.
  - **Manual override controls (exception path only)** — collapsed by default behind a "Manual actions" disclosure, shown prominently only when an order is stuck or the node is offline. Same `grid grid-cols-2 gap-3` shape:
    - **Accept / Decline** (`POST /orders/:id/accept` · `/decline`) — for when the node didn't respond.
    - **Fulfil** (`POST /orders/:id/fulfill`, tx_id + psp + validation_status) — manually report a payout the node couldn't. Money-flow rules apply: amount persists, success tone only after server confirms.
    - **Cancel** (`POST /orders/:id/cancel`, reason) — reason select (Invalid recipient bank details / Insufficient funds / Other); warns when cancellation may trip auto-refund.
  - Copy throughout frames these as overrides ("Your node normally handles this automatically") so the LP isn't misled into thinking manual action is required.
- **Data:** `GET /provider/orders` (paginated, status filter). Poll on interval (G1) with the top-strip "updated Ns ago" indicator; when SSE lands, swap to live.
- **States:** skeleton rows; empty per-tab states; **insufficient-float banner** at top when the delegated sub-account can't cover expected payouts (the dominant failure cause → links to Balances & Float to top up); declined/cancelled rows render reasons read-only (esp. "Insufficient funds").

### 5.3 Settlements (`/settlements`)

- **Purpose:** track USDC escrow releases (and refunds) on Sui — the "did I get paid?" screen.
- **Content:**
  - List of orders in `validated` / `settled` / `refunded`, each with: USDC amount, tx_hash chip (`font-mono`, links to Sui explorer), settlement status chip, timestamp.
  - **In-flight settlements** surfaced first with a pending chip; resolve to success chip + settle pulse on the destination row when the digest lands (continuity: indicator settles here).
  - Refunds (G9) derived from `refunded` status, shown with a neutral/amber chip and the original cancellation reason.
- **Data:** `GET /provider/orders?status=validated|settled|refunded`; settlement aggregator status via existing polling (`services/settlement/client.go`). G3 caveat: until the the BaaS provider webhook is wired, validated→settled may lag — show an honest "awaiting confirmation" pending state, never a fake success.
- **States:** skeleton; empty; explorer-link hover `opacity-70`.

### 5.4 Activity (`/activity`)

- **Purpose:** unified chronological ledger — every Naira-in instruction, USDC-out settlement, cancellation, refund.
- **Content:** single timeline; each row a `<PressableScale>` opening the related order; amount + direction (NGN out to recipient / USDC in from escrow), status chip, dashed separators. Filter chips by type/date.
- **Data:** derived from orders + fulfilments today; **bind to `GET /provider/transactions` when G7 ships** (preferred — cleaner ledger semantics). Note this dependency in the build.
- **States:** skeleton; empty; date-range filter.

### 5.5 Balances & Float (`/balances`) — **the operational heart of the dashboard**

- **Purpose:** keep the delegated Naira sub-account funded and reconcile the two sides of the LP's liquidity. This is the page the LP lives on: a dry float means every matched order's transfer fails. Float health is the dashboard's single most important signal.
- **Content:**
  - **Naira float card** (`rounded-3xl`, hero treatment) — the **available balance of the delegated the BaaS provider sub-account**, `.tabular-nums`, count-up on mount; ledger vs available balance; account number (copyable `font-mono` chip) + account name. A **float-health status chip** (icon-only color): green (healthy), amber (low — below a configurable threshold relative to recent payout velocity), red (depleted/transfers will fail). **Funding instructions** — bank details to top up the sub-account (the LP funds it by bank transfer; the platform does not pull funds). If not yet provisioned, info banner → Onboarding.
  - **Float runway** — an estimate of how long current balance lasts at recent payout velocity (derived from settled-order volume); the practical "do I need to top up?" answer. Honest/indicative labeling (depends on G6).
  - **USDC settlement positions** — per settlement address from `ProviderOrderToken.addresses` (`[{address, network}]`): network label, address chip (`font-mono`), settled-to-date. Count-up on mount.
  - **Reconciliation strip** — NGN debited vs USDC received over the period, with the realized rate spread (the LP's earnings). Split panels use dashed vertical rules; amounts `.tabular-nums`, stable format.
- **Data:** `GET /settings/provider` (mfb account fields + tokens[].addresses); the BaaS provider account balance via the BaaS client (`ListAccounts`/`GetAccount`) — **needs a read endpoint exposed to the LP** (today only admin/server-side); settlement totals from stats/orders. Until the balance read is exposed, show ledger from the last known value with a "as of" timestamp.
- **States:** **low/depleted-float alert is the priority state** — a persistent amber/red banner here and globally (top strip) when the float can't cover expected payouts; un-provisioned account → banner; skeleton balances.

### 5.6 Rates & Tokens (`/rates`)

- **Purpose:** manage per-token conversion rates, order-size range, and settlement wallets — the page the backend explicitly wants to own this (G5).
- **Content:**
  - One **token card** per configured token (e.g. USDC): symbol, **conversion type toggle** (Fixed / Floating `TabButton`), `fixedConversionRate` **or** `floatingConversionRate` field, `minOrderAmount` / `maxOrderAmount` (`LimitField` shape — label-left, value pill right, range below using `accent-blue-600`, help text `text-xs`), and **settlement addresses** per network.
  - **Indicative market rate card** — `GET /rates/:token/:fiat` shows marketRate + min/max deviation bands; label as **indicative** (G10). During refresh the rate label updates **without** replacing the number (money-flow rule).
  - Save via `PATCH /settings/provider` (tokens[]). Validation surfaces as **blue** `<InputError>` (not red); rate-deviation-out-of-band rejected with the band shown.
  - "Add token" grows a new card from the trigger (origin-aware).
- **Data:** `GET /settings/provider`, `GET /rates/:token/:fiat`, `PATCH /settings/provider`.
- **States:** dirty-state save bar; per-field validation; success tone after server confirm.

### 5.7 Availability (`/availability`)

- **Purpose:** control whether the LP receives orders and how they're matched.
- **Content:**
  - **Online/Offline** master toggle (`isAvailable`) — large, with a status chip; medium haptic on commit; the same control mirrored in the sidebar bottom cluster (shared state).
  - **Visibility** (`visibilityMode` Private/Public `TabButton`) — explains: public = in the matching queue; private = excluded.
  - **Provision buckets** — read-only list of the amount-range buckets the LP is assigned to (derived from min/max per token), each a chip with the NGN range. Explains eligibility = available && active && KYB-verified && public.
  - **Eligibility checklist** — green-icon chips for each met condition; amber for unmet (e.g. "KYB pending" → links to Onboarding).
- **Data:** `GET/PATCH /settings/provider`; bucket info from profile/`services/priority_queue` semantics.
- **States:** disabled toggles with explainer when KYB unverified (can't go public until eligible).

### 5.8 Onboarding / KYB (`/onboarding`)

- **Purpose:** get the LP from registered → eligible: KYB docs + Naira deposit account. **Sidebar item carries a badge when action is needed.**
- **Content (stepper — plain page nav for v1, per motion-guidelines §17):**
  1. **Business profile** — trading name, business name, address, mobile, DOB.
  2. **Identity & business docs** — identity_document_type, identity_document, business_document upload.
  3. **Naira deposit account** — the BaaS provider provisioning. **Until G2 is wired this is admin-gated**: show status (Not started / Submitted / **Under review** / Account issued) with an honest info banner ("Your deposit account is being provisioned. This currently completes within X.") and render the issued account number when present. Do **not** fake a self-serve flow that the backend can't fulfil.
  - **KYB status chip** at top: `is_kyb_verified` false → "Under review" (amber icon chip); true → "Verified" (green icon chip).
- **Data:** `PATCH /settings/provider` (KYB fields); `is_kyb_verified` + mfb fields read-only from `GET /settings/provider` (flipped by admin, G4/G2).
- **States:** each step skeleton/saved; verified state collapses the page to a summary; pending state is explicit and non-celebratory.

### 5.9 Settings (`/settings`)

- **Purpose:** account, credentials, security, integration.
- **Content (sectioned cards):**
  - **Profile** — name, email (verified chip), currency.
  - **API access** — API key (reveal/copy), HMAC secret rotation, signing instructions (timestamp + signature window). Treat reveal as a commit action (medium haptic, confirm).
  - **Security** — password change, email-OTP verification status, active refresh-token/sessions (revoke).
  - **Node (optional/legacy)** — `host_identifier` URL + health via `GET /node-info` (green/red dot + last-checked), for LPs who still run the legacy notification node. Secondary: payouts debit the delegated sub-account regardless of node state, so this is informational, not critical.
  - **Appearance** — theme switch (mirror of sidebar).
- **Data:** `GET/PATCH /settings/provider`, `GET /provider/node-info`, auth endpoints for password/session.
- **States:** copy-to-clipboard feedback chips; node-unreachable amber banner.

---

## 6. Cross-cutting requirements

- **Auth & route guarding:** all pages behind LP session (JWT Bearer). On 401, route to sign-in. Respect `OnlyProviderMiddleware` semantics — non-provider users never reach the dashboard. HMAC is for programmatic/server use, not the browser dashboard.
- **Real-time (G1):** poll Orders/Overview on a sane interval (e.g. 10–15s) with a visible "updated Ns ago" indicator; architect the data layer so swapping to SSE/websocket later is a transport change, not a UI rewrite.
- **Empty / loading / error — the tapp way:** first-render data = **skeletons** sized to final content (never spinners). In-flight actions = the `.loader` primitive. Loading↔loaded swaps = `<CrossFade>`. Errors = blue `<InputError>` inline; page-level failures = amber info banner with retry, never a dead screen.
- **Money formatting:** one formatter, `.tabular-nums`, stable decimals per token; NGN and USDC never reformat between states; count-up only on first arrival.
- **Accessibility/motion:** every page honors `prefers-reduced-motion` via `useMotionPrefs` (keep haptics + color, drop travel/scale). 60fps budget — `transform`/`opacity` only.
- **Icons & assets:** `react-icons` (`pi`/`fi`/`tb`) exclusively. **No `lucide-react`, no gradients, no shadows, no aggregator-branded silhouettes** (tapp D14/D15) — ship Sui + USDC marks only.
- **Component reuse:** lift the tapp primitives — `<Button>`, `<PressableScale>`, `<CountUp>`, `<CrossFade>`, `<AnimatedPage>`, status chip, info banner, TransactionPreview detail row, `SelectField`/`TabButton`/`InputError`/`PinInput` — rather than re-authoring. The dashboard is a *consumer* of the tapp design system, not a fork of it.

---

## 7. MVP cut line

**Blocker before the route is real:** wire auto-pay `Transfer` after on-chain settle (**G2b**) + the float-balance read endpoint (**G2c**). The dashboard can be built in parallel, but its core promise ("watch auto-pays debit your float, settlements land") is hollow until G2b ships.

**In (build now against live endpoints):** Sidebar shell · Overview · Orders (monitoring + manual override) · Settlements · Rates & Tokens · Availability · **Balances & Float** (the heart) · Onboarding (status surface) · Settings. Polling for freshness.

**Fast-follow (depends on backend gaps):** SSE order push (G1) · self-serve the BaaS provider sub-account provisioning (G2) · settlement/transfer webhook + reconcile poll (G3) · Activity on `/transactions` (G7) · stats v2 charts incl. float runway (G6).

**Out of MVP:** Disputes/chargebacks (G8) · automated KYB (G4 stays admin-gated) · refund notifications (G9).

---

## 8. Success criteria

- An eligible LP can, in the dashboard alone: **keep their delegated Naira sub-account funded** (see balance, float health, runway, and how to top up), configure the rates/ranges/wallets/availability that steer the auto-matcher, **watch the platform auto-pay from their float and settle them in USDC in real time**, reconcile NGN-out vs USDC-in (their spread), and spot/resolve any payout that failed for insufficient float — with no admin/Postman in the loop (modulo onboarding G2–G4 and the G2b/G2c backend wiring). The dashboard never asks the LP to manually work the happy path; the platform pays from their delegated account automatically.
- A Zap/tapp user recognizes the dashboard instantly: same royal accent, same radii, dashed dividers, icon-only status chips, count-up money, sub-300ms easeOut motion, single column rhythm widened with a tapp-styled rail.
- Zero gradients, zero shadows, zero `lucide-react`, zero magic-number animations — verified against the §3.5 checklist per page.

---

## 9. Open questions / dependencies

1. **Real-time:** ship MVP with polling, or block on SSE (G1)? (Recommended: poll now, SSE fast-follow.)
2. **the BaaS provider self-serve (G2):** can `IdentityInit` (BVN/NIN + OTP) be wired this milestone, or does Onboarding stay admin-gated for v1? Drives whether §5.8 step 3 is interactive or status-only.
3. **Stats depth (G6):** acceptable to derive Overview breakdowns client-side from the orders list for MVP, with a `/stats?groupBy&from&to` to follow?
4. **Transactions endpoint (G7):** confirm a dedicated `GET /v1/provider/transactions` for the Activity ledger vs. deriving from orders.
5. **Rate semantics (G10):** confirm whether the indicative-rate label is acceptable until the USD/token ratio TODO lands.
6. **Dashboard surface:** standalone Next.js app mirroring tapp's stack (App Router + Framer Motion + Tailwind v4 + the tapp `Styles.ts`/`lib/motion.ts`), reusing tapp primitives directly — confirm repo/home for the new frontend.
