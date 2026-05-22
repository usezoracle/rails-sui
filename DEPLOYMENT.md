# Rails — Deployment & production-readiness checklist

End-to-end ops guide for taking the Zoracle stack from a working local
dev loop to a live deployment. Each section maps to a concrete config
+ env var the code reads.

> Companion to the per-repo README. Touches all three repos:
> `usezoracle/rails-sui` (this one), `usezoracle/tapp` (cardholder PWA),
> `usezoracle/tapp-merchant` (merchant app).

---

## 1. Sui Move package

1. **Publish.** From `contracts/gateway/`:
   ```bash
   sui client switch --env testnet     # or mainnet, when ready
   sui client publish --gas-budget 200_000_000
   ```
   Note the **PackageID** (`Published Objects → ...`), the
   **AdminCap** object ID, and the shared **Gateway** object ID.

2. **Mint an aggregator cap to the backend wallet:**
   ```bash
   sui client call \
     --package <PackageID> --module config --function mint_aggregator_cap \
     --args <AdminCap> <AggregatorWalletAddress> \
     --gas-budget 50_000_000
   ```
   Capture the **AggregatorCap** object ID.

3. **Tests passing pre-publish:**
   ```bash
   sui move test           # must be 16/16 green (9 order + 7 tapp_card)
   ```

4. **Wire IDs into Rails env (next section).**

---

## 2. Rails backend (`/Users/mac/rails`)

### Required env

```env
# Server
SECRET=                                  # `openssl rand -hex 32`
HOST_DOMAIN=https://api.zoracle.com      # public URL
PWA_BASE_URL=https://tapp.zoracle.com    # used in /c/:token redirect
CHECKOUT_BASE_URL=https://checkout.zoracle.com

# Auth
GOOGLE_OAUTH_CLIENT_ID=                  # same client ID as PWA
ADMIN_API_TOKEN=                         # `openssl rand -hex 32` for /v1/cards/issue-batch + recovery

# Database + Redis (managed Postgres / Redis is fine; see Makefile)
DB_NAME=rails  DB_USER=...  DB_PASSWORD=...  DB_HOST=...

# Sui
SUI_RPC_URL=https://fullnode.testnet.sui.io:443
SUI_GATEWAY_PACKAGE_ID=                  # from step 1.1
SUI_GATEWAY_OBJECT_ID=                   # from step 1.1
SUI_AGGREGATOR_CAP_ID=                   # from step 1.2
SUI_AGGREGATOR_PRIVATE_KEY=              # 32-byte hex (NO 0x prefix)

# Email (SendGrid for cardholder + admin recovery)
EMAIL_API_KEY=                           # SendGrid API key
EMAIL_FROM_ADDRESS=Rails <no-reply@usezoracle.com>
CARD_RECOVERY_SENDGRID_TEMPLATE=         # dynamic template id (d-...)

# KYC (Smile Identity for merchant KYB)
SMILE_IDENTITY_API_KEY=
SMILE_IDENTITY_PARTNER_ID=
SMILE_IDENTITY_BASE_URL=https://api.smileidentity.com
```

### Migration

```bash
go generate ./ent/...
make migrate              # atlas-driven schema migration
```

### Smoke

```bash
go build ./... && go run main.go
curl localhost:8000/health
```

### Bootstrap admin tasks

- Seed the `Institution` table with NGN banks (BVN-friendly CBN codes).
  Source: CBN bank-code list. ~30 rows.
- Configure the `Network` row for `sui-testnet` (or `sui-mainnet`)
  with the right RPC URL.
- Configure the `Token` row for USDC on Sui with `contract_address` =
  the Move coin type string from `sui::coin::Coin<...::usdc::USDC>`.

---

## 3. Tapp PWA (`/Users/mac/tapp`)

### Required env (`.env.local` for dev, Vercel project env for prod)

```env
NEXT_PUBLIC_API_BASE_URL=https://api.zoracle.com
NEXT_PUBLIC_GOOGLE_OAUTH_CLIENT_ID=...
NEXT_PUBLIC_SUI_NETWORK=testnet
NEXT_PUBLIC_TAPP_PACKAGE_ID=             # same as Rails SUI_GATEWAY_PACKAGE_ID
NEXT_PUBLIC_USDC_COIN_TYPE=0x...::usdc::USDC
NEXT_PUBLIC_ZKLOGIN_PROVER_URL=          # default points at Mysten testnet — self-host for prod
NEXT_PUBLIC_ZKLOGIN_SALT_URL=
NEXT_PUBLIC_DEMO_LINK=                   # leave empty for prod; "1" skips Sui chain calls
```

### Build

```bash
npm install
npm run build
```

### Deploy (Vercel)

```bash
vercel link
vercel env pull .env.production.local
vercel --prod
```

Add the production URL to Google OAuth Authorized Origins.

### zkLogin infra (recommended for production)

Mysten's hosted prover and salt service are testnet-rate-limited. Self-host both:
- Prover: github.com/MystenLabs/zklogin-prover (Docker)
- Salt service: same repo, `zklogin-salt-service/`

