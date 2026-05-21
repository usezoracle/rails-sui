# Rails B2B API — Spec

## Audience

Third-party dApps, exchanges, and consumer products (including our own Tapp client) that need to convert Sui stablecoins → local fiat without building bridging, KYC, LP matching, or banking infrastructure themselves.

---

## Design Principles

1. **One endpoint creates an order.** The integrator does not pick Route A vs Route B vs LP; the engine picks. The integrator's only routing knob is `mode` for Route A (`"lp" | "treasury"`).
2. **Idempotent everywhere.** Every mutating call accepts `Idempotency-Key`. Replays return the original response.
3. **Webhooks are the source of truth for state changes.** Polling is supported (`GET /orders/{id}`) but webhooks are the official channel.
4. **Webhooks are signed.** HMAC-SHA256 over the request body using a per-integrator secret.
5. **Errors are typed.** Every error has a stable `code` (e.g. `INSUFFICIENT_LP_LIQUIDITY`) the integrator can branch on.
6. **No PII in URLs.** Account numbers, names, addresses go in request bodies.

---

## Authentication

API key in `Authorization: Bearer rk_live_...` (or `rk_test_...` for sandbox).

- Keys issued per integrator via internal admin tool.
- Keys can be scoped: `orders:read`, `orders:write`, `webhooks:manage`.
- Key rotation supported. Old key remains valid for 24h after rotation.

---

## Versioning

URL-versioned: `/v1/...`. Breaking changes only in new major versions. Additive changes (new optional fields, new endpoints) within v1.

---

## Endpoints

### Create order
```http
POST /v1/orders
Authorization: Bearer rk_live_...
Idempotency-Key: 7f3a...
Content-Type: application/json

{
  "coin": "USDC",
  "amount": "100.00",
  "fiat_currency": "NGN",
  "recipient": {
    "institution_code": "044",
    "account_number": "0123456789",
    "account_name": "Jane Doe",
    "memo": "Invoice 18472"          // optional
  },
  "refund_address": "0x...sui-address...",
  "route_preference": "auto",         // "auto" | "route_b" | "route_a_lp" | "route_a_treasury"
  "webhook_url": "https://...",       // optional override of default
  "metadata": { "integrator_ref": "..." }  // arbitrary, returned in webhooks
}
```

Response (`201 Created`):
```json
{
  "order_id": "ord_abc123",
  "status": "awaiting_deposit",
  "route": "route_b",
  "pay_to": {
    "chain": "sui",
    "amount": "100.000000",
    "coin_type": "0x...::usdc::USDC",
    "options": [
      {
        "method": "ptb",
        "label": "Pay with connected Sui wallet (one click, custody-free)",
        "gateway_package_id": "0xabc...",
        "ptb_base64": "<serialized PTB the user's wallet signs and submits>"
      },
      {
        "method": "receive_address",
        "label": "Send from any wallet or exchange (Bybit, Binance, OKX, etc.)",
        "address": "0xdeadbeef...",
        "qr_payload": "sui:0xdeadbeef...?amount=100&coin=0x...::usdc::USDC",
        "valid_until": "2026-05-21T17:30:00Z"
      }
    ]
  },
  "rate_quoted": "1530.50",
  "fiat_amount": "153050.00",
  "fee": "0.50",                       // protocol + sender fee, in coin units
  "expires_at": "2026-05-21T17:30:00Z",
  "created_at": "2026-05-21T17:00:00Z"
}
```

`pay_to.options` returns BOTH deposit paths. The integrator's UI shows them side-by-side and the end-user picks based on where their crypto lives.

**Method `ptb` — one-click signing, custody-free.** A pre-built Sui Programmable Transaction Block (PTB) the user's connected wallet signs via `@mysten/dapp-kit`'s `signAndExecuteTransactionBlock`. Atomically takes the specified amount of coin from the user's wallet and calls the Move Gateway's `create_order` entry function. Coin flows from user → directly into the on-chain Order escrow. No Rails-controlled wallet in the path. Works for Sui Wallet, Suiet, Phantom, hardware bridges, and Tapp's zkLogin (where the same PTB is signed via Google OAuth proof with Rails sponsoring gas).

