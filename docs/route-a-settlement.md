# Route A — settlement Gateway fulfillment (BSC)

Status: **DRAFT — awaiting approval before scaffolding**
Last updated: 2026-05-22
Owner: see `docs/route-a-spec.md` for the surrounding pipeline.

## 0. One-sentence summary

After the Sui→BSC bridge in Route A reports DONE, Rails turns around
on BSC and calls settlement's on-chain `Gateway.createOrder` with the
bridged USDC. settlement's Aggregator indexes the event, dispatches one
of their Provision Nodes to pay the recipient's bank/MoMo account,
and we learn of completion by polling `GET /v1/orders/{chainId}/{orderId}`
and pushing status changes to the PWA over SSE.

## 1. Goal & non-goals

**Goal.** Wire `dispatchLP()` in `services/route_a_dispatcher.go` to
the settlement Gateway so `RouteAOrder.bridge_status` moves
`bridged → dispatching → settled` end-to-end without manual touch.

**Non-goals.**

- Building our own fiat rail. settlement's Provision Nodes own the
  bank-payout leg.
- Refunds-on-rails (v1). If settlement reports `refunded`, USDC lands
  back in our BSC wallet — manual reverse-bridge in v1.
- Smart-wallet / paymaster integration. Server-side wallet pays BNB.
- Subgraph / EVM event indexer. Polling for v1; event indexer can
  ship in v1.5 if the poll budget becomes a problem.
- On-ramp (USD-in → crypto-out). settlement supports it; out of scope.

## 2. Where this slots into the existing pipeline

```
   ┌─────────────────────────────────────────────────────────────────┐
   │                       services/route_a_dispatcher.go            │
   │   (already exists — gocron tick every 1 min)                    │
   ├─────────────────────────────────────────────────────────────────┤
   │                                                                  │
   │   advancePending      →  Sui USDC self-settles to aggregator    │
   │   advanceBridging     →  poll LiFi → BSC USDC at aggregator     │
   │   advanceBridged      →  ◆ NEW: dispatchLP (settlement)           │
   │   advanceDispatching  →  ◆ NEW: poll settlement /orders → settle  │
   │                                                                  │
   └─────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │                      services/evm/  (NEW)                        │
   │   • ethclient + signer + nonce manager                           │
   │   • abigen-generated Gateway + ERC-20 bindings                   │
   │   • chain-config driven (BSC testnet ↔ mainnet on one env flip)  │
   └─────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │                   services/settlement/  (NEW)                      │
   │   • HTTP client for /pubkey, /orders/{chain}/{id}                │
   │   • RSA-PKCS1v15 messageHash builder                             │
   │   • pubkey cache with TTL                                        │
   └─────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
                  ┌──────────────────────────────┐
                  │    api.paycrest.io (HTTPS)   │
                  │    BSC Gateway (on-chain)    │
                  └──────────────────────────────┘
                                  │
                                  ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │                  controllers/orders/sse.go (NEW)                 │
   │   GET /v1/orders/:id/events  → SSE stream of bridge_status      │
   └─────────────────────────────────────────────────────────────────┘
```

## 3. Module layout

```
services/
  evm/
    client.go         ethclient.Client + chain config + signer
    gateway.go        abigen-generated settlement Gateway binding
    erc20.go          abigen-generated ERC-20 binding
    config.go         chainId → {RPC, GatewayAddr, USDC, USDCDec, signerKey}
    abigen.go         //go:generate directive
    abi/Gateway.json  ABI committed to repo (regenerable from Etherscan)
    abi/ERC20.json    standard ERC-20 ABI
  settlement/
    client.go         FetchPubkey, FetchOrderStatus (HTTP)
    pubkey_cache.go   PEM cached with TTL (default 1h)
    recipient.go      EncryptRecipient: crypto/rsa.EncryptPKCS1v15 + base64
    types.go          OrderStatus enum mirroring aggregator response

ent/schema/route_a_order.go     +gateway_order_id (hex bytes32, indexed)
                                +gateway_chain_id (uint64)
                                +sender_fee_subunit (decimal, persisted)

config/order.go                 +BSC RPC config block (see §6)
controllers/orders/sse.go       GET /v1/orders/:id/events
tasks/tasks.go                  no changes — existing 1-min tick covers
                                advanceDispatching since it's added to
                                RouteADispatcher.Tick()

docs/
  route-a-settlement.md           this doc
  route-a-spec.md               cross-reference added in failure section
```

