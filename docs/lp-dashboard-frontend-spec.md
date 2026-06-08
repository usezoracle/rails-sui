# LP Dashboard — Frontend Engineering & API Integration Spec

Status: **DRAFT v1**
Last updated: 2026-06-08
Companion to: `docs/lp-dashboard-prd.md` (product + design), `docs/lp-dashboard-backend-changes.md` (backend)
Audience: frontend engineers building the LP dashboard.

This doc is the **engineering** counterpart to the PRD. The PRD owns *what to build and how it should look/feel* (the tapp design mandate, page intents, IA). This doc owns *how to build it against the real backend*: the API contract, types, auth, data layer, and page→endpoint wiring. Where this doc says "see PRD §X," that section is binding and not repeated here.

Everything below is grounded in the **actual** Go handlers and response types in this repo (`controllers/provider`, `controllers/accounts`, `types/types.go`). Field names are the real JSON keys.

---

## 1. Stack

Mirror the tapp PWA so the dashboard is a *consumer* of the same design system (PRD §3), not a fork.

| Concern | Choice | Why |
|---|---|---|
| Framework | **Next.js (App Router)** | Same as tapp; SSR-capable, file routing for the sidebar sections |
| Styling | **Tailwind v4** (`@theme` in `globals.css`) | tapp tokens verbatim — `--color-royal: #0065F5`, no `tailwind.config.ts` |
| Motion | **Framer Motion** + `lib/motion.ts` | Port tapp's tokens (durations/curves/springs), `PressableScale`, `CountUp`, `CrossFade`, `AnimatedPage` |
| Data | **TanStack Query (React Query)** | Polling, cache, query invalidation, "updated Ns ago" |
| Light client state | **zustand** | Auth/session, theme, float-health banner |
| Forms | **react-hook-form** + zod | Rates/availability/onboarding forms |
| Icons | **react-icons** (`pi`/`fi`/`tb`) | tapp rule: zero `lucide-react`, zero gradients |
| Charts | lightweight (e.g. visx/recharts) for stats breakdowns | keep flat, royal accent only |

