# LP Virtual Fiat Accounts — Spec

## Purpose

Issue a **dedicated virtual bank account** to each onboarded LP, in the LP's local currency. LP deposits fiat into that account; Rails has pull authority to debit it when the LP wins an order. This is a hard requirement from the PRD, and a deliberate departure from the legacy protocol design (which delegates fulfillment to LP-operated webhook servers via `host_identifier`).

## Custody & Mandate Model (locked, regulatorily load-bearing)

Three parties, three roles, three different risk profiles:

| Party | Role | Holds funds? | Licensed? |
|---|---|---|---|
| **LP** | Legal owner of record — the virtual account is titled to them | ✅ — the fiat is legally theirs throughout | No |
| **BaaS provider** (Paystack / Flutterwave / Xendit, plus their underlying partner bank) | Account issuer, rails operator, custodian of record | ✅ — funds sit in their banking infrastructure, segregated under the LP's name | Yes — Payment Institution / EMI / banking license per jurisdiction |
| **Rails (aggregator)** | Holds a signed debit mandate from the LP; instructs the BaaS to execute debits within mandate scope | ❌ — never touches the money | Lighter — Payment Aggregator / PSP class, typically umbrella'd under the BaaS partner |

**The mandate is the load-bearing primitive.** At LP onboarding, the LP signs (click-through or wet) a direct-debit authorization scoped to "Rails may instruct BaaS to debit account X up to amount Y per transaction, only for the purpose of settling matched orders within the configured rate ceiling." Standard product across SEPA Direct Debit, ACH, GoCardless, UPI AutoPay.

**Money flow on a matched order** — note the absence of any Rails-controlled account:

```
LP's virtual account                              Merchant's bank
(titled to LP @ partner bank)                     (titled to merchant)
        │                                                ▲
        │                                                │
        │   Rails sends "debit X / credit Y"             │
        │   instruction to BaaS                          │
        ▼                                                │
   BaaS provider ──────── executes the transfer ─────────┘
                          (their licensed rails)
        │
        │   Webhook: payout success / fail
        ▼
   Rails handler → trigger on-chain settle_order (release Sui USDC to LP wallet)
```

**Why this matters:**
- **Regulatory burden:** Custody = Money Transmitter / EMI / trust licensing (months-to-years per jurisdiction, capital reserves, fiduciary audits). Mandate orchestration = much lighter PSP class, often issued by the BaaS as a sub-license.
- **LP trust:** LPs retain title to their funds. They can revoke the mandate any time (the dashboard exposes a "revoke mandate" action — calling `BaaS.RevokeMandate` and flipping `LPFiatAccount.status = "suspended"`).
- **Architectural clarity:** No "Rails treasury account for LP fiat" exists. If you ever find yourself designing one, you've slipped into custody territory and should stop.

The whole structure is the same legal pattern as Stripe charging a card-on-file for a Netflix subscription, Klarna pulling an installment, or GoCardless executing a Direct Debit — Rails is an instructing party with a mandate; the licensed entity (BaaS) actually moves the money.

## Operational Model: Passive LPs (no node, no webhooks)

LPs are **passive capital providers**. They do not run any infrastructure — no node, no webhook server, no per-order accept/reject. Their interactions with Rails are:

1. Sign up + KYB (one-time)
2. Sign the BaaS debit mandate (one-time, during onboarding)
3. Configure rate band + supported currencies in dashboard (occasional)
4. Deposit fiat into their virtual account via bank transfer (recurring)
5. Watch their Sui wallet receive coin payouts as orders settle

