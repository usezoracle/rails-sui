# Route A — Bridging Spec

## Purpose

Convert Sui stablecoins → BSC USDC via LiFi, then deliver fiat to the merchant through either the existing EVM LP system (`mode: "lp"`) or our centralized OTC treasury (`mode: "treasury"`). The integrator chooses per transaction.

This is the "resilience layer" — when Route B (Sui LP supply) is thin, expensive, or unavailable for a corridor, Route A absorbs the flow without changing the integrator's API surface.

---

## API Entry Point

```http
POST /v1/orders/route-a
Authorization: Bearer {api_key}
Idempotency-Key: {uuid}
Content-Type: application/json

{
  "coin": "USDC",
  "amount": "100.00",
  "fiat_currency": "NGN",
  "recipient": {
    "institution_code": "044",
    "account_number": "0123456789",
    "account_name": "Jane Doe"
  },
  "mode": "lp",          // or "treasury"
  "refund_address": "0x...sui-address...",
  "webhook_url": "https://integrator.example.com/rails/webhook"
}
```

Response:

```json
{
  "order_id": "ord_abc123",
  "pay_to": {
    "chain": "sui",
    "object_id": "0x...sui-shared-order-object...",
    "amount": "100.00",
    "coin_type": "0x...::usdc::USDC"
  },
  "expires_at": "2026-05-21T17:00:00Z",
  "status": "awaiting_deposit"
}
```

The user pays Sui to the order's `pay_to.object_id` via the Move Gateway's `create_order`. Everything after that is Rails-internal.

---

## Flow Per Mode

### `mode: "lp"` — Re-enter the EVM Gateway on BSC

```
[1] User pays Sui          [2] Indexer sees     [3] Rails requests
    via create_order  ───▶     OrderCreated     ───▶   LiFi quote
    (Sui Gateway)              on Sui                  Sui→BSC USDC

[4] LiFi executes          [5] BSC USDC lands   [6] Rails calls
    cross-chain swap  ───▶    in Rails hot      ───▶   createOrder on
                              wallet on BSC          existing legacy
                                                      EVM Gateway

[7] Existing EVM LP        [8] EVM LP settles   [9] Rails calls
    matching engine   ───▶    BSC order, fiat   ───▶   settle_order on
    picks BSC LP              hits merchant            Sui Gateway
                                                       (LP wallet =
                                                       our hot wallet)

[10] Sui USDC released     [11] Rails fires
     to Rails hot wallet ─▶    order.settled
     (closes the loop)         webhook to
                               integrator
```

The Sui order is the source of truth for the integrator. The BSC order is an internal sub-order used to source the fiat. When the BSC order settles (fiat delivered), Rails settles the Sui order to its own hot wallet, recovering the deposited stablecoin (now sitting as BSC USDC in the hot wallet, having paid the fiat via the BSC LP).

Net economic outcome: integrator's user is debited Sui USDC; merchant is credited fiat; Rails treasury is roughly neutral (small inventory swing managed by treasury ops).

### `mode: "treasury"` — Centralized OTC

```
[1] User pays Sui          [2] Indexer sees     [3] Rails requests
    via create_order  ───▶     OrderCreated     ───▶   LiFi quote
    (Sui Gateway)              on Sui                  Sui→BSC USDC

[4] LiFi executes          [5] BSC USDC lands   [6] Rails triggers
    cross-chain swap  ───▶    in Rails hot      ───▶   banking partner
                              wallet on BSC          payout to merchant
                                                      from treasury float

[7] Banking partner        [8] Rails calls
    confirms payout    ───▶   settle_order on
    (tx_id captured)          Sui Gateway (LP =
                              Rails hot wallet)

[9] Sui USDC released      [10] Rails fires
    to Rails hot wallet ─▶    order.settled
                              webhook
```

Same Sui-side mechanics. Difference: fiat is paid directly from our treasury, not sourced through an LP. Faster (no LP matching latency), more reliable (no LP availability risk), more expensive (treasury cost of capital + banking partner fees).