## 4. State machine

`RouteAOrder.bridge_status` is the single source of truth.
`PaymentOrder.status` stays as-is (initiated → pending → settled /
expired / refunded), reflecting the user-facing lifecycle.

```
   pending ─advancePending──▶  bridging
   bridging ─advanceBridging─▶ bridged          (LiFi DONE)
                              │
                              ▼  dispatchLP succeeds
   bridged ──────────────────▶ dispatching      (settlement orderId persisted)
                              │
                              ▼  advanceDispatching polls settlement
   dispatching ─{settled}────▶ settled          ── PaymentOrder.status = settled
   dispatching ─{refunded}───▶ refunded         ── ops ticket; manual unwind
   dispatching ─{expired}────▶ failed           ── ops ticket; manual unwind
```

Transitions are all DB-driven via Ent. No new fields on PaymentOrder.

## 5. ABI + addresses

Single ABI file committed under `services/evm/abi/Gateway.json`,
regenerable from BscScan (mainnet) or BscScan-testnet via:

```
forge inspect Gateway abi  # if we ever vendor the contracts repo
# OR
curl https://api.bscscan.com/api?module=contract&action=getabi&address=<addr>
```

### `createOrder` signature

```
function createOrder(
  address _token,
  uint256 _amount,
  uint96  _rate,
  address _senderFeeRecipient,
  uint256 _senderFee,
  address _refundAddress,
  string  messageHash
) external returns (bytes32 orderId);
```

### Events we care about

```
event OrderCreated(
  address indexed sender,
  address indexed token,
  uint256 indexed amount,
  uint256 protocolFee,
  bytes32 orderId,
  uint256 rate,
  string  messageHash
);
event OrderSettled (bytes32 splitOrderId, bytes32 indexed orderId, address indexed liquidityProvider, uint96 settlePercent);
event OrderRefunded(uint256 fee, bytes32 indexed orderId);
```

We only need the `OrderCreated` log to capture `orderId` (matched by
(sender, token, amount) — all indexed). `OrderSettled` / `OrderRefunded`
are observed *off-chain* via settlement's `/orders` endpoint in v1.

### Per-chain address table

```
chainId | name        | Gateway                                              | USDC                                                  | USDC dec
--------+-------------+------------------------------------------------------+-------------------------------------------------------+---------
56      | BSC mainnet | 0x1fa0ee7f9410f6fa49b7ad5da72cf01647090028           | 0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d            | 18
97      | BSC testnet | (set BSC_TESTNET_GATEWAY env once known)             | (set BSC_TESTNET_USDC env)                            | 18
```

Note BSC USDC is **18 decimals** (Binance-Peg ERC-20), not 6. Subunit
math through the EVM layer uses 18; subunit math on the Sui side stays
6. The dispatcher must convert at the bridge boundary
(`SuiAmount6 → BscAmount18 = SuiAmount6 * 1e12`).

## 6. Config additions