Desktop console shell = **persistent left sidebar** (the one sanctioned departure from tapp's mobile column, PRD §3.0/§4), content in a comfortable max-width with tapp's `grid gap-6 py-10` rhythm.

---

## 2. API foundation

### 2.1 Base URL & envelope

All endpoints return the same envelope (`utils/http.go` `APIResponse` → `types.Response`):

```ts
interface ApiEnvelope<T> {
  status: "success" | "error";
  message: string;
  data: T | null;
}
```

Base URL from env: `NEXT_PUBLIC_API_BASE` (e.g. `https://api.…`). All paths below are appended to it.

### 2.2 Auth

JWT Bearer. The dashboard is a browser client → **always use Bearer JWT** (HMAC is for server/machine callers; the provider routes accept Bearer via `DynamicAuthMiddleware`).

- **Login** → store `accessToken` + `refreshToken`; gate the app on `scopes` including `"provider"`.
- Attach `Authorization: Bearer <accessToken>` to every request.
- On `401`, attempt `POST /v1/auth/refresh` once; on failure, route to sign-in.
- Persist tokens in memory + httpOnly-ish storage per your security bar (refresh token is sensitive).

```ts
// minimal typed fetch
async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    ...init,
    headers: { "Content-Type": "application/json", ...authHeader(), ...init?.headers },
  });
  const body = (await res.json()) as ApiEnvelope<T>;
  if (!res.ok || body.status !== "success") throw new ApiError(res.status, body.message);
  return body.data as T;
}
```

### 2.3 Pagination

`?page=<1-based>&pageSize=<n>` (`utils/http.go` `Paginate`; defaults page=1, pageSize=10). List responses carry `{ total, page, pageSize, orders }`.

---

## 3. API contract reference (real shapes)

> Types below mirror `types/types.go`. Decimals serialize as **strings** (shopspring/decimal) — parse with care; never do float math on money. Timestamps are RFC3339 strings.

### 3.1 Auth — `/v1/auth/*` (public)

| Method | Path | Body | Returns |
|---|---|---|---|
| POST | `/v1/auth/register` | `{ firstName, lastName, email, password, currency, scopes: ["provider"] }` | `RegisterResponse` |
| POST | `/v1/auth/login` | `{ email, password }` | `LoginResponse` |
| POST | `/v1/auth/refresh` | `{ refreshToken }` | tokens |
| POST | `/v1/auth/logout` | `{ refreshToken }` | — |

```ts
interface LoginResponse { accessToken: string; refreshToken: string; scopes: string[]; }
interface RegisterResponse {
  id: string; createdAt: string; updatedAt: string;
  firstName: string; lastName: string; email: string;
  accessToken?: string; refreshToken?: string; // present only on local/dev; prod requires email OTP verify
}
```

### 3.2 Provider — `/v1/provider/*` (Bearer, provider scope)

```ts
type OrderStatus = "pending" | "processing" | "cancelled" | "fulfilled" | "validated" | "settled" | "refunded";
type FiatPayoutStatus = "none" | "pending" | "success" | "failed";

interface LockPaymentOrder {
  id: string; token: string; gatewayId: string;
  amount: string; rate: string;            // decimals as strings; NGN = amount × rate
  blockNumber: number; txHash: string;
  institution: string; accountIdentifier: string; accountName: string;
  providerId: string; memo: string; network: string;
  status: OrderStatus;
  fiatPayoutStatus: FiatPayoutStatus;      // NEW: the Naira leg state
  fiatPayoutError?: string;                // NEW: reason when failed
  updatedAt: string; createdAt: string;
  transactionLogs: TransactionLog[];
}
interface ProviderLockOrderList { total: number; page: number; pageSize: number; orders: LockPaymentOrder[]; }
```

| Method | Path | Query / Body | Returns |
|---|---|---|---|
| GET | `/v1/provider/orders` | `?status=&ordering=asc\|desc&page=&pageSize=` | `ProviderLockOrderList` |
| GET | `/v1/provider/balance` | — | `ProviderBalanceResponse` |
| GET | `/v1/provider/stats` | — | `ProviderStatsResponse` |
| GET | `/v1/provider/rates/:token/:fiat` | path params, e.g. `/rates/USDC/NGN` | `MarketRateResponse` |
| GET | `/v1/provider/node-info` | — | node health passthrough (optional/legacy) |
| POST | `/v1/provider/orders/:id/accept` | — | override only (see §3.5) |
| POST | `/v1/provider/orders/:id/decline` | — | override only |
| POST | `/v1/provider/orders/:id/fulfill` | `{ txId, psp, validationStatus, validationError }` | override only |
| POST | `/v1/provider/orders/:id/cancel` | `{ reason }` | override only |

```ts
// GET /v1/provider/balance  — the dashboard's most important payload
interface ProviderBalanceResponse {
  naira: {
    available: boolean;        // false when rail unconfigured / account unprovisioned / read failed
    accountNumber: string;
    accountName: string;
    balance: string;           // spendable float (decimal string)
    ledgerBalance: string;
    status: string;            // rail account status
    reason?: string;           // present when available=false
  };
  usdc: {
    totalSettled: string;
    wallets: { token: string; network: string; address: string }[];
  };
}

// GET /v1/provider/stats
interface ProviderStatsResponse {
  totalOrders: number;
  totalFiatVolume: string;     // NGN
  totalCryptoVolume: string;   // USDC
  statusBreakdown: Record<OrderStatus, number>;
  fiatPayoutBreakdown: Record<FiatPayoutStatus, number>; // surfaces payouts needing attention
}

// GET /v1/provider/rates/:token/:fiat
interface MarketRateResponse { marketRate: string; minimumRate: string; maximumRate: string; }
```

### 3.3 Settings (provider profile) — `/v1/settings/provider` (Bearer, provider scope)

```ts
interface ProviderOrderToken {
  symbol: string;
  conversionRateType: "fixed" | "floating";
  fixedConversionRate: string;
  floatingConversionRate: string;
  maxOrderAmount: string;
  minOrderAmount: string;
  addresses: { address: string; network: string }[]; // settlement wallets
}

interface ProviderProfile {
  id: string; firstName: string; lastName: string; email: string;
  tradingName: string; currency: string; hostIdentifier: string;
  isAvailable: boolean; isActive: boolean; isKybVerified: boolean;
  tokens: ProviderOrderToken[];
  apiKey: { id: string; secret?: string /* shape per APIKeyResponse */ };
  address: string; mobileNumber: string; dateOfBirth: string;
  businessName: string; identityType: string; identityDocument: string; businessDocument: string;
  visibilityMode: "private" | "public";
}
```

| Method | Path | Body | Returns |
|---|---|---|---|
| GET | `/v1/settings/provider` | — | `ProviderProfile` |
| PATCH | `/v1/settings/provider` | `ProviderProfilePayload` (subset of profile: `tradingName, currency, hostIdentifier, isAvailable, tokens[], visibilityMode, address, mobileNumber, dateOfBirth, businessName, identityDocumentType, identityDocument, businessDocument`) | updated profile |

`PATCH /settings/provider` is the **one write surface** for rates/ranges/wallets (Rates page), availability/visibility (Availability page), and KYB fields (Onboarding). Validation errors come back as `status:"error"` with a `message`; render as a **blue** `<InputError>` (tapp convention).

### 3.4 What the dashboard does NOT need to call

Auto-pay, settlement, matching, and reconciliation are **backend-driven** (`ExecuteOrderService`, the matching engine, the reconcile cron). The dashboard **observes** their results via `fiatPayoutStatus` on orders, the stats breakdowns, and the balance endpoint. There is no "pay" or "settle" button in the happy path.

### 3.5 Override endpoints (exception path only)

`accept/decline/fulfill/cancel` are the machine API the platform/node uses. In the dashboard they appear **only** as collapsed manual-override controls for stuck orders (PRD §5.2), labelled "your liquidity is operated automatically." They accept Bearer JWT. Treat them as commit actions (confirm + medium haptic).

---

## 4. Design system (condensed; full philosophy in PRD §3 — binding)

Port from tapp; do not invent tokens.

- **Accent:** `#0065F5` only. **No gradients, no shadows.**
- **Surfaces:** `bg-white ↔ bg-neutral-900`; subtle `bg-gray-50 ↔ bg-white/5`. Borders `border-gray-200 ↔ border-white/10`.
- **Radii ladder:** `rounded-full → rounded-xl → rounded-2xl → rounded-3xl`.
- **Type:** Inter; titles `text-xl font-medium`; sections `text-xs uppercase tracking-wider text-gray-400`; body `text-sm`; money `.tabular-nums`.
- **Dividers:** dashed. **Status:** chip with icon-only color. **Errors:** blue, not red.
- **Motion:** `lib/motion.ts` tokens; `<PressableScale>` on every tappable; `<CountUp>` for money on mount; `<CrossFade>` for loading↔loaded; skeletons (never spinners) for first-render data; `transform`/`opacity` only; honor `prefers-reduced-motion`; `layoutId` for elements that persist across navigation.

---

## 5. Page → data → component map

Each row: route · primary endpoints · key UI · notes. Page intents/states are in PRD §5.

| Route | Endpoints | Key UI | Notes |
|---|---|---|---|
| `/` Overview | `GET /provider/balance`, `GET /provider/stats`, `GET /provider/orders?status=processing` | Hero metrics (`CountUp`), float-health chip, live order feed | Float health from `balance.naira` vs threshold; settlement-in-flight = orders `status∈{processing,validated}` |
| `/orders` | `GET /provider/orders?status=&page=` | Status tabs (`layoutId`), order table, detail sheet, **"Needs attention"** filter | "Needs attention" = `fiatPayoutStatus==="failed"` OR stuck; show `status` + `fiatPayoutStatus` chips + auto-transition timeline from `transactionLogs` |
| `/settlements` | `GET /provider/orders?status=settled` (+`validated`,`refunded`) | USDC releases, `txHash` → Sui explorer | Pending = `validated`; success = `settled` |
| `/activity` | `GET /provider/orders` (derive ledger) | Unified timeline | Until a `/transactions` endpoint exists, derive from orders (PRD G7) |
| `/balances` **(heart)** | `GET /provider/balance`, `GET /provider/stats` | Naira float hero, float-health chip, runway, top-up details, USDC wallets, NGN↔USDC reconciliation | If `naira.available===false`, show `naira.reason` + link to Onboarding |
| `/rates` | `GET /settings/provider`, `GET /provider/rates/:token/:fiat`, `PATCH /settings/provider` | Per-token cards (fixed/floating toggle, min/max `LimitField`, wallets), indicative market-rate card | Save via `tokens[]`; rate-deviation errors render blue |
| `/availability` | `GET/PATCH /settings/provider` | Online/Offline (`isAvailable`), visibility (`visibilityMode`), eligibility checklist | Eligibility = `isAvailable && isActive && isKybVerified && visibility==="public"` + funded float |
| `/onboarding` | `GET/PATCH /settings/provider` | KYB stepper, status chips | `isKybVerified` + safehaven account status; admin-gated (PRD §5.8 / backend G2) |
| `/settings` | `GET/PATCH /settings/provider`, `GET /provider/node-info` | Profile, API key reveal, security, node (optional/legacy) | API key reveal = commit action |

**Sidebar IA** (PRD §4): Workspace (Overview, Orders, Settlements, Activity) · Liquidity (**Balances & Float** ← primary, Rates & Tokens, Availability) · Account (Onboarding/KYB, Settings). Active item morphs via `layoutId="sidebar-active"`. Global top strip shows page title + **float-health indicator** + poll-status ("updated Ns ago").

---

## 6. Data layer & real-time

- **React Query keys:** `["balance"]`, `["stats"]`, `["orders", {status,page}]`, `["profile"]`, `["rate", token, fiat]`.
- **Polling (backend G1 — no push yet):** Overview + Orders `refetchInterval` ~10–15s; Balance ~30s. Surface "updated Ns ago" from `dataUpdatedAt`. Architect the query layer so swapping to SSE later (backend reuses its existing publisher) is a transport change, not a UI rewrite.
- **Invalidation:** after `PATCH /settings/provider`, invalidate `["profile"]` + `["rate", …]` + `["availability"]`-dependent views.
- **Derived float health:** compute client-side — `green` if `balance.naira.balance ≥ recentPayoutVelocity × buffer`, `amber` near threshold, `red` if below the largest expected order. Velocity from settled volume over a window (stats). Until a richer stats endpoint exists, keep it indicative (label it).

---

## 7. State, money, and error patterns

- **First-render data → skeletons** sized to final content (never spinners). Use `<CrossFade>` for loading↔loaded. Spinners only for *action* in-flight (the tapp `.loader`).
- **Money:** one formatter; parse decimal strings with a decimal lib (e.g. `dinero.js`/`big.js`), never `parseFloat` for arithmetic; `.tabular-nums`; stable formatting across states; `<CountUp>` only on first arrival.
- **Empty:** `rounded-3xl` centered card, `text-sm text-gray-400`, honest copy.
- **Errors:** field-level = blue `<InputError>` from envelope `message`; page-level = amber info banner with retry; `401` → refresh-once → sign-in.
- **The dominant alert:** insufficient/low float — persistent amber/red banner on Balances and in the global top strip when `naira.balance` can't cover expected payouts (this is the LP's main failure mode, backend-confirmed).