---

## Sui-side coin handoff (open design choice, decide in Phase 1)

Route A needs the user's coin to end up in our Sui hot wallet so we can hand it to LiFi for bridging. Two valid Move-package designs achieve this. Decision deferred to Phase 1 when we have concrete Sui gas-cost data.

### Option A — Same `create_order` entry as Route B, then self-settle

User signs the same PTB regardless of route. Coin lands in the standard Order shared object. Backend immediately calls `settle_order(... liquidity_provider: <route_a_hot_wallet>, settle_percent: 10_000 ...)` from the aggregator wallet, releasing the coin to our Sui hot wallet. Then LiFi from there.

```
user signs create_order PTB
  → coin → Order shared object (Gateway escrow)
  → backend self-settle to Rails Sui hot wallet
  → LiFi bridge → BSC
```

- **Pros:** one entry function, one user-facing PTB shape, simpler Move package, route choice can be made AFTER deposit (we can decide Route A vs Route B based on real-time LP availability).
- **Cons:** one extra on-chain transaction per Route A order (gas overhead), and the Order shared object briefly exists holding escrow that's immediately drained.

### Option B — Dedicated `create_route_a_order` entry that puts coin directly in bridge wallet

A separate Move entry function: takes coin from user, transfers it directly to the Rails bridge hot wallet, emits a `RouteAOrderCreated` event. No Order shared object created.

```
user signs create_route_a_order PTB
  → coin → Rails Sui hot wallet (atomic)
  → LiFi bridge → BSC
```

- **Pros:** cheaper (one tx vs two), more honest about where the coin is going (the user sees in their wallet "this PTB transfers coin to Rails," not "to Gateway escrow").
- **Cons:** two entry paths in the Move package, route choice must be known at PTB construction time (no after-the-fact route switching), Route A loses the Gateway escrow's settlement and refund event mirroring.

### Provisional default

Document Option A as the working assumption for the spec until Phase 1 measures Sui gas costs and we make the final call. If gas-per-tx is cheap enough that the extra step doesn't materially affect economics, Option A wins on simplicity. If gas matters, switch to Option B.

---

## LiFi Integration

### Sui chain facts (verified against `li.quest/v1/chains?chainTypes=MVM`)
| Field | Value |
|---|---|
| `chainId` | `9270000000000000` |
| `key` | `sui` |
| `chainType` | `MVM` |
| Native gas token | `SUI` (`0x2::sui::SUI`, 9 decimals) |
| Mainnet RPC | `https://fullnode.mainnet.sui.io:443` |
| Token reference format | Move type string `0x<package>::<module>::<TYPE>` |

To list LiFi-supported tools/bridges for Sui: `GET /v1/tools?chains=9270000000000000`.

### Client surface

```go
// services/lifi/client.go
type Client struct {
    apiKey   string
    baseURL  string   // https://li.quest/v1
    http     *fastshot.Client
}

type Quote struct {
    Tool         string  // bridge provider chosen by LiFi (e.g. "wormhole", "mayan")
    FromAmount   string
    ToAmount     string
    EstimatedFee string
    // For Sui as source: TransactionRequest.Data is base64-encoded tx bytes (decode → sign → submit).
    // For EVM as source: TransactionRequest carries {to, data, value, gas...}.
    TransactionRequest TransactionRequest
    ETA          int     // seconds
}

type TransactionRequest struct {
    Data     string  // base64 (MVM/SVM) OR hex-encoded calldata (EVM)
    To       string  // EVM only
    Value    string  // EVM only
    GasPrice string  // EVM only
    GasLimit string  // EVM only
}

func (c *Client) GetQuote(ctx context.Context, req QuoteRequest) (*Quote, error)
func (c *Client) ExecuteBridgeSui(ctx context.Context, quote *Quote, signer SuiSigner) (*BridgeTx, error)
func (c *Client) MonitorStatus(ctx context.Context, txHash string) (*BridgeStatus, error)
```

### Quote request shape