```ini
# config/server.go and config/order.go consume via viper
EVM_NETWORK                       = bsc-testnet            # bsc-testnet | bsc-mainnet
BSC_RPC_URL                       = https://data-seed-prebsc-1-s1.binance.org:8553
BSC_AGGREGATOR_ADDRESS            = 0x…                    # already exists; reused
BSC_SIGNER_KEY                    = 0x…                    # NEW; hex private key
BSC_SENDER_FEE_BPS                = 50                     # 0.5%; sender fee skim
BSC_GAS_LIMIT_CREATE_ORDER        = 350000
BSC_GAS_PRICE_GWEI                = 3                      # BSC default; override for spikes
BSC_BNB_LOW_THRESHOLD_WEI         = 50000000000000000      # 0.05 BNB → alert
BSC_TESTNET_GATEWAY               = 0x…
BSC_TESTNET_USDC                  = 0x…
BSC_MAINNET_GATEWAY               = 0x1fa0ee7f9410f6fa49b7ad5da72cf01647090028
BSC_MAINNET_USDC                  = 0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d

SETTLEMENT_API_URL                  = https://api.paycrest.io  # already exists
SETTLEMENT_PUBKEY_CACHE_TTL_SECONDS = 3600
SETTLEMENT_SENDER_API_KEY_ID        = <UUID>                   # see §9
SETTLEMENT_POLL_INTERVAL_SECONDS    = 30                       # advanceDispatching cadence
```

`EVM_NETWORK` is the single flip-switch. Going to mainnet is one env
change + a wallet swap, nothing else.

## 7. `messageHash` construction (Go)

```go
// services/settlement/recipient.go
type Recipient struct {
    AccountIdentifier string                 `json:"accountIdentifier"`
    AccountName       string                 `json:"accountName"`
    Institution       string                 `json:"institution"`
    Memo              string                 `json:"memo,omitempty"`
    ProviderID        string                 `json:"providerId,omitempty"`
    Nonce             string                 `json:"nonce"`
    Metadata          map[string]string      `json:"metadata"`  // {"apiKey": senderId}
}

func EncryptRecipient(r Recipient, pem string) (string, error) {
    plain, err := json.Marshal(r)
    if err != nil { return "", err }
    block, _ := pem.Decode([]byte(pem))
    pub, err := x509.ParsePKIXPublicKey(block.Bytes)
    if err != nil { return "", err }
    rsaPub, ok := pub.(*rsa.PublicKey)
    if !ok { return "", errors.New("settlement: pubkey is not RSA") }
    cipher, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, plain)
    if err != nil { return "", err }
    return base64.StdEncoding.EncodeToString(cipher), nil
}
```

`Nonce` = 12 chars of `time-now-base36 + rand-base36` (matches noblocks).

## 8. Sender fee (0.5%)

`senderFee` is denominated in token subunits (BSC USDC = 18 dec). For
`amount = 100 USDC` and `BSC_SENDER_FEE_BPS = 50`:

```
amount     = 100_000000000000000000   // 100 USDC in 18-dec subunits
senderFee  = amount * 50 / 10000      // 0.5%
           = 500_000000000000000      // 0.5 USDC
```

The total `approve(GATEWAY, amount + senderFee)` call is `100.5 USDC`.
The recipient receives fiat for `amount` (100 USDC at the locked rate);
`senderFee` goes to `senderFeeRecipient = BSC_AGGREGATOR_ADDRESS` (us).

We persist `RouteAOrder.sender_fee_subunit` for accounting.

## 9. Sender attribution (no settlement account)

`messageHash.metadata.apiKey` identifies the sender of the order. With
no registered settlement sender account in v1:

- Generate a stable UUID at first boot, store it in `.env.local` as
  `SETTLEMENT_SENDER_API_KEY_ID`. Doesn't matter what it is — settlement
  uses it only for analytics + LP attribution; no auth check.
- Document in the env: "Register the project at https://paycrest.io and
  replace this with the real sender UUID for proper attribution + LP
  routing preferences."
- Operational note: with a real sender account, you can set a
  per-sender `providerId` to pin orders to a known-good Provision
  Node. v1 leaves `providerId` unset → random LP within rate ceiling.

## 10. Gas (BNB) funding ops

Server-side wallet (no paymaster). Op cost on BSC:

