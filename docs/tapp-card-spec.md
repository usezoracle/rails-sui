# Tapp Card — Linking, PIN auth, and on-chain debit

Status: **DESIGN, AWAITING IMPLEMENTATION SIGN-OFF**
Last updated: 2026-05-22 (rev 2 — HMAC PIN, tiered auth, resync flow, PoC scope)

This spec covers the Tapp Card vertical end-to-end: a blank NTAG215 NFC
card that, once linked to a user's zkLogin wallet, can pay a Tapp
Merchant by tapping the merchant's phone — without the cardholder ever
opening their own phone. Small transactions complete on tap alone;
medium transactions require a PIN on the merchant's screen; large
transactions require a step-up biometric check on the cardholder's own
phone.

The phone-to-phone tap flow (already shipped in `controllers/sender/merchant.go`)
is unchanged. Tap Card is an additive path that lands the same way for
the merchant: a `PaymentOrder` → settled to their saved bank account.

---

## Proof-of-Concept (test the UX before writing the security)

For the first round of physical-card hand-tests, **no custom card-write
code is needed**. The flow is:

1. Buy 10 blank NTAG215 cards.
2. Call `POST /v1/cards/issue-batch { count: 10 }` (admin-scoped) →
   server returns 10 unique opaque-token URLs like
   `https://api.zoracle.com/c/A7B2K9P3`.
3. Open a free NDEF writer app (e.g. NFC Tools), paste each URL into a
   card. ~30 seconds per card.
4. Hand cards to the team. Each tap on a phone opens the URL in
   Safari/Chrome via the OS's built-in NDEF URL handler — no app
   required, no Web NFC needed, works on iOS *and* Android.
5. `GET /c/:token` resolves the token + redirects: unlinked →
   `<PWA_BASE_URL>/link?token=…`; linked + same user logged in →
   `/dashboard/cards/:id`; linked + different user → a "claimed
   already" page.

What PoC validates:
- Does the tap-to-open feel snappy? (NDEF spec promises ~300ms; in
  practice depends on the OS)
- Does the claim flow feel right on the PWA?
- Do users understand "this is now my card"?

What PoC does NOT validate (post-PoC build):
- Per-tap secret-token rotation (needs custom NFC write + password lock)
- PIN protocol
- Transaction debits
- Resync flow

The PoC ent entity is intentionally minimal — just enough to mint
URLs, track claim state, and route the redirect. The full schema below
extends it without breaking anything written to a PoC card.

---

## Architectural decisions (locked)

1. **Custody model: NOT us, per [[project-rails-tapp]].** The card never
   delegates spend authority to Rails directly. Instead the user funds
   a Move object (`CardSpendingCap`) they own; Rails (with
   `AggregatorCap`) can call `debit(cap, …)` against it, but only
   within Move-enforced daily / per-tap limits. User destroys the cap
   any time to reclaim the balance — no Rails action required.
2. **Three-tier auth, Apple-Pay style.** Most transactions are small;
   making every customer punch a PIN kills the speed pitch.
   Thresholds (NGN, configurable per-card):
   - `< ₦2,000` → **no auth.** Tap, debit, done.
   - `₦2,000 – ₦15,000` → **PIN.** Merchant screen shows pad; cardholder
     types 4 digits.
   - `> ₦15,000` → **step-up.** Merchant screen shows QR; cardholder
     scans with their own phone, completes WebAuthn biometric in PWA.
   Daily cap still applies across all tiers as the absolute backstop.
3. **PIN is never sent or stored as a hashable scalar — HMAC challenge-
   response only.** Card stores a 32-byte random secret `K` written at
   linking (protected by NTAG215 password lock). Merchant app computes
   `K' = HMAC(K, PIN)` locally and sends `HMAC(K', server_nonce)` to
   the backend. Server stores only `HMAC(K, "verifier")` and verifies
   without ever holding the PIN or anything derived from it alone. A
   complete server DB leak yields nothing useful for offline cracking
   (PIN candidates × K candidates = 10,000 × 2²⁵⁶ = infeasible).
4. **Per-tap secret-token rotation, gated by the strict state machine.**
   Token rotation only fires inside the success branch of the atomic
   debit transaction: PIN check → limits check → Move debit → THEN
   issue new token. A failed PIN never advances state, so cloned cards
   used by attackers without the PIN cannot burn the legitimate user's
   card. The clone walks away with nothing; the real card stays
   perfectly synced.