**Method `receive_address` — exchange / external wallet fallback, brief custody.** A fresh per-order Sui address. The user opens their exchange withdrawal UI (Bybit, Binance, OKX, etc.), pastes the address, sends the specified amount of USDC. Backend indexer detects the deposit via Sui WebSocket event subscription (sub-second), then submits `create_order` from the receive address to move the coin into Gateway escrow. The receive address is owned by a Rails-managed keypair and holds the coin for the few seconds between detection and on-chain forwarding — a brief in-flight custody window that is unavoidable for exchange-sourced payments. Each receive address is per-order and never reused.

The integrator can suppress either option with the optional request field `pay_from`:

```json
{
  ...,
  "pay_from": "self_custody"      // returns only the ptb method
  // or "external_wallet"          // returns only the receive_address method
  // or "both" (default)           // returns both
}
```

`route_preference: "auto"` lets the engine pick (defaults to Route B if LP liquidity available within ceiling, else falls back to Route A treasury). Explicit values force the route.

### Get order
```http
GET /v1/orders/{order_id}
```
Returns the same shape as the create response, plus current status, on-chain refs (`sui_tx_hash`, `bsc_tx_hash` if Route A), and `settlement_tx_id` (BaaS reference for the fiat payout).

### List orders
```http
GET /v1/orders?status=settled&limit=50&cursor=...
```
Cursor-paginated.

### Quote (no commitment)
```http
POST /v1/quotes
{ "coin": "USDC", "amount": "100", "fiat_currency": "NGN", "route_preference": "auto" }
```
Returns current rate, fees, ETA per available route. Doesn't reserve anything. Useful for UI rate displays. Quote valid for 30s.

### Supported currencies
```http
GET /v1/currencies
```
Lists currencies, supported institution codes per currency, current ceiling rate.

### Webhook management
```http
POST /v1/webhooks
GET /v1/webhooks
DELETE /v1/webhooks/{id}
POST /v1/webhooks/{id}/rotate-secret
```

---

## Order Status Lifecycle

```
awaiting_deposit       user hasn't paid Sui yet
       │
       ▼ (Sui Gateway OrderCreated indexed)
deposited              Sui locked in escrow
       │
       ▼ (Route B: matched / Route A: bridge initiated)
processing
       │
       ▼ (fiat sent via BaaS or banking partner)
fulfilled
       │
       ▼ (BaaS webhook confirms payout)
validated
       │
       ▼ (on-chain settle_order succeeds)
settled

Alternate terminal states:
- expired       user didn't deposit by expires_at
- refunded      bridge failed / slippage exceeded / no LP available
- cancelled     integrator-initiated cancel before deposit
```

---

## Webhooks

### Delivery
- HTTPS only.
- POST to integrator-provided URL.
- Timeout 10s.
- Retry on 5xx or timeout: at 30s, 2min, 10min, 1h, 6h, 24h. After last attempt fails, webhook marked `failed` and visible in the integrator dashboard for manual replay.

### Signature
Header: `X-Rails-Signature: t=1700000000,v1=hex_hmac_sha256`
- `t` = unix timestamp at send.
- `v1` = `hmac_sha256(secret, "t={t}.{body}")`.
- Integrators must verify by recomputing and reject if `t` is older than 5 minutes (replay protection).

### Event types
| Event | When |
|---|---|
| `order.deposited` | Sui Gateway `OrderCreated` indexed; coin in escrow. |
| `order.processing` | Matching succeeded / Route A bridge in flight. |
| `order.fulfilled` | Fiat disbursement sent to merchant. |
| `order.validated` | BaaS confirmed payout. |
| `order.settled` | On-chain `settle_order` succeeded. |
| `order.refunded` | Coin returned to refund_address. |
| `order.failed` | Terminal failure (no LP, bridge dead, etc.). |
| `order.expired` | User didn't deposit in time. |

### Webhook payload shape
```json
{
  "event": "order.settled",
  "event_id": "evt_xyz",
  "created_at": "2026-05-21T17:05:00Z",
  "data": {
    "order_id": "ord_abc123",
    "status": "settled",
    "metadata": { "integrator_ref": "..." },
    "sui_tx_hash": "0x...",
    "settlement_tx_id": "psp_ref_...",
    "fiat_amount": "153050.00",
    "fiat_currency": "NGN"
  }
}
```