The `host_identifier` webhook field on the legacy `ProviderProfile` schema is **dropped** in the Rails fork. LPs do not receive per-order webhooks because they have no per-order decision to make — the matching engine selects them based on static config (rate, balance, KYB), and the BaaS executes the debit. (We may later repurpose a notification webhook for things like low-balance alerts, but it's not a per-order signal.)

Two distinct capabilities are needed from whichever BaaS provider we pick per market:

1. **Issue virtual accounts** keyed to our LPs (sub-account / dedicated account number per LP).
2. **Pull funds** from those accounts on demand to pay merchants (push payouts to arbitrary local bank accounts).

These are not always the same product. Some BaaS providers do (1) but not (2), or vice versa. Per-market selection.

---

## v1 Scope: NGN Only

**v1 ships with NGN (Nigeria) as the sole supported currency.** KES and IDR are post-v1 expansion candidates documented at the bottom of this section, not in scope for the initial build.

The specific NGN BaaS provider is **to be supplied by the user** (they have a preferred partner in mind based on existing commercial relationships). The `Provider` interface defined below is provider-agnostic — once the choice is made, implementing it is a contained piece of work in `services/baas/<provider_name>.go`.

Evaluation criteria for whichever NGN BaaS gets picked:

| Requirement | Why it matters |
|---|---|
| **Dedicated virtual NUBAN per LP** | Each LP needs an account number titled to them that they fund via standard bank transfer. |
| **Transfer/payout API** | Required to disburse from LP virtual accounts to merchant bank accounts on order match. |
| **Mandate / pre-authorized debit support** | Required for the no-custody pull model. The LP signs once during onboarding; subsequent debits don't need per-transaction LP consent. |
| **Webhook reliability** | Payout success / failure webhooks are the source of truth for `LockOrderFulfillment` validation. |
| **Signature-verifiable webhooks** | Required for security (no spoofed payout-success claims). |
| **Sandbox environment** | Required for end-to-end test flows without touching real money. |
| **Idempotency keys on disbursement API** | Required to make our retry logic safe. |
| **TOS permits third-party crypto-related disbursements** | Verify explicitly during procurement — some BaaS contracts restrict crypto-adjacent use cases. |

Common Nigerian BaaS candidates that meet most of these (for reference, not a commitment): Paystack (Dedicated NUBAN + Transfers), Flutterwave, Anchor, Monnify. The user's chosen provider plugs into the abstraction once known.

### Post-v1 currency expansion (out of scope for initial build, documented for forward-compatibility)

| Currency | Likely BaaS candidates | Notes |
|---|---|---|
| KES (Kenya) | Flutterwave (M-Pesa B2C), Cellulant | Tapp's broader Africa expansion. |
| IDR (Indonesia) | Xendit, Midtrans, DOKU | Tapp's SEA launch market. Trigger once NGN is stable in production. |

The `Provider` abstraction supports multiple currencies via the registry pattern below, so adding KES/IDR later is contained to (1) building a new `Provider` implementation per market and (2) registering it in `services/baas/registry.go`. No core matching / lifecycle code changes.

---

## Abstraction

```go
// services/baas/baas.go
package baas

type Provider interface {
    // Issue a dedicated virtual account for an LP.
    IssueVirtualAccount(ctx context.Context, req IssueRequest) (*VirtualAccount, error)

    // Get current balance of an LP's virtual account.
    GetBalance(ctx context.Context, account *VirtualAccount) (decimal.Decimal, error)

    // Pull funds from LP's account and disburse to a merchant bank account.
    DisburseFunds(ctx context.Context, req DisburseRequest) (*Disbursement, error)

    // Verify a webhook signature (each provider differs).
    VerifyWebhook(signature, body []byte) bool

    // Parse webhook payload into a normalized event.
    ParseWebhook(body []byte) (*WebhookEvent, error)

    Currency() string  // "NGN" | "KES" | "IDR"
    Name() string      // "paystack" | "flutterwave" | "xendit"
}

// Registry — currency → Provider.
// v1: NGN only. The concrete provider is the one the user selects during procurement.
var providers = map[string]Provider{
    "NGN": &NGNProvider{...},  // concrete impl plugged in once user supplies their BaaS choice
}

func GetProviderFor(currency string) (Provider, error) {
    p, ok := providers[currency]
    if !ok { return nil, ErrNoProvider }
    return p, nil
}
```

Per-provider implementations under `services/baas/paystack.go`, `flutterwave.go`, `xendit.go`. Each implements the `Provider` interface.

---

## Database Schema

```go
// ent/schema/lp_fiat_account.go
type LPFiatAccount struct{}
func (LPFiatAccount) Fields() []ent.Field {
    return []ent.Field{
        field.String("id").Unique(),                          // internal id
        field.String("baas_provider"),                        // "paystack" | "flutterwave" | "xendit"
        field.String("baas_reference"),                       // provider's internal account id
        field.String("account_number"),                       // human-visible (NUBAN, MPESA shortcode, IDR VA)
        field.String("account_name"),
        field.String("bank_name"),                            // for NGN/IDR bank-style VAs
        field.String("currency"),                             // "NGN" | "KES" | "IDR"
        field.Decimal("last_known_balance").Default(0),
        field.Time("last_balance_check_at").Optional(),
        field.Enum("status").Values("active", "suspended", "closed").Default("active"),
        field.Time("created_at"),
    }
}
func (LPFiatAccount) Edges() []ent.Edge {
    return []ent.Edge{
        edge.From("provider_profile", ProviderProfile.Type).
            Ref("fiat_accounts").Unique().Required(),
    }
}
```

Extend `ProviderProfile` to add the inverse:
```go
edge.To("fiat_accounts", LPFiatAccount.Type),
```

One LP can have multiple fiat accounts (one per currency they support).

---

## LP Sui Payout Wallet (Bring Your Own — never generated)

The crypto-side custody story mirrors the fiat side: **LPs supply their own Sui wallet address**. Rails does not generate keypairs, does not store private keys, does not custody coin. The wallet is whatever the LP wants — Sui Wallet, Suiet, hardware wallet, exchange deposit address, multisig.

Trust model: we adopt **the two-tier pattern verbatim** (see `controllers/index.go:594-628` in the upstream reference for the EVM original):

### Tier 1 — KYC-time wallet proof (cryptographic, once)

The LP proves control of one Sui wallet during KYC signup. This binds their KYB'd identity to a wallet, and authenticates all subsequent dashboard actions.

Frontend (using `@mysten/dapp-kit`):
1. Connect Wallet → user picks Sui Wallet / Suiet / Ethos / Phantom / hardware bridge / etc.
2. Backend issues a nonce.
3. Frontend calls `signPersonalMessage` with the message:
   ```
   I accept the Rails KYC Policy and request an identity verification
   for {address} with nonce {nonce}
   ```
4. Wallet returns `{ signature, publicKey, address }`.

Backend (`controllers/index.go` ported, `utils/sui_signature.go` new):
1. Reconstruct the message from the stored nonce.
2. Decode the signature; assert it parses (Sui sigs are `flag_byte || sig_bytes || pubKey_bytes` per the Sui signature spec).
3. Verify cryptographically by signature scheme:
   - **Ed25519** (default): `ed25519.Verify(pubKey, message, sig)`.
   - **Secp256k1 / Secp256r1**: standard ECDSA verify with the matching curve.
   - **Multisig / zkLogin**: defer to Sui's verification helpers (Phase 1 ships Ed25519 only; others added as we encounter them).
4. Derive the Sui address from the recovered pubKey: `blake2b256(flag_byte || pubKey)[:32]`. Note Sui uses an **Intent scope wrapper** around `personal_sign` — the verified hash is `blake2b256(IntentScope::PersonalMessage || BCS::serialize(message))`, not the raw message. Adopt Sui's spec, don't roll our own prefix.
5. Compare derived address to claimed address. Reject on mismatch.
6. Persist on `IdentityVerificationRequest.wallet_address` + `.wallet_signature` (the existing schema works for Sui addresses with no change).

Same algorithmic shape as the upstream EVM flow, Sui primitives substituted:

| Upstream (EVM) | Rails (Sui) |
|---|---|
| EIP-191 prefix `"\x19Ethereum Signed Message:\n" + len(msg)` | `IntentScope::PersonalMessage` BCS-encoded prefix |
| `crypto.Keccak256Hash` | `blake2b256` |
| `crypto.SigToPub` (secp256k1 ecrecover) | `ed25519.Verify(pubKey, msg, sig)` (default; other schemes as needed) |
| `crypto.PubkeyToAddress` | `blake2b256(flag_byte \|\| pubKey)[:32]` |
| MetaMask `personal_sign` on frontend | `@mysten/dapp-kit signPersonalMessage` |

### Tier 2 — Payout addresses declared freely (no per-address proof)

Once authenticated (tier 1 done, KYB approved), the LP configures payout addresses on `ProviderOrderToken.addresses` via the existing `UpdateProviderProfile` endpoint (`controllers/accounts/profile.go:215+`). Addresses are accepted as submitted, with format validation only (`IsValidSuiAddress`). No signature, no test transfer, no proof of ownership per address.

This matches the established pattern verbatim and is industry-standard for B2B settlement (same trust model as Stripe Connect's payout-account configuration). The implicit contract: the authenticated LP is responsible for the correctness of payout addresses they declare. If they typo and lose funds, that's on them.

The existing schema is fine — no extension needed:

```json
{
  "network": "sui-mainnet",
  "address": "0xabc..."
}
```

We can add a `label` field later if LPs ask for it (purely cosmetic), and `coin_type` if we ever support multiple coin types per LP per currency, but v1 ports the legacy shape one-for-one.

### Settlement (custody-free path on-chain)

When the Rails aggregator wallet (holding `AggregatorCap`) calls `settle_order(... liquidity_provider: lp_address ...)`, the Move Gateway transfers coin from order escrow directly to the LP's address in the same transaction. The aggregator wallet only signs the call; coin never passes through it.

### Hard rule

If we ever find ourselves writing code that generates a Sui keypair for an LP and stores the private key (KMS-wrapped or otherwise), that's a custody slip — stop. The whole regulatory posture depends on Rails never holding LP-attributable keys.

## LP Onboarding Flow Extension

Current legacy flow (controllers/provider/provider.go):
1. User signs up → `User` created.
2. User submits KYB → Smile Identity verification.
3. KYB passes → `ProviderProfile.is_kyb_verified = true`.
4. User configures `ProviderOrderToken` (rates, tokens, networks).
5. LP can receive orders.

**New step (4.5):** After KYB passes, before LP can receive orders for a currency:
- Backend calls `baas.GetProviderFor(currency).IssueVirtualAccount(...)`.
- Persists returned `LPFiatAccount`.
- LP sees their virtual account number in their dashboard.
- LP funds it via bank transfer (off-platform action by LP).
- LP cannot receive orders for that currency until `LPFiatAccount.status = active` AND `last_known_balance > min_threshold`.

---

## Settlement Hook (Pull Flow)

When the matching engine assigns an order to an LP (LP selected by static rules — within ceiling, balance ≥ min, KYB valid — with no per-order LP acknowledgment needed):

```go
// services/order/sui.go (or shared service)
func DisburseLPFiat(ctx context.Context, order *LockPaymentOrder) error {
    lp := order.Edges.Provider
    fiatAcct, err := db.LPFiatAccount.Query().
        Where(lpfiataccount.HasProviderProfileWith(providerprofile.IDEQ(lp.ID))).
        Where(lpfiataccount.CurrencyEQ(order.FiatCurrency)).
        Only(ctx)
    if err != nil { return err }

    provider, err := baas.GetProviderFor(order.FiatCurrency)
    if err != nil { return err }

    disb, err := provider.DisburseFunds(ctx, baas.DisburseRequest{
        FromAccount:        fiatAcct,
        ToBankCode:         order.Edges.Recipient.Institution,
        ToAccountNumber:    order.Edges.Recipient.AccountIdentifier,
        ToAccountName:      order.Edges.Recipient.AccountName,
        Amount:             order.Amount.Mul(order.Rate),  // fiat = coin × rate
        Currency:           order.FiatCurrency,
        Reference:          order.ID,                       // for idempotency at BaaS
        Memo:               order.Edges.Recipient.Memo,
    })
    if err != nil {
        order.Status = "fulfillment_failed"
        // log + alert
        return err
    }

    // Record disbursement
    db.LockOrderFulfillment.Create().
        SetTxID(disb.ProviderRef).
        SetPSP(provider.Name()).
        SetValidationStatus("pending").
        SetLockPaymentOrder(order).
        Save(ctx)

    return nil
}
```

When the BaaS provider's webhook confirms the payout, validation flips to `success` and the on-chain `settle_order` is queued for submission.

---

## Webhook Handling

Each BaaS provider has its own webhook signature scheme. Unified handler:

```go
// controllers/baas_webhook.go
func HandleBaaSWebhook(c *gin.Context) {
    providerName := c.Param("provider")  // "paystack" | "flutterwave" | "xendit"
    provider, err := baas.GetProviderByName(providerName)
    if err != nil { c.AbortWithStatus(404); return }

    body, _ := io.ReadAll(c.Request.Body)
    sig := c.GetHeader(provider.SignatureHeader())

    if !provider.VerifyWebhook([]byte(sig), body) {
        c.AbortWithStatus(401); return
    }

    event, err := provider.ParseWebhook(body)
    if err != nil { c.AbortWithStatus(400); return }

    switch event.Type {
    case baas.EventDisbursementSucceeded:
        markFulfillmentValidated(event.Reference)
    case baas.EventDisbursementFailed:
        markFulfillmentFailed(event.Reference, event.FailureReason)
    case baas.EventBalanceUpdated:
        updateLPBalance(event.AccountID, event.Balance)
    }

    c.Status(200)
}
```

Endpoint per provider: `POST /v1/baas/{provider}/webhook`. Configured in each BaaS's dashboard pointing to our public URL.

---

## Operational Concerns

### Funding policy
- LPs fund their virtual accounts via standard bank transfer to the issued account number.
- Minimum balance threshold per currency to be matchable for orders (e.g., NGN: ₦100k, KES: KSh 50k, IDR: Rp 10M). Below threshold, LP shows as `unavailable` in matching even if `is_active`.
- Periodic balance refresh: `GetBalance` polled every 5 min per active LP, OR push-updated via balance webhooks where the BaaS supports it.

### Compliance / regulatory
- Each BaaS handles its own KYC of us as a business customer. LP KYC is our responsibility (via Smile Identity, already in place).
- The virtual accounts are titled to the LP (not Rails), so funds in them are LP-owned. Rails has pull authority via mandate, not custody.
- Each provider's TOS may restrict use cases — verify "third-party crypto-related disbursements" is permitted before go-live.

### Reconciliation
- Daily reconciliation cron compares `LPFiatAccount.last_known_balance` against provider-reported balance.
- Discrepancies > 0.01 trigger ops alert.
- Provider-side statement download (monthly) cross-checked against our `LockOrderFulfillment` table.

### Failure modes
- **BaaS down** → matching engine treats all LPs on that currency as unavailable. Falls back to Route A if integrator allows.
- **LP overdraft attempt** → BaaS rejects the disbursement; order marked `fulfillment_failed`; LP penalized in reputation.
- **Wrong recipient details** → disbursement fails; manual ops triage; refund flow triggered.

---

## Testing

- **Mock BaaS provider** for development: in-memory provider implementing the interface, with configurable success/failure rates.
- **Sandbox testing**: each BaaS has a sandbox environment. Onboarding + disbursement tested end-to-end before any production traffic.
- **Webhook signature verification** must have unit tests per provider (each scheme is different).

---

## Open Items

- **Currency expansion path:** Beyond NGN/KES/IDR, what's next? PHP, GHS, INR? Each new currency = new BaaS evaluation + integration.
- **Cross-currency LPs:** Can one LP serve NGN and KES? Schema supports it (multiple `LPFiatAccount` per `ProviderProfile`), but matching engine assumes single-currency per LP today. Confirm and adjust.
- **Idempotency at BaaS:** All chosen providers support idempotency keys. Standardize on using `order.ID` as the key.
- **Multi-account redundancy:** Should one LP have multiple virtual accounts per currency (one per BaaS) for resilience? v1: no. Single account per (LP, currency).