---

## 8. Suggested structure

```
app/
  (auth)/sign-in/
  (dashboard)/
    layout.tsx            // sidebar shell + top strip + providers
    page.tsx              // Overview
    orders/  settlements/  activity/
    balances/  rates/  availability/
    onboarding/  settings/
lib/
  api.ts                 // typed fetch + envelope unwrap + refresh
  motion.ts              // ported tapp tokens/hooks
  format.ts              // money/decimal formatting
  queries/               // React Query hooks per resource
components/ui/            // ported tapp primitives: Button, PressableScale, CountUp, CrossFade, StatusChip, InfoBanner, DetailRow, TabButton, InputError, LimitField, Sidebar
types/api.ts             // the interfaces in §3 (single source on the FE)
```

---

## 9. Build sequence

1. **Foundation:** stack, `lib/api.ts` (+ envelope/refresh), `types/api.ts` (§3), ported tapp primitives + tokens, sidebar shell + auth gate.
2. **Balances & Float** (the heart) + **Overview** — highest value, depends only on `balance`/`stats`/`orders`.
3. **Orders** (monitoring + detail + override) + **Settlements** — payout/settlement visibility via `fiatPayoutStatus`.
4. **Rates & Tokens** + **Availability** — the `PATCH /settings/provider` write surface.
5. **Onboarding** (status) + **Settings**.
6. **Activity** + polling polish + (later) SSE swap.