`event_id` is unique per event — integrators dedupe on it (we may resend on transient errors).

---

## Idempotency

`Idempotency-Key` header on `POST /v1/orders`, `POST /v1/quotes`, `POST /v1/webhooks`, `POST /v1/webhooks/.../rotate-secret`.

- Server stores `{key → response}` for 24h.
- Replay returns the original response (200 status echoed even if cached).
- Replays with the same key but different body return `409 IDEMPOTENCY_KEY_MISMATCH`.

---

## Error Model

```json
{
  "error": {
    "code": "INSUFFICIENT_LP_LIQUIDITY",
    "message": "No LP available within ceiling rate for NGN at requested amount.",
    "request_id": "req_...",
    "details": {
      "currency": "NGN",
      "amount": "100.00",
      "ceiling_rate": "1545.81"
    }
  }
}
```

### Error codes
| Code | HTTP | Meaning |
|---|---|---|
| `UNAUTHENTICATED` | 401 | Missing/invalid API key. |
| `FORBIDDEN` | 403 | Key lacks required scope. |
| `INVALID_REQUEST` | 400 | Bad payload (validation details in `details`). |
| `UNSUPPORTED_CURRENCY` | 400 | Currency not configured. |
| `UNSUPPORTED_COIN` | 400 | Coin not in Gateway's supported set. |
| `RECIPIENT_VALIDATION_FAILED` | 400 | Bank account doesn't validate via BaaS pre-check. |
| `INSUFFICIENT_LP_LIQUIDITY` | 503 | No matchable LP within ceiling. Try `route_preference: "route_a_treasury"`. |
| `BRIDGE_UNAVAILABLE` | 503 | LiFi quote failed, all bridges down. |
| `MATCHING_PAUSED` | 503 | Operator paused matching for currency. |
| `IDEMPOTENCY_KEY_MISMATCH` | 409 | Replay with different body. |
| `RATE_LIMITED` | 429 | Per-key throttle exceeded. Includes `Retry-After`. |
| `INTERNAL_ERROR` | 500 | Generic; correlate with `request_id` for support. |

---

## Rate Limiting

- Default: 60 requests/min per API key, burst 120.
- `POST /v1/orders` and `POST /v1/quotes` count against the limit.
- `GET /v1/orders/{id}` cheaper, separate bucket (300/min).
- Headers: `X-RateLimit-Remaining`, `X-RateLimit-Reset`.

---

## Sandbox Environment

- Base URL: `https://sandbox.api.rails.dev` (subdomain TBD).
- API keys: `rk_test_...`.
- Mock LP responds instantly to all orders (within ceiling).
- Mock BaaS auto-confirms payouts.
- Mock LiFi bridge resolves in 5s.
- Mock Sui chain: testnet, with a faucet for the coin types we support.
- Webhook deliveries to integrator's `webhook_url` work normally — test against your real handler.

---

## SDK Plan

v1 ships with:
- **OpenAPI 3.1 spec** at `https://api.rails.dev/openapi.json` (auto-generated from controllers).
- **TypeScript SDK** generated from OpenAPI; published to npm.
- **Go SDK** hand-written for ergonomics (auto-generated Go from OpenAPI is awkward).
- Postman collection.

Other language SDKs deferred until integrator demand justifies.

---

## Observability

- `request_id` returned in every response header and error body.
- Webhook deliveries logged with response code + latency, viewable in integrator dashboard.
- Order timeline endpoint: `GET /v1/orders/{id}/events` returns the sequence of state transitions + timestamps.

---

## Open Items

- **API key scoping granularity:** v1 ships with coarse scopes (`orders:read`, `orders:write`). Per-currency or per-route scoping deferred until enterprise integrators ask.
- **Multi-tenant idempotency isolation:** Idempotency keys are scoped per API key (not global). Confirmed acceptable.
- **OAuth / OIDC for integrator users:** v1 is API-key only. OAuth deferred.
- **gRPC option:** Not in v1. Revisit if a high-volume integrator asks.
- **Subscription / streaming events:** WebSocket order updates deferred to v2.