```
approve()      ~ 46_000  gas → ~$0.03 at 3 gwei + BNB ~ $600
createOrder()  ~ 280_000 gas → ~$0.20 at 3 gwei + BNB ~ $600
total per fulfillment ~ $0.23
```

- **Initial funding:** 1 BNB (~$600) gets us ~2,600 orders.
- **Low-balance alert:** `BSC_BNB_LOW_THRESHOLD_WEI` = 0.05 BNB. New
  cron in `tasks/tasks.go` ticks every 5 min, checks
  `evmClient.BalanceAt(ctx, signerAddr)`, posts to Slack via existing
  `services/notifications` when below threshold.
- **Auto-top-up:** out of scope for v1. Manual transfer from cold wallet.

## 11. SSE design — `/v1/orders/:id/events`

Per-order live status push for the PWA. Implementation outline:

```
controllers/orders/sse.go
  GET /v1/orders/:id/events
    auth: existing PaymentOrder ownership check
    response: text/event-stream
    behaviour:
      1. fetch current PaymentOrder + RouteAOrder snapshot, emit once
      2. subscribe to a Postgres LISTEN/NOTIFY channel keyed by order_id
      3. on each NOTIFY, refetch snapshot, emit { status, bridge_status,
         tx_hash, gateway_order_id, updated_at }
      4. heartbeat ":\n\n" every 25s to keep proxies open
      5. close on client disconnect; goroutine cleanup

route_a_dispatcher.advanceDispatching
  on each state transition: pg_notify("order:<id>", "")
  (already standard pattern in Rails — extract a tiny helper if not yet)
```

Why Postgres LISTEN/NOTIFY: zero new infra, fan-out across server
instances for free. Redis Pub/Sub is an alternative if we move to
multi-region.

Client side is already wired in `app/order/[id]/page.tsx` (uses
React Query polling). Swap to native `EventSource` once SSE lands.

## 12. Failure modes + recovery

| Failure | Symptom | Recovery |
|---|---|---|
| BSC RPC down | `dispatchLP` errors at gas estimation | Tick retries every minute. After 5 consecutive failures, post to Slack. Order stays `bridged`. |
| `approve` reverts | tx receipt `status=0` | Treat as transient; retry next tick. After 3 retries, mark `RouteAOrder.bridge_status = failed`, ops ticket. |
| `createOrder` reverts | tx receipt `status=0` | Same as above. Common causes: rate outside settlement's ceiling, token not whitelisted. |
| `OrderCreated` log missing | tx confirmed but no log | Cross-check `Gateway.getOrderInfo` view fn. If returns zeroes, treat as failed-but-funds-still-ours (USDC was not transferred — `createOrder` reverts atomically). |
| settlement `/pubkey` 5xx | can't build messageHash | Use cached pubkey (until TTL); if no cache, retry next tick. |
| settlement `/orders` 5xx | can't poll | Retry next tick. Stale `dispatching` for >30 min → ops alert. |
| settlement reports `refunded` | USDC bounced back to `_refundAddress` (us) | Set `bridge_status = refunded`, mark `PaymentOrder.status = refunded`, ops ticket. v2: auto reverse-bridge to user's Sui address. |
| settlement reports `expired` | order never fulfilled, refunded automatically | Same as above. |
| BNB low | gas estimation throws or tx fails | Slack alert, hold dispatches. |
| Wallet nonce desync | `nonce too low` / `nonce too high` | Read on-chain nonce, reset in-process counter. Standard go-ethereum pattern. |

## 13. Reconciliation cron

`route_a_dispatcher.Tick()` already runs every 1 min. Add a single new
phase:

```go
func (d *RouteADispatcher) Tick(ctx context.Context) error {
    // existing
    if err := d.advancePending(ctx);     err != nil { logger.Error(err) }
    if err := d.advanceBridging(ctx);    err != nil { logger.Error(err) }
    if err := d.advanceBridged(ctx);     err != nil { logger.Error(err) }   // now calls dispatchLP
    // NEW
    if err := d.advanceDispatching(ctx); err != nil { logger.Error(err) }
    return nil
}
```