Point `NEXT_PUBLIC_ZKLOGIN_*_URL` at your hosted endpoints.

---

## 4. Tapp Merchant (`/Users/mac/tapp-merchant`)

### env (`.env` for local; `app.json` `expo.extra.*` for production builds)

```env
EXPO_PUBLIC_API_BASE_URL=https://api.zoracle.com
EXPO_PUBLIC_CHECKOUT_BASE_URL=https://checkout.zoracle.com
```

### Build pipeline

```bash
npm install
npm run prebuild         # writes android/ and ios/
npm run android          # or `ios`
```

Production: use EAS Build. Apple Developer team profile required for the iOS NFC entitlement.

---

## 5. Smoke-test the full Tap Card vertical end-to-end

Once the Move package is published + env wired, prove the loop:

1. **Issue & write a card** (admin):
   ```bash
   curl -X POST https://api.zoracle.com/v1/cards/issue-batch \
     -H "X-Admin-Token: $ADMIN_API_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"count":1}'
   ```
   Paste the returned URL into NFC Tools → write to an NTAG215.

2. **Link the card** (cardholder, on Android Chrome):
   - Tap card → Safari/Chrome opens the URL → claim flow runs
   - `/link/configure` → set limits + PIN + funding amount
   - `/link/write` — tap card; PWA writes K + token + sets uid_hash
   - `/link/sign` — zkLogin signs `create_cap` PTB → POSTs
     `/v1/cards/link/complete`
   - Rails verifies the digest via SuiGetTransactionBlock, persists
     pin_verifier + linking_proof
   - Dashboard shows status=live

3. **Tap to pay** (merchant, on physical Android device):
   - `EXPO_PUBLIC_API_BASE_URL=https://api.zoracle.com` + sign in
     as a merchant who saved a NGN bank account
   - New payment → enter amount → "Tap Card"
   - Cardholder taps; PIN pad if amount ≥ ₦2k; debit POSTed
   - `OrderSui.DebitCard` builds the `tapp_card::debit<USDC>` PTB
     and submits
   - Sui event indexer sees `CardDebited` → marks PaymentOrder
     settled → publishes `payment.settled` on the SSE bus
   - Merchant screen shows "Payment received"

4. **Test resync** (cardholder Android):
   - Pull the card away early during a tap to simulate a torn write
   - Dashboard `/cards/me` shows `needs_resync: true`
   - Run `/cards/resync` — Web NFC writes the canonical token; flag
     clears

5. **Test step-up** (cardholder iOS or Android):
   - Set per-tap cap small, attempt a debit above the step-up
     threshold
   - Merchant screen renders the QR
   - Cardholder scans → `/cards/step-up?token=…` → WebAuthn → grant
   - Merchant polls `/step-up?token=…` → 200 granted
   - Merchant re-submits debit; succeeds

---

## 6. Production-readiness punch list (after the above)

- [ ] **Self-host the Mysten prover + salt service.** Hosted endpoints
      are rate-limited.
- [ ] **Verify WebAuthn assertions in `StepUpGrant`.** v1 accepts
      any authenticated POST; production should verify via
      `github.com/go-webauthn/webauthn`. Needs a credential
      registration flow first (lazy on first step-up is acceptable).
- [ ] **Run a Sui devnet integration test** that publishes a fresh
      Gateway + tapp_card, creates a cap, runs a debit, and observes
      the indexer event. Wire into CI.
- [ ] **CARD_RECOVERY_SENDGRID_TEMPLATE** — create a dynamic SendGrid
      template with one variable: `{{recovery_code}}`. Tested via
      the AdminRecovery endpoint.
- [ ] **Apple Developer team profile** for the merchant app's iOS
      NFC entitlement (paid Developer account required).
- [ ] **Per-card lockout windows** — currently locked-card recovery
      is only via `AdminRecovery`. Consider adding a 24h auto-unlock
      hook for the `locked` state.
- [ ] **Sui devnet → mainnet cutover plan.** Specifically: re-publish
      Gateway at mainnet, re-derive aggregator wallet, drain testnet
      funds.
- [ ] **Rate-limit the `/nonce` endpoint** per (sender_id, card_id)
      to ~10/min — defense against probing the tier resolver.
- [ ] **Add `card_id` column on `PaymentOrder`** for Tap Card flows
      (currently correlated via `Reference`). Cleaner FK + indexer
      lookups.

---

## 7. Known limitations carried into v1

- **iOS cardholders cannot Web-NFC.** Linking + resync require an
  Android device or admin recovery. v1.5 plan: ship a tiny native
  iOS Zoracle Wallet app for linking + resync only.
- **NTAG215 PWD_AUTH is not set during PWA linking** (Web NFC has no
  raw transceive). The `card_password` we generate is stored but
  unused until a native linking client lands. The HMAC PIN protocol
  is the real security boundary; PWD_AUTH was defense-in-depth.
- **Step-up token verification is shape-only in v1** — full WebAuthn
  signature check follows credential registration.
- **NGN-only** in v1. Multi-currency support is post-v1 and would
  parameterize the BaaS rail + the `CardSpendingCap<T>` per coin.