LiFi exposes a single `/v1/quote` endpoint. Sui is identified by chain ID `9270000000000000` (key `sui`, chainType `MVM`). Tokens on Sui are referenced by Move type strings, **not** EVM-style addresses.

```
GET https://li.quest/v1/quote
  ?fromChain=9270000000000000
  &toChain=56
  &fromToken=0x...::usdc::USDC                  # Sui Move type
  &toToken=0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d  # BSC USDC address
  &fromAmount=100000000
  &fromAddress=0x...rails-sui-hot-wallet...
  &toAddress=0x...rails-bsc-hot-wallet...
  &slippage=0.003
```

Equivalent POST body shape is also accepted.

### Execution

LiFi returns a `transactionRequest` per step. For non-EVM source chains (Sui, Solana), the shape diverges from EVM:

| Chain type | `transactionRequest` shape |
|---|---|
| EVM source | `{ to, data, value, gasPrice, gasLimit }` — directly callable via `eth_sendTransaction` |
| MVM source (Sui) | `{ data: "<base64-encoded-tx-bytes>" }` — client decodes, signs with Sui key, submits via Sui RPC |
| SVM source (Solana) | Same `{ data: "<base64>" }` pattern as Sui |

Execution path in our backend:
1. `lifi.GetQuote(ctx, req)` → returns quote with `transactionRequest.data` (base64).
2. Decode base64 → bytes.
3. Pass bytes to the Sui RPC client wrapper: parse into a Sui `TransactionData`, sign with our hot wallet's `Ed25519Keypair`, submit via `sui_executeTransactionBlock`.
4. Capture the on-chain digest as `RouteAOrder.bridge_tx_sui`.

**Unverified in LiFi public docs:**
- Whether the base64 blob is a fully-built `TransactionData` (we only sign) or a programmable transaction block we have to further compose. Treat as fully-built until proven otherwise; the Solana parallel suggests fully-built.
- Sponsored-transaction support for Sui (we cover gas in the v1 design either way).
- The exact bridge tool LiFi picks for Sui↔EVM (likely Wormhole NTT or Mayan; should not matter to our integration as long as the quote → execute → status contract is stable).

Confirm both during Phase 6 (LiFi integration) with a devnet bridge test before committing to the abstraction.

### Status polling