`advanceDispatching` scans `RouteAOrder where bridge_status='dispatching'`,
calls `settlementClient.FetchOrderStatus(chainId, gatewayOrderID)`,
transitions on settled/refunded/expired.

## 14. Build order

Each step ships something verifiable on its own. Don't merge until the
prior step is green.

1. **`services/evm/`** with `ethclient` + `abigen` bindings.
   Standalone unit test that reads `Gateway.getOrderInfo` for a known
   on-chain order. No state changes anywhere else.
2. **`services/settlement/`** with `FetchPubkey` + `FetchOrderStatus` +
   `EncryptRecipient`. Test against the real `api.paycrest.io` v1
   pubkey + a known testnet order id.
3. **Schema migration**: add `gateway_order_id`, `gateway_chain_id`,
   `sender_fee_subunit` to `RouteAOrder`. Atlas migration via `atlas.hcl`.
4. **Wire `dispatchLP`**. Behind a feature flag `ROUTE_A_SETTLEMENT_ENABLED`
   (default off) until BSC testnet wallet + settlement pubkey confirmed.
5. **Wire `advanceDispatching`**. Same flag.
6. **SSE endpoint**. Independent — works against polled status today.
7. **Low-BNB alert cron**. Independent.
8. **Flip the flag in dev**. Run a full end-to-end dry-run on BSC
   testnet with a small amount. Confirm event log capture, status
   transitions, SSE push.
9. **Documentation**: ops runbook for refund handling (manual reverse-
   bridge steps).
10. **Mainnet**: flip `EVM_NETWORK=bsc-mainnet`, swap signer key, fund
    1 BNB. Smoke-test one $1 order. Watch for `OrderSettled`.

## 15. Open ops items

- Register a settlement sender account at https://paycrest.io and
  replace the placeholder `SETTLEMENT_SENDER_API_KEY_ID` UUID. Without
  it, orders still flow but show as "anonymous" in their analytics +
  can't use `providerId` pinning.
- Decide which BSC RPC provider for production. BSC official endpoints
  are rate-limited; consider Ankr, Alchemy, QuickNode for production.
- Choose Slack channel for low-BNB + stale-`dispatching` alerts. Reuse
  `services/notifications` patterns.
- Document the manual refund path: ops engineer pulls USDC from BSC
  aggregator wallet, bridges back to Sui via LiFi-reverse, sends to
  user's Sui address. v2 lift to automate.
- BSC testnet faucet: document where to top up the testnet wallet
  with testnet BNB + testnet USDC for dry runs.

## 16. What's NOT in this spec (parked)

- **Event indexer** for BSC Gateway (`OrderSettled` / `OrderRefunded`
  long-lived ws subscription). Polling covers v1.
- **Auto reverse-bridge on refund** — v2.
- **Biconomy / paymaster** integration. Server-side wallet pays gas.
  Single-digit cents per order; not worth the complexity.
- **Subgraph** for richer order metadata. Polling settlement +
  on-chain reads cover what we need.
- **EIP-7702 sponsored execution**. Server-side; no relevance.
- **Multi-chain settlement support** (Base, Arbitrum, Polygon, Celo).
  Same code, different config blocks. Easy to add when needed.
- **On-ramp** (USD → USDC). Different flow entirely.

## 17. Sign-off checklist

Before scaffolding starts, confirm:

- [ ] BSC testnet RPC URL chosen and added to env
- [ ] BSC testnet aggregator wallet created, private key in env
- [ ] BSC testnet Gateway + USDC addresses sourced (settlement docs or contract repo)
- [ ] BSC testnet faucet available for BNB + USDC
- [ ] Slack channel chosen for ops alerts
- [ ] Approval to add `go-ethereum` dep (~4MB binary increase)
- [ ] Approval to add Atlas migration for the 3 new `RouteAOrder` columns