---

## 10. Backend dependencies (track)

- **Live now:** `/provider/orders` (with `fiatPayoutStatus`), `/provider/balance`, `/provider/stats` (with breakdowns), `/provider/rates`, `/settings/provider`, auth. Auto-pay + settlement are backend-driven and reflected in the data.
- **Pending (PRD/backend-changes):** SSE push (G1) → drop polling; `/provider/transactions` (G7) → first-class Activity ledger; self-serve Safe Haven onboarding (G2) → interactive `/onboarding` step 3; production Atlas migration for the payout columns. Build against the contract in §3; these change *fidelity*, not shape.

---

# Part B — Product & Design Specification

This part is the concrete, buildable design spec: component anatomy with exact tapp Tailwind classes, page layouts, states, motion, and microcopy. It operationalizes the PRD's philosophy (PRD §3 remains the binding source for *why*). Class names are literal — copy them. Light/dark pairs are written `light ↔ dark`.

## 11. Design foundations (full token reference)

### 11.1 Color tokens

| Role | Light | Dark |
|---|---|---|
| Page bg | `bg-white` | `bg-neutral-900` |
| Card / panel | `bg-white` | `bg-neutral-900` |
| Subtle fill | `bg-gray-50` | `bg-white/5` |
| Panel border | `border-gray-200` | `border-white/10` |
| Control border | `border-gray-300` | `border-white/20` |
| Text primary | `text-neutral-900` | `text-white` |
| Text secondary | `text-gray-500` | `text-white/50` |
| Text muted/help | `text-gray-400` | `text-white/40` |
| Accent (links/CTA) | `text-blue-600` | `text-blue-500` |
| Form error | `text-blue-500` (yes, blue) | `text-blue-500` |
| Required marker | `text-rose-500` | `text-rose-500` |