5. **Single canonical token per card — no sliding window.** Earlier
   drafts kept a `previous_token` valid for 30s to absorb torn writes;
   dropping it in favor of explicit PWA-driven resync (see "Torn
   writes & resync"). Cleaner state machine, smaller attack surface
   (one valid token at a time, not two).
6. **PWA-only linking flow.** Linking requires a personal phone session
   (zkLogin login, write card sector, sign cap-creation PTB). The
   Tapp Merchant app never participates in linking — keeps the
   merchant trust boundary tight.
7. **Custody model: NOT us, per [[project-rails-tapp]].** The card never
   delegates spend authority to Rails directly. Instead the user funds
   a Move object (`CardSpendingCap`) they own; Rails (with
   `AggregatorCap`) can call `debit(cap, …)` against it, but only
   within Move-enforced daily / per-tap limits. User destroys the cap
   any time to reclaim the balance — no Rails action required.
8. **Daily allowance enforced in Move, not in Rails.** A compromised
   Rails instance with a stolen `AggregatorCap` still cannot drain a
   card past its on-chain daily limit. Off-chain limits in Rails are
   defense-in-depth, not the boundary.

---

## On-chain: `tapp_card` Move module

Sits in `contracts/gateway/sources/` alongside the existing `rails::order`
module. Reuses `AggregatorCap` (already minted at Gateway publish time).

### Structs

```move
/// Per-user spending cap. Owns the funded balance. Only the user can
/// top up / destroy; only the aggregator (with cap) can call debit.
struct CardSpendingCap<phantom T> has key {
    id: UID,
    owner: address,            // zkLogin-derived address
    balance: Balance<T>,       // pre-funded USDC the card can spend
    daily_limit_subunit: u64,  // hard cap per UTC day, in coin subunits
    spent_today_subunit: u64,  // running total, reset on first debit of new day
    day_index: u64,            // floor(epoch_ms / 86_400_000)
    per_tap_limit_subunit: u64,// step-up threshold; debit > this is rejected
    card_uid_hash: vector<u8>, // sha256(card UID) — commits the card identity
    revoked: bool,             // user kill-switch
}

/// Bookkeeping receipt emitted on every debit — Rails indexer
/// listens for these to write TransactionLog + trigger fiat payout.
struct CardDebited has copy, drop {
    cap_id: ID,
    owner: address,
    merchant_recipient: address, // Rails treasury addr for this debit
    amount_subunit: u64,
    fiat_reference: vector<u8>,  // PaymentOrder UUID bytes — correlation key
    timestamp_ms: u64,
}
```

### Entry functions

```move
/// Called by the cardholder during linking (signs via zkLogin in PWA).
public entry fun create_cap<T>(
    funding: Coin<T>,
    daily_limit_subunit: u64,
    per_tap_limit_subunit: u64,
    card_uid_hash: vector<u8>,
    ctx: &mut TxContext
)

/// User tops up additional balance (also zkLogin-signed in PWA).
public entry fun top_up<T>(cap: &mut CardSpendingCap<T>, more: Coin<T>, ctx: &TxContext)

/// User updates limits or pauses the card.
public entry fun update_limits<T>(
    cap: &mut CardSpendingCap<T>,
    new_daily: u64,
    new_per_tap: u64,
    ctx: &TxContext
)
public entry fun set_revoked<T>(cap: &mut CardSpendingCap<T>, revoked: bool, ctx: &TxContext)

/// User destroys the cap and reclaims the remaining balance (kill switch).
public entry fun destroy_and_reclaim<T>(cap: CardSpendingCap<T>, ctx: &mut TxContext)

/// Aggregator-only. Called by Rails after PIN + token verification.
/// Enforces revoke, daily limit, per-tap limit. Emits CardDebited.
public entry fun debit<T>(
    _: &AggregatorCap,
    cap: &mut CardSpendingCap<T>,
    amount_subunit: u64,
    merchant_recipient: address,
    fiat_reference: vector<u8>,
    clock: &Clock,
    ctx: &mut TxContext
)
```

### Invariants (Move-enforced, tested)

- `debit` fails if `revoked = true`
- `debit` fails if `amount > per_tap_limit_subunit`
- `debit` fails if `(spent_today + amount) > daily_limit_subunit`
- Day rollover (epoch_ms / 86_400_000 differs from `day_index`)
  resets `spent_today` to 0 atomically inside `debit`
- Cap can only be destroyed by `cap.owner`

### Move tests (must pass)

- create + debit happy path → balance decreases, `CardDebited` emitted
- debit > per-tap → aborts with `EOverPerTapLimit`
- multiple debits crossing daily limit → 4th aborts with `EOverDailyLimit`
- day rollover → spent_today resets, next debit succeeds
- revoked cap → debit aborts with `ERevoked`
- non-aggregator call → fails ownership check

---

## Off-chain: Rails additions

### Ent schema (full, post-PoC)

`ent/schema/tapp_card.go`:

```go
type TappCard struct{ ent.Schema }

// Fields
//   id (uuid)
//   activation_token (16-char base32, unique) — the opaque token in the
//       URL written to the card. Stays valid only while status=issued;
//       once claimed, the URL still resolves but the "claim" action is
//       closed (returns "already claimed" or "your dashboard" depending
//       on the viewer).
//   status (issued | claimed | live | revoked | locked)
//   user_id (fk → User, nullable) — set when status≥claimed
//   card_uid_hash (sha256 of factory UID, bytes32, nullable)
//       — set when PWA writes K to the card (status flips to live)
//   cap_object_id (Sui object ID of CardSpendingCap, nullable)
//       — set when the create_cap PTB lands on-chain
//   coin_type (e.g. "0x...::usdc::USDC", nullable)
//
//   -- HMAC PIN protocol (no stored PIN, no stored PIN hash) --
//   pin_verifier (32 bytes) — = HMAC(K, "verifier"); used to confirm
//       the merchant app's challenge response without ever holding K
//       or the PIN
//   pin_attempts_remaining (int, default 5)
//   locked_until (time, nullable) — set when attempts hit 0
//
//   -- Single canonical token, no sliding window --
//   current_token_ciphertext (bytes) — AES-GCM(random_token, server_key);
//       what the merchant app writes back to the card sector. Mismatch
//       on read = hard fail, recovery only via PWA resync.
//   token_rotated_at (time)
//
//   -- Cached limits (Move is source of truth; cached for fast pre-check) --
//   daily_limit_subunit (u64)
//   per_tap_limit_subunit (u64)         — PIN threshold, in coin subunits
//   step_up_threshold_subunit (u64)     — above this requires PWA biometric
//   spent_today_subunit (u64)
//   day_index (u64)                      — UTC day rollover marker
//
//   created_at, updated_at (TimeMixin)
//
// Edges
//   user ← Many(User → tapp_cards) Unique() — at most one card per user in v1
//   coin_type column (string) — no FK to Token; Token is per-merchant config
```

### PoC subset (ship now)

For the NFC-Tools hand-test PoC, only a tiny subset of the fields above
is needed. Fields the full schema adds later are nullable / defaulted,
so a PoC card row remains valid after the schema grows.

```go
// PoC-shippable today:
//   id, activation_token, status (issued | claimed), user_id (nullable),
//   created_at, updated_at
```

That's it. No K, no PIN, no cap, no rotation. The PoC just validates
"tap-to-URL → claim flow feels right." Everything else gets added once
the full Move + PWA pipeline is greenlit.

### API endpoints

All under `/v1/cards/` (cardholder-scope, JWT-protected) and the
existing `/v1/sender/me/tap-card` (merchant-scope, unchanged path).

| Method  | Path                                          | Caller    | Purpose |
| ------- | --------------------------------------------- | --------- | --- |
| POST    | `/v1/cards/issue-batch`                       | Admin     | **PoC.** Body `{ count }`. Returns `{ urls: [...] }`. Mints opaque tokens, returns `<API_BASE>/c/<token>` URLs ready to paste into NFC Tools. |
| GET     | `/c/:token`                                   | Public    | **PoC.** Resolves the token + 302 redirects: `issued` → `<PWA>/link?token=…`; `claimed` (same user) → `<PWA>/dashboard/cards/:id`; `claimed` (different user) → `<PWA>/cards/already-claimed`. |
| POST    | `/v1/cards/link/claim`                        | PWA       | Body `{ token }`. PWA-authed (zkLogin JWT). Flips `status=issued → claimed`, binds `user_id`. After this, server issues a `K` write-payload + `card_password` for the PWA to write to the card sector. |
| POST    | `/v1/cards/link/complete`                     | PWA       | Body `{ card_uid_hash, cap_object_id, pin_verifier, tx_digest }`. Server verifies the `tx_digest` confirms a `create_cap` published with matching `card_uid_hash`; persists the row; flips `status=live`. |
| POST    | `/v1/cards/revoke`                            | PWA       | User-initiated kill switch. Submits `set_revoked` PTB; flips local status. |
| POST    | `/v1/cards/top-up`                            | PWA       | Server-side helper that wraps a `top_up` PTB build for the PWA to sign (so the PWA doesn't need full Move SDK). |
| GET     | `/v1/cards/me`                                | PWA       | Returns the user's card status + remaining-daily / per-tap limits + tier thresholds. |
| POST    | `/v1/cards/me/resync`                         | PWA       | Torn-write recovery: returns `{ current_token_ct, card_password, resync_nonce }` for the PWA to write back to the card. |
| POST    | `/v1/cards/me/resync/complete`                | PWA       | Body `{ resync_nonce }`. Confirms the write landed; consumes the nonce. |
| GET     | `/v1/sender/me/tap-card/nonce`                | Merchant  | Body `{ amount, card_uid_hash }`. Returns `{ tier, server_nonce }`. Single-use nonce (60s TTL); tier is `none | pin | step_up`. |
| POST    | `/v1/sender/me/tap-card`                      | Merchant  | **Replaces** the current 501 stub. See "Debit request" below. |
| POST    | `/v1/sender/me/tap-card/:order_id/token-ack`  | Merchant  | Body `{ written: bool }`. Reports whether the new-token write to the card actually landed. |
| GET     | `/v1/sender/me/tap-card/step-up`              | Merchant  | Poll for biometric grant. `?token=…`. Returns 200 when the cardholder's PWA completes the WebAuthn check. |
| POST    | `/v1/admin/cards/:id/recovery`                | Admin     | iOS-cardholder fallback for desynced cards: emails a 6-digit code; pairs with a manual reset. |

### Debit request — `POST /v1/sender/me/tap-card`

**Request:**
```json
{
  "card_uid_hash":     "hex(sha256(UID))",
  "current_token_ct":  "hex(the ciphertext bytes the merchant app read off the card)",
  "amount":            "decimal NGN amount, same shape as /tap",
  "currency":          "NGN",
  "memo":              "optional",
  "server_nonce":      "hex(echoed back from prior GET /v1/sender/me/tap-card/nonce)",
  "pin_response":      "hex(HMAC-SHA256(K', server_nonce)) — null if amount < PIN tier",
  "step_up_token":     "hex(echoed back when re-submitting after step-up grant) — null otherwise"
}
```

The two-step handshake (`GET /nonce` → `POST /tap-card`) makes the PIN
response single-use: any captured `pin_response` is useless because
it's bound to a specific `server_nonce` the server only honors once.

`K'` is derived locally by the merchant app: `K' = HMAC-SHA256(K, PIN)`,
where `K` was read off the card sector during the same NFC session. The
server never sees `K`, `K'`, or the PIN itself — only `pin_response`,
which it verifies by computing `HMAC(pin_verifier, server_nonce)`-class
material using the stored `pin_verifier`. (Exact verifier scheme: see
the "PIN protocol math" appendix at the bottom of this doc.)

**Server pipeline (strict state machine, atomic, all-or-nothing):**

1. Look up card by `card_uid_hash`. 404 → `card_unrecognized`.
2. Status check: must be `live`. Else 409.
3. `server_nonce` consumption: must exist, be < 60s old, not previously
   redeemed. Atomic SQL update marks it consumed; if no row affected,
   reject. Replays die here.
4. Token match: compare `current_token_ct` byte-for-byte to
   `current_token_ciphertext`. **No sliding window — single canonical
   token.** Mismatch → 403 `token_invalid_resync_required` (the
   message that triggers the PWA resync flow on the cardholder side).
   Increment a per-card token-mismatch counter; > 3 in 1h →
   `status=locked` to defend against blind probing.
5. Auth tier:
   - `amount < per_tap_limit_subunit` → no auth required, skip to (6)
   - `amount < step_up_threshold_subunit` → require `pin_response`.
     Verify against `pin_verifier` + `server_nonce`. Mismatch:
     decrement `pin_attempts_remaining`; on 0 → `locked_until = now+24h`,
     `status=locked`.
   - `amount >= step_up_threshold_subunit` → require `step_up_token`.
     If absent → 402 `step_up_required` + issue token. If present →
     verify it was granted by a recent PWA WebAuthn assertion. Mismatch
     → 403.
6. Daily-limit pre-check (off-chain mirror, fast-fail before chain):
   `(spent_today + amount) > daily_limit` → 402 `daily_limit_exceeded`.
   On-chain Move enforces this too; this check is just to avoid wasting
   gas on a request we know will abort.
7. Create `PaymentOrder` row (same shape as `/tap`, recipient from
   merchant's `MerchantBankAccount`). Set `Reference = order.ID`.
8. Submit `tapp_card::debit` PTB with `AggregatorCap`. Move enforces
   limits authoritatively — if it aborts, mark order `failed`, return
   409. **No token rotation if this step fails.**
9. **Only now** rotate token: generate new random plaintext token,
   AES-GCM-encrypt under the per-card server key, replace
   `current_token_ciphertext`, stamp `token_rotated_at`. Return the
   new ciphertext for the merchant app to write to the card.
10. Settled status comes via the existing `OrderSettled` event path
    (Tap Card emits a `CardDebited` event the indexer also listens
    on, but settlement to the merchant rides the Route A / Route B
    pipe just like phone-to-phone — no separate fiat rail).

The atomic guarantee here is what closes the clone-burn attack: if any
step before (9) fails, the token never rotates, the legit card stays
in sync. A cloned card without the PIN dies at step (5) — server tells
the attacker nothing, doesn't advance state.

**Response (200, success):**
```json
{
  "status":          "settled",
  "order_id":        "uuid",
  "amount":          "2500.00",
  "currency":        "NGN",
  "new_card_token":  "hex(ciphertext the merchant app writes to card)",
  "card_password":   "hex(NTAG215 PWD for the write — short-lived, single-use)",
  "remaining_daily": "subunit u64",
  "tx_hash":         "sui digest"
}
```

After a successful write, the merchant app POSTs to
`/v1/sender/me/tap-card/:order_id/token-ack` with `{ written: true }`.
On `written: false` (write failed mid-air), the server does NOT roll
back the rotation, because (8) already executed on-chain. Instead the
cardholder is sent down the resync flow (next section). The merchant
app surfaces "Please tap once more to finalize" as the *in-the-moment*
recovery — if the card is still in the field, the second tap retries
the write under the same order. Only after the card leaves does the
cardholder need to resync via PWA.

### Daily-reset cron

A small task in `tasks/tasks.go` runs every minute, finds rows whose
`day_index` is stale relative to UTC now, and zeroes `spent_today`. The
on-chain Move module is the source of truth — the cron only keeps the
off-chain mirror fresh for `step_up_required` pre-checks.

---

## Mobile app (Tapp Merchant) changes

The existing reader half (`hooks/useTapCard.ts`,
`docs/nfc-reader-spec.md`) stays. New work, all on-device:

1. **Auth-tier branching.** After the NFC read, the app calls
   `GET /v1/sender/me/tap-card/nonce?amount=…&card_uid_hash=…` to
   learn which auth tier the amount falls into:
   - `tier=none` → submit the debit directly, no UI prompt.
   - `tier=pin` → show PIN pad screen, compute `K' = HMAC(K, PIN)` and
     `pin_response = HMAC(K', server_nonce)` on-device, submit.
   - `tier=step_up` → show step-up QR screen, poll for grant, re-submit
     debit with the granted `step_up_token`.
2. **PIN pad screen.** Modal numeric keypad over the amount, "Enter
   Zoracle PIN" header, masked dots, auto-submit on 4th digit. Android
   `FLAG_SECURE` set for the screen so it doesn't appear in recents
   or screenshots. Backspace + long-press-to-clear. Wrong-PIN error
   chip slides in with attempts-remaining count.
3. **PIN math on-device only.** `K` is read off the card sector during
   the same NFC session. PIN string → `K' = HMAC-SHA256(K, PIN)` →
   `pin_response = HMAC-SHA256(K', server_nonce)`. `K`, `K'`, and PIN
   are zeroed immediately after the response is computed. Only
   `pin_response` ships to the server. Library: `@noble/hashes`.
4. **NFC write-back.** After the backend returns `new_card_token` +
   `card_password`, write the token to the card's NDEF sector via
   `react-native-nfc-manager`'s `writeNdefMessage`. Authenticate with
   `card_password` first (NTAG215 `PWD_AUTH`). On write success, POST
   `/token-ack { written: true }`. On write failure, show "Please tap
   once more to finalize" — the in-the-moment rescue. If the card has
   already left the field, the cardholder will need to resync via PWA
   later.
5. **Step-up QR display.** On `402 step_up_required`, render a
   full-screen QR containing the step-up URL. Poll
   `GET /v1/sender/me/tap-card/step-up?token=…` every 1.5s. On grant,
   re-submit the debit with the same `server_nonce` + the
   `step_up_token`. Backend skips the auth check on re-submit.

Spec lives at `tapp-merchant/docs/tap-card-pin-flow.md` (companion to
this doc; updated in lockstep with this rev).

---

## Torn writes & resync (PWA-driven recovery)

The strict state machine guarantees that token rotation only fires
after a successful on-chain debit. But the *write* of that new token
to the physical card can still fail mid-air — the customer pulls the
card away a hair too quickly, the merchant phone has a sketchy NFC
antenna, etc. Result: server DB has token B, physical card still has
token A. Next tap of the card anywhere = `403 token_invalid_resync_required`.

Recovery is **cardholder-initiated, PWA-driven**, no support ticket:

1. Cardholder notices card fails at next merchant.
2. Opens Zoracle PWA on their own phone, authenticates via zkLogin.
3. Hits "Resync card" on the dashboard.
4. PWA calls `POST /v1/cards/me/resync` → server returns
   `{ current_token_ct, card_password, resync_nonce }`. The
   `resync_nonce` is a one-shot consumed by step (6).
5. PWA prompts the cardholder to tap the card to the back of their
   phone (Web NFC).
6. PWA does the NTAG215 PWD_AUTH + writes `current_token_ct` to the
   sector, then POSTs `/v1/cards/me/resync/complete { resync_nonce }`.
   Server marks the resync complete; future taps work again.

The `resync_nonce` exists so that a captured response from step (4)
can't be replayed later by anyone who somehow got the cardholder's
device.

**iOS gap:** Web NFC is Chrome-on-Android only. iOS Safari has no
NFC API. So an iOS cardholder whose card desyncs cannot self-serve
resync. v1 stance:
- Expose `POST /v1/admin/cards/:id/recovery` (admin-scoped) that issues
  a 6-digit recovery code emailed to the cardholder. Support reads
  the code over a call, instructs cardholder to read it back, support
  forces a token reset paired with the next tap *on any device that has
  Web NFC* — including, if necessary, an Android Zoracle staff device.
- v1.5 plan: a tiny native iOS "Zoracle Wallet" app that just does
  linking + resync (cardholders don't need it for transactions).
  Estimated ~2 weeks.

**Locked guidance:** never try to fake Web NFC on iOS, never store the
on-card secret `K` outside the card, never let support agents see PINs.

---

## PWA changes (usezoracle/tapp — separate session)

Listed here so the contract is clear; full spec lives in
`tapp/docs/` (to be written).

- `/order/:id` — phone-to-phone payer checkout (zkLogin → sign
  `create_order` PTB).
- `/link?token=…` — claim a freshly-issued card. zkLogin → server flips
  status `issued` → `claimed` → Web NFC writes `K` to the card sector
  with PWD lock → zkLogin signs `create_cap` PTB → status `live`.
- `/dashboard/cards/:id` — status, balance, daily-limit consumed,
  recent debits, top-up, resync, revoke.
- `/cards/step-up?token=…` — WebAuthn platform authenticator → POST a
  grant flag the merchant-side poll picks up.
- `/cards/resync` — the torn-write recovery flow above.

---

## Open questions (flag for sign-off before implementing)

1. **Card population in v1.** Where do blank NTAG215s come from?
   Branded cards (better UX, supply lead time) or BYOC from Amazon
   (zero lead time, no Zoracle branding)? Affects card pre-formatting
   + password defaults.
2. **PIN reset.** With no stored PIN, "forgot PIN" cannot be recovered
   server-side — the cardholder must revoke + re-link. They keep the
   on-chain balance (it's their cap, only their key destroys it; PIN
   is just for runtime auth, not custody). Acceptable, or do we want
   to add a re-link path that preserves the same `cap_object_id`?
   (Doable — re-linking writes a new `K`, the cap on-chain doesn't
   need to change.)
3. **Step-up biometric.** WebAuthn platform authenticator works on
   most modern phones, but enrollment requires HTTPS + a registered
   credential. Issue at signup or first step-up? Issue at first
   step-up is friction-cheaper but adds latency on first big tap.
4. **Default thresholds.** Drafted as per-tap `₦2,000`, step-up
   `₦15,000`, daily `₦40,000` (~$25). Configurable per-card via PWA.
   Acceptable, or push lower for v1 caution?
5. **Multiple cards per user.** Spec assumes 1:1 (matches `Unique()`
   edge). Multi-card = one `CardSpendingCap` per card; trivial to
   relax later but call it now if v1 wants room.
6. **Currency.** v1 = USDC on Sui only. Multi-coin caps would mean
   one `CardSpendingCap<T>` per coin type — viable, but adds UX
   complexity. Defer to v2?

---

## Verification strategy

- **Move:** existing 9/9 Gateway tests + a new `tapp_card_tests`
  suite covering the 6 invariants. `sui move test` must still exit
  clean.
- **Rails:**
  - `go build ./...` exit 0
  - Unit tests in `controllers/cards/cards_test.go` covering: PoC
    issue-batch + redirect, link claim → complete happy path, no-PIN
    tier under threshold, PIN tier with valid/invalid response, PIN
    lockout after 5, step-up tier with QR grant, daily-limit reject,
    day rollover, torn-write resync round-trip, replayed nonce
    rejection, replayed pin_response rejection.
  - Integration test on Sui devnet for create_cap + debit end-to-end.
- **Tapp Merchant:** manual E2E on a physical Android device — tap
  an NTAG215 pre-linked via local PWA, complete each tier (none /
  PIN / step-up), observe new token written, observe `payment.settled`
  SSE event.

---

## Appendix A — PIN protocol math

Server-stored (per card): `pin_verifier = HMAC-SHA256(K, "tapp-card-verifier-v1")`

Per-tap nonce: `server_nonce ← random(32 bytes)`, single-use, 60s TTL.

Client (merchant app, after reading `K` off the card):
```
K_prime       = HMAC-SHA256(K, utf8(PIN))
pin_response  = HMAC-SHA256(K_prime, server_nonce)
```

Server verify:
```
expected_K_prime = HMAC-SHA256(pin_verifier, utf8("derive"))    // off-chain anchor
                                                                // pre-derived at linking,
                                                                // never computable from K alone
expected_resp    = HMAC-SHA256(expected_K_prime, server_nonce)
ok               = constant_time_eq(pin_response, expected_resp)
```

The `expected_K_prime` is itself derived at linking time:
```
// Linking, in PWA, after PIN selection:
K               = random(32 bytes)
K_prime         = HMAC-SHA256(K, utf8(PIN))
pin_verifier    = HMAC-SHA256(K, "tapp-card-verifier-v1")
linking_proof   = HMAC-SHA256(K_prime, "linking-anchor-v1")
// PWA sends { pin_verifier, linking_proof, card_uid_hash, ... }
// Server stores { pin_verifier, linking_proof }; never sees K, K', or PIN.
```

At debit time the server doesn't compute `expected_K_prime` from `K`
(it doesn't have it). It computes:
```
expected_resp = HMAC-SHA256(linking_proof, server_nonce)
                                ^^^^^^^^^^^^^ stored at linking
```
…and the merchant app, knowing `K` (read from card) and the PIN,
computes the same value:
```
K_prime       = HMAC-SHA256(K, utf8(PIN))
client_anchor = HMAC-SHA256(K_prime, "linking-anchor-v1")  // === linking_proof
pin_response  = HMAC-SHA256(client_anchor, server_nonce)
```

If the PIN is wrong, `client_anchor ≠ linking_proof`, so
`pin_response` doesn't match `expected_resp`. The server never had to
know the PIN, never had to know `K`, and never had to store anything
that lets an offline cracker recover the PIN from the DB.

(Implementation: ~30 lines of Go using `crypto/hmac` + `crypto/subtle`
on the server, ~25 lines of TS using `@noble/hashes/hmac` on the
merchant app.)