After source-chain submission, poll `GET /status?txHash={tx}` every 30s (LiFi's recommended cadence) until status = `DONE` or `FAILED`. Persist polling state in DB so a restart doesn't lose track of in-flight bridges.

### Slippage policy

Default `slippage: 0.003` (0.3%). If the user-quoted Sui amount minus slippage minus LiFi fees results in less-than-expected BSC USDC, the order's `rate` becomes the actual delivered amount divided by fiat amount. If this exceeds the Ceiling Rate, the order is auto-refunded (see Failure Handling).

---

## Database Schema Additions

```
RouteAOrder
  id              string (PK)
  order_id        string  → LockPaymentOrder.id
  mode            enum    "lp" | "treasury"
  lifi_quote_id   string
  bridge_tx_sui   string  source-chain tx hash
  bridge_tx_bsc   string  dest-chain tx hash (filled when bridge completes)
  bridge_status   enum    "pending" | "bridging" | "bridged" | "dispatching" | "settled" | "failed"
  bsc_order_id    string  Legacy EVM Gateway order ID (only when mode=lp)
  treasury_payout_ref string banking partner tx ref (only when mode=treasury)
  bridged_amount  decimal actual BSC USDC received
  failure_reason  string  nullable
  created_at, updated_at
```

State transitions:
```
pending ─▶ bridging ─▶ bridged ─▶ dispatching ─▶ settled
                                              └▶ failed (reconciliation)
```

---

## Failure Handling

### Source-chain submission fails
Sui tx never lands. `RouteAOrder.bridge_status = "failed"`. User's original `create_order` on Sui never happened — there's nothing to refund. Webhook `order.failed` sent.

### LiFi bridge fails after Sui deposit
User has paid Sui (it's in the Order shared object's escrow). Bridge fails to deliver BSC USDC. Reconciliation cron:
1. Mark `RouteAOrder.bridge_status = "failed"`.
2. Call `refund_order` on the Sui Gateway (aggregator-signed) to return Sui USDC to the sender's `refund_address`.
3. Send `order.refunded` webhook with `reason: "bridge_failed"`.

### LP dispatch fails (mode=lp, BSC Gateway rejects)
BSC USDC is sitting in the hot wallet. Options:
1. Retry the BSC `createOrder` with adjusted parameters.
2. Fall back to `mode: "treasury"`: use the bridged USDC to pay merchant from treasury, then sweep the BSC USDC to treasury float.

Default: retry 3x, then auto-fallback to treasury. Configurable per-integrator via API key settings.

### Treasury payout fails (mode=treasury, banking partner error)
BSC USDC is in hot wallet, fiat not sent. Manual ops intervention: investigate banking partner error, retry payout, or initiate refund chain (BSC USDC → bridge back → Sui refund).

### Slippage exceeds ceiling
Bridge succeeds but delivered amount makes effective rate worse than the Ceiling Rate. Order auto-refunded as above. Webhook `order.refunded` with `reason: "slippage_exceeded_ceiling"`.

### Stuck bridge (LiFi status = `PENDING` for too long)
Cron job runs every 5 min. Any `RouteAOrder` in `bridging` state for > 30 min is escalated:
1. Re-check LiFi status one more time.
2. If still pending: alert ops via email/slack. Do not auto-refund (bridge may still complete).

---

## Reconciliation Cron

Runs every 60s:
- Find all `RouteAOrder` in `bridging` → poll LiFi status, advance to `bridged` or `failed`.
- Find all `RouteAOrder` in `bridged` → kick off dispatch (call BSC Gateway or trigger treasury payout).
- Find all `RouteAOrder` in `dispatching` for `mode=lp` → poll BSC Gateway order status, advance to `settled` when BSC order is fulfilled, then trigger Sui-side settlement.
- Find all `RouteAOrder` in `dispatching` for `mode=treasury` → poll banking partner status, advance to `settled` when payout confirmed, then trigger Sui-side settlement.

Cron lives at `tasks/route_a_reconciliation.go`.

---

## Open Items

- **Hot wallet on BSC for inbound bridged USDC.** Needs to be funded with BNB for gas; auto-topup cron.
- **Hot wallet on Sui** for bridge-source signing. Needs SUI for gas; auto-topup cron parallel to BSC one.
- **Treasury float sizing per corridor.** v1: start with $50k float per supported currency (NGN, KES, IDR), revisit after volume data.
- **LiFi API key + rate limits.** Need to apply for production access; sandbox is free.
- **BSC LP availability monitoring.** When BSC LP supply for a corridor drops below threshold, auto-disable `mode=lp` for that corridor (force `mode=treasury`).
- **Banking partner for treasury payouts.** Same decision-space as Virtual Accounts spec — likely same provider serves both purposes (BaaS that does both virtual accounts and treasury payouts).
- **Confirm Sui transactionRequest shape** during Phase 6 spike: is `data` a complete `TransactionData` we only sign, or a PTB we must compose further? Affects how thin the Sui signing wrapper is.
- **Sponsored Sui transactions.** Sui supports gas sponsorship natively. If LiFi's returned tx supports a separate gas payer, we could centralize gas via our hot wallet for users on Route A (relevant if Route A ever runs against user wallets rather than just our hot wallet).

---

## References

- LiFi Sui overview: <https://docs.li.fi/introduction/lifi-architecture/sui-overview>
- LiFi chains catalog (live): <https://li.quest/v1/chains?chainTypes=MVM>
- LiFi non-EVM tx execution pattern (Solana, mirrors Sui): <https://docs.li.fi/introduction/user-flows-and-examples/solana-tx-execution>
- LiFi quote endpoint: <https://docs.li.fi/api-reference/get-a-quote-for-a-token-transfer>