Semantic (icon tint only, never a loud fill): success `#30D158`, danger `#FF453A`, warning `#FF9F0A`. Accent `#0065F5` (`text-royal`/`bg-royal` via `--color-royal`). **No gradients. No shadows.**

### 11.2 Type scale

| Use | Classes |
|---|---|
| Page title | `text-xl font-medium tracking-tight` |
| Hero number | `text-3xl font-semibold tabular-nums` (count-up on mount) |
| Section label | `text-xs font-medium uppercase tracking-wider text-gray-400 dark:text-white/30` |
| Body | `text-sm` |
| Help / caption | `text-xs text-gray-400 dark:text-white/40` |
| Mono (ids/hashes/accounts) | `font-mono text-xs` |
| Money | always add `tabular-nums` |

### 11.3 Spacing, radii, dividers

- Page rhythm: `grid gap-6 py-10`. Header block: `space-y-1`. Card padding: `p-4` (sub) / `p-6` (hero).
- Radii: `rounded-full` (chips) → `rounded-xl` (inputs/CTA) → `rounded-2xl` (sub-panels) → `rounded-3xl` (cards/modals).
- Dividers: **dashed** — horizontal `border-t border-dashed border-gray-200 dark:border-white/10`; vertical split `w-px border-l border-dashed border-gray-200 dark:border-white/10`.

### 11.4 Motion defaults (`lib/motion.ts`, ported from tapp)

`DURATIONS {fast .16, normal .24, slow .36, countUp 1.0}` · `CURVES.easeOut [0.32,0.72,0,1]` · `SPRINGS.tight {mass .4, damping 22, stiffness 320}`. Page entry `y:12→0→-12` opacity, 0.24s easeOut. `transform`/`opacity` only. Honor `prefers-reduced-motion` (keep color shifts, drop travel/scale).

---

## 12. Component specifications

Each: **anatomy → classes → states → motion**.

### 12.1 Sidebar (`<Sidebar>`)
- **Anatomy:** brand (Zoracle wordmark + royal dot) → nav groups (Workspace / Liquidity / Account) → bottom cluster (availability toggle, theme switch, account menu).
- **Container:** `flex h-screen w-64 flex-col bg-white px-3 py-5 dark:bg-neutral-900` — **no right border** (separation by padding).
- **Group label:** §11.2 section label, `px-3 pb-1 pt-4`.
- **Nav item:** `flex items-center gap-3 rounded-xl px-3 py-2 text-sm text-gray-500 transition-colors hover:text-neutral-900 dark:text-white/50 dark:hover:text-white`. Icon `size-[18px]` from `react-icons`.
- **Active item:** royal pill behind via `layoutId="sidebar-active"` (`absolute inset-0 rounded-xl bg-blue-600/10`), label `text-blue-600 dark:text-blue-500 font-medium`. Light haptic on select.
- **Badge** (e.g. Onboarding action needed): `ml-auto size-2 rounded-full bg-[#FF9F0A]`.
- **Collapsed (narrow):** icon-rail `w-16`, labels hidden, tooltip on hover.
- **Motion:** active pill morphs (spring `tight`); item press = `<PressableScale>`.

### 12.2 Top strip (`<TopBar>`)
- `flex items-center justify-between pb-6` — **no bottom border**.
- Left: page title (§11.2). Right cluster `gap-3`: **float-health indicator** (§12.4) + poll-status (`text-xs text-gray-400` "updated 12s ago", refreshes from `dataUpdatedAt`).

### 12.3 Card / Hero metric (`<Card>`, `<MetricHero>`)
- **Card:** `rounded-3xl border border-gray-200 p-6 dark:border-white/10` (no shadow). Subtle variant adds `bg-gray-50 dark:bg-white/5`.
- **MetricHero:** label (section label) + value (`text-3xl font-semibold tabular-nums`, `<CountUp>` on mount) + secondary line (`text-sm text-gray-500`). Multi-figure heroes split with a **dashed vertical rule**.

### 12.4 Float-health chip (`<FloatHealth>`) — the signature signal
Drives Balances + top strip. Computed client-side (§6).
- **Shape:** `inline-flex items-center gap-1.5 rounded-full bg-gray-50 px-2 py-1 text-xs dark:bg-white/5`.
- **States** (icon-only color, label text neutral):
  - `green` — `PiCheckCircleFill` `#30D158`, label "Healthy".
  - `amber` — `PiWarningCircleFill` `#FF9F0A`, label "Low float".
  - `red` — `PiXCircleFill` `#FF453A`, label "Depleted".
- **Motion:** state change animates `color` only (200ms easeOut), never position.

### 12.5 Status chip (`<StatusChip>`)
- `inline-flex items-center gap-1.5 rounded-full bg-gray-50 px-2 py-1 text-xs dark:bg-white/5`; semantic color on **icon only**; label is plain text.
- Order/payout mappings in §14. Icon swap (e.g. clock→check) cross-fades via `<AnimatePresence mode="wait">`.

### 12.6 Detail row (`<DetailRow>`) — TransactionPreview pattern
- `flex items-center justify-between py-2.5`: label `text-sm text-gray-500`, value `text-sm text-neutral-900 dark:text-white tabular-nums` (mono for ids/hashes). Dashed `<hr>` between rows.

### 12.7 Order table & row (`<OrderTable>`, `<OrderRow>`)
- Header row: section-label styling, `px-4 py-2`. Body rows wrapped in `<PressableScale>`, `grid grid-cols-[…] items-center gap-4 rounded-2xl px-4 py-3 hover:bg-gray-50 dark:hover:bg-white/5`.
- Columns: amount (USDC `tabular-nums`, `layoutId={`order-amount-${id}`}`) · NGN @ rate · recipient (bank + account name) · `<StatusChip status>` · `<StatusChip payout>` · age (`text-gray-400`).
- **No animation on re-sort** (rule 5). New rows stagger-in capped at 5 (`delay: i*0.04`).

### 12.8 Order detail sheet (`<OrderSheet>`)
- Headless UI panel, grows from row origin (`transformOrigin`), `rounded-3xl`, backdrop `bg-black/25 backdrop-blur-sm`, 240ms easeOut.
- Body: `<DetailRow>`s (gatewayId, amount, rate, institution, accountIdentifier, accountName, memo) + **status timeline** built from `transactionLogs` (each transition + timestamp). Amount carries the same `layoutId` from the row.
- **Manual override** under a collapsed `<Disclosure>` "Manual actions" — `grid grid-cols-2 gap-3`, copy: *"Your liquidity is operated automatically — use these only if an order is stuck."* Buttons = commit actions (confirm + `useHaptic().medium()`).

### 12.9 Info banner (`<InfoBanner variant>`)
- `flex gap-2.5 rounded-xl border p-3 text-sm`. Variants: info `border-gray-200 bg-gray-50 dark:bg-white/5` + `TbInfoSquareRounded`; warning amber `border-[#FF9F0A]/30 bg-[#FF9F0A]/5` + `PiWarningOctagon`. Leading icon `size-5` semantic-tinted.

### 12.10 Empty state (`<Empty>`)
- `rounded-3xl border border-gray-200 bg-gray-50 p-8 text-center text-sm text-gray-400 dark:border-white/10 dark:bg-white/5 dark:text-white/40`. Optional leading icon + one-line copy + optional secondary CTA.

### 12.11 Forms — `<LimitField>`, `<TabButton>`, `<InputError>`, `<Field>`
- **Field/input:** `w-full rounded-xl border border-gray-300 bg-transparent px-3 py-2 text-sm focus:border-blue-600 focus:outline-none dark:border-white/20`. Label section-style; required `text-rose-500 *`.
- **LimitField** (rates min/max): label-left, current value pill right (`rounded-full bg-gray-50 px-2 py-1 text-xs dark:bg-white/5`), range below using `accent-blue-600`, help `text-xs text-gray-400`.
- **TabButton** (status tabs, fixed/floating): pill group; active pill morphs via `layoutId`, active `text-blue-600`, inactive `text-gray-500`.
- **InputError:** `text-xs text-blue-500` (blue, not red) — message from envelope.

### 12.12 Buttons (`<Button variant>`)
- **primary:** `rounded-xl bg-blue-600 px-4 py-2 text-sm font-medium text-white active:scale-95`.
- **secondary:** `rounded-xl border border-gray-300 px-4 py-2 text-sm dark:border-white/20` (the ambient action — no true ghost).
- Every tappable surface wraps `<PressableScale>` + haptic tier (light nav, medium commit, success/error resolution).

### 12.13 Loading — skeleton vs spinner
- **Data first-render → skeleton:** `animate-pulse rounded-2xl bg-gray-100 dark:bg-white/5`, dimensions matching final content. Hero gets a skeleton block, never a spinner.
- **Action in-flight → `.loader`** primitive (the tapp royal CSS loader from `globals.css`).
- Loading↔loaded swap via `<CrossFade>` (`AnimatePresence mode="wait"`).

---

## 13. Page design specifications

Layout = `<Screen>` (max-w content) + `grid gap-6 py-10`. Each page: **header → sections** with exact composition, **states**, **copy**, **motion**.

### 13.1 Overview (`/`)
- **Header:** title "Overview", subtitle "Your liquidity at a glance."
- **Row 1 — three `<MetricHero>` cards** (`grid grid-cols-3 gap-6`): Settled volume (USDC), Naira deployed (NGN), Orders settled. Count-up; NGN secondary stable.
- **Row 2 — Float + availability strip** (`<Card>`): `<FloatHealth>` + balance figure + Online/Offline state + eligibility summary chips ("Funded · Visible · KYB verified · 3 buckets").
- **Row 3 — Live order feed** (`<Card>`): compact `<OrderRow>` list of `status∈{processing,validated}`; each opens `<OrderSheet>`; amount travels via `layoutId`. Stagger-in ≤5.
- **States:** skeleton heroes; empty feed → `<Empty>` "No orders right now. You're online and eligible." Offline → amber `<InfoBanner>` "You're offline — orders won't be routed to you."

### 13.2 Orders (`/orders`)
- **Header:** title "Orders" + subtitle "What your liquidity is doing, in real time."
- **Tabs** (`<TabButton>`): All · Pending · Processing · Fulfilled · Validated · Settled · Cancelled · **Needs attention** (filter `fiatPayoutStatus==="failed"` or stuck). Active morphs.
- **`<OrderTable>`** (§12.7) with both status + payout chips. Top: **insufficient-float banner** when relevant (links to Balances).
- **Detail:** `<OrderSheet>` (§12.8) with auto-transition timeline + collapsed override.
- **States:** skeleton rows; per-tab `<Empty>`; "updated Ns ago" in top strip.
- **Copy (override):** "Your node/liquidity normally handles this automatically."

### 13.3 Settlements (`/settlements`)
- **Header:** "Settlements" + "USDC released to your wallets."
- **List:** rows = `status∈{validated,settled,refunded}`: USDC amount, `txHash` mono chip → Sui explorer (`hover:opacity-70`), settlement `<StatusChip>`, timestamp. In-flight (`validated`) first with pending chip → settles to green + settle-pulse on the row when digest lands.
- **States:** skeleton; `<Empty>` "No settlements yet."

### 13.4 Activity (`/activity`)
- **Header:** "Activity" + "Every Naira out, every USDC in."
- **Timeline:** rows (`<PressableScale>`) with direction (NGN out / USDC in), `<StatusChip>`, dashed separators; filter chips by type/date.
- **Note:** derive from orders until `/provider/transactions` (G7).

### 13.5 Balances & Float (`/balances`) — the heart
- **Header:** "Balances & Float" + "Keep your Naira account funded — it pays every order."
- **Naira float card** (hero `<Card>`): `<FloatHealth>` top-right; available balance `text-3xl font-semibold tabular-nums` count-up; ledger vs available; account number (copyable mono chip) + name; **funding instructions** ("Top up by bank transfer to the account above"). If `naira.available===false` → amber `<InfoBanner>` with `naira.reason` + link to Onboarding.
- **Float runway** (`<Card>`): estimated days at recent payout velocity; label "indicative."
- **USDC positions** (`<Card>`): per `usdc.wallets[]` — network label, address mono chip, settled-to-date; total count-up. Split with dashed vertical rules.
- **Reconciliation strip:** NGN debited vs USDC received + realized spread (earnings).
- **Dominant state:** low/depleted float → persistent amber/red banner here **and** in top strip.

### 13.6 Rates & Tokens (`/rates`)
- **Header:** "Rates & Tokens" + "Set the rates and ranges the matcher quotes."
- **Per-token `<Card>`** (one per `tokens[]`): symbol; **Fixed/Floating `<TabButton>`** → `fixedConversionRate` or `floatingConversionRate` field; `<LimitField>` min/max; settlement addresses per network.
- **Indicative market-rate card:** `GET /rates/:token/:fiat` → marketRate + min/max band; label "indicative"; refresh updates the *label*, never replaces the number.
- **Save:** dirty-state save bar; `PATCH /settings/provider` `tokens[]`; out-of-band rate → blue `<InputError>` showing the allowed band. "Add token" grows a new card from the trigger.

### 13.7 Availability (`/availability`)
- **Header:** "Availability" + "Control whether orders route to you."
- **Online/Offline** master toggle (`isAvailable`) with `<StatusChip>`; medium haptic; mirrors sidebar control.
- **Visibility** Private/Public `<TabButton>` (`visibilityMode`) with explainer.
- **Provision buckets:** read-only chips of NGN ranges.
- **Eligibility checklist:** green/amber icon chips for `isAvailable`, `isActive`, `isKybVerified`, `visibility==="public"`, **funded float**. Unmet KYB/float link out. Toggles disabled with explainer until eligible.

### 13.8 Onboarding / KYB (`/onboarding`)
- **Header:** "Onboarding" + KYB `<StatusChip>` ("Under review" amber / "Verified" green from `isKybVerified`).
- **Stepper (plain nav v1):** 1) Business profile (tradingName, businessName, address, mobile, DOB) · 2) Identity & business docs (type + document fields) · 3) **Naira deposit account** — admin-gated until backend G2: status (Not started / Submitted / Under review / Issued) + honest `<InfoBanner>`; render issued account number when present.
- **States:** verified collapses to summary; pending is explicit, non-celebratory.

### 13.9 Settings (`/settings`)
- **Header:** "Settings".
- **Sections (`<Card>` each):** Profile (name, email + verified chip, currency) · **API access** (key reveal/copy as commit action, HMAC rotation + signing note) · Security (password, OTP status, sessions/revoke) · **Node (optional/legacy)** — `hostIdentifier` + `GET /node-info` green/red dot, marked informational · Appearance (theme switch).

---

## 14. Status → chip mapping & microcopy

### 14.1 `OrderStatus` → `<StatusChip>`
| status | icon (`react-icons`) | color | label |
|---|---|---|---|
| pending | `PiClockBold` | `#9A9A9A` | Pending |
| processing | `PiArrowsClockwiseBold` | `#0065F5` | Processing |
| fulfilled | `PiCheckBold` | `#0065F5` | Paid |
| validated | `PiCheckBold` | `#0065F5` | Validated |
| settled | `PiCheckCircleFill` | `#30D158` | Settled |
| cancelled | `PiXCircleFill` | `#FF453A` | Cancelled |
| refunded | `PiArrowUUpLeftBold` | `#FF9F0A` | Refunded |

### 14.2 `FiatPayoutStatus` → `<StatusChip>`
| payout | icon | color | label |
|---|---|---|---|
| none | `PiMinusBold` | `#9A9A9A` | — |
| pending | `PiClockBold` | `#FF9F0A` | Paying |
| success | `PiCheckCircleFill` | `#30D158` | Paid |
| failed | `PiWarningCircleFill` | `#FF453A` | Payout failed |

### 14.3 Microcopy principles
- Plain, calm, finance-trustworthy. Numbers do the talking; chips/labels stay short.
- Never celebrate before the server confirms (success motion only after the digest).
- Empty states state the *good* default ("You're online and eligible") not just absence.
- Errors are actionable: say what to do ("Top up your Naira account to keep receiving orders").

---

## 15. Responsive

- **≥1280px:** full sidebar (`w-64`) + multi-column heroes (`grid-cols-3`).
- **1024–1280px:** sidebar collapses to icon rail (`w-16`); heroes `grid-cols-3` → keep, tables scroll-x if needed.
- **<1024px (rare for an ops console):** sidebar becomes a top drawer; heroes stack `grid-cols-1`; the mobile column inherits tapp's 428px rhythm directly.
- Money/figures never reflow format across breakpoints; `tabular-nums` everywhere.
