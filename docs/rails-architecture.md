# Rails — Architecture

## What Rails Is

A B2B settlement infrastructure that converts **Sui stablecoins → local fiat** on behalf of third-party dApps, exchanges, and consumer products (including our own Tapp). The integrator never touches bridging, KYC, LP matching, or banking — they call one API and fiat lands in the merchant's account.

Two routes are exposed; the integrator picks one per transaction:

- **Route A — Bridging:** Sui stablecoin → LiFi bridge → BSC USDC → existing EVM fiat rail OR our centralized OTC treasury → merchant bank.
- **Route B — Decentralized LP:** Sui stablecoin → on-chain Gateway order → LP wins the order → LP pulls fiat from their virtual account → fiat lands in merchant bank → Sui stablecoin released to LP wallet.

Both routes share: order lifecycle, KYC, merchant onboarding, webhooks, settlement reporting.

---

## System Components

```
┌──────────────────────────────────────────────────────────────┐
│                     Integrator (dApp / Tapp / B2B caller)    │
└─────────────────────────────┬────────────────────────────────┘
                              │ REST + signed webhooks
                              ▼
┌──────────────────────────────────────────────────────────────┐
│                     Rails Go Backend                         │
│  ┌────────────────┐  ┌─────────────────┐  ┌───────────────┐  │
│  │   API Layer    │  │  Matching       │  │   Indexer     │  │
│  │  (controllers) │  │  (priority      │  │  (Sui events) │  │
│  │                │  │   queue + rate  │  │               │  │
│  └────────────────┘  │   ceiling)      │  └───────┬───────┘  │
│                      └─────────┬───────┘          │          │
│  ┌────────────────┐  ┌─────────▼───────┐  ┌───────▼───────┐  │
│  │ Ceiling Rate   │  │  Order State    │  │ Sui RPC       │  │
│  │ engine         │  │  Machine        │  │ client        │  │
│  │ (Redis median) │  │  (Ent / PG)     │  │               │  │
│  └────────────────┘  └─────────────────┘  └───────────────┘  │
│  ┌────────────────┐  ┌─────────────────┐  ┌───────────────┐  │
│  │  KYC (Smile)   │  │  LiFi client    │  │ BaaS client   │  │
│  │  Auth (API key)  │  │  (Route A)      │  │ (virtual      │  │
│  │                │  │                 │  │  accounts)    │  │
│  └────────────────┘  └─────────────────┘  └───────────────┘  │
└──────┬─────────────────────┬───────────────────┬─────────────┘
       │                     │                   │
       ▼                     ▼                   ▼
┌────────────┐       ┌──────────────┐    ┌──────────────────┐
│ Sui chain  │       │ LiFi /       │    │ BaaS provider    │
│ (Gateway   │       │ EVM Gateway  │    │ (LP fiat virtual │
│  Move pkg) │       │ on BSC       │    │  accounts)       │
└────────────┘       └──────────────┘    └──────────────────┘
       ▲
       │ on-chain events (OrderCreated, OrderSettled, OrderRefunded)
       │
┌──────┴─────┐
│ LP wallet  │
│ (Sui)      │
└────────────┘
```

---

## Route B — Decentralized LP (sequence)

LPs are **passive capital providers**. They do not run nodes, webhooks, or any infrastructure. They deposit fiat into a virtual account we issue them, configure a rate band in their dashboard, and grant Rails a pull mandate via the BaaS. Everything per-order is automatic; the LP takes no action between deposit and payout.

```
Integrator        Rails API        Sui Gateway       Matching        BaaS         Merchant bank
    │                │                   │              │              │                │
    │ POST /orders   │                   │              │              │                │
    │ (Sui token,    │                   │              │              │                │
    │  amount, fiat, │                   │              │              │                │
    │  bank info)    │                   │              │              │                │
    │───────────────▶│                   │              │              │                │
    │                │ derive pay_to     │              │              │                │
    │                │ shared object     │              │              │                │
    │ 200 {orderId,  │                   │              │              │                │
    │  pay_to}       │                   │              │              │                │
    │◀───────────────│                   │              │              │                │
    │                │                   │              │              │                │
    │ user pays Sui  │                   │              │              │                │
    │ via create_    │                   │              │              │                │
    │ order(...)     │                   │              │              │                │
    │───────────────────────────────────▶│              │              │                │
    │                │                   │ emit         │              │                │
    │                │                   │ OrderCreated │              │                │
    │                │◀──────────────────│              │              │                │
    │                │ indexer creates   │              │              │                │
    │                │ LockPaymentOrder  │              │              │                │
    │                │──────────────────────────────────▶              │                │
    │                │                   │              │ select LP    │                │
    │                │                   │              │ (round-robin │                │
    │                │                   │              │ within rate  │                │
    │                │                   │              │ ceiling, KYB │                │
    │                │                   │              │ valid, bal ≥ │                │
    │                │                   │              │ min)         │                │
    │                │                   │              │              │                │
    │                │                   │              │ pull fiat    │                │
    │                │                   │              │ from LP's    │                │
    │                │                   │              │ virtual acct │                │
    │                │                   │              │─────────────▶│                │
    │                │                   │              │              │ transfer to    │
    │                │                   │              │              │ merchant       │
    │                │                   │              │              │───────────────▶│
    │                │                   │              │              │ tx_id          │
    │                │                   │              │◀─────────────│                │
    │                │                   │              │              │                │
    │                │                   │              │ aggregator   │                │
    │                │                   │              │ submits      │                │
    │                │                   │              │ settle_order │                │
    │                │                   │              │ (LP wallet,  │                │
    │                │                   │              │ 100%)        │                │
    │                │                   │◀─────────────│              │                │
    │                │                   │ emit         │              │                │
    │                │                   │ OrderSettled │              │                │
    │                │                   │ + transfer   │              │                │
    │                │                   │ coin to LP   │              │                │
    │                │◀──────────────────│              │              │                │
    │ webhook        │                   │              │              │                │
    │ order.settled  │                   │              │              │                │
    │◀───────────────│                   │              │              │                │
```

Note: `settle_order` is called by the Rails aggregator (holding `AggregatorCap`), not by the LP. The LP has no chain interaction at all in the happy path.

States flow: `pending → processing → fulfilled (fiat sent via BaaS) → validated (BaaS confirmed) → settled (on-chain coin released to LP wallet)`.

---

## Deposit paths on Route B: PTB (default, custody-free) + receive address (exchange fallback)

Two deposit paths supported on Route B, surfaced side-by-side in the integrator's UI. The end-user picks based on where their crypto lives.

### Path 1 — PTB (default, custody-free)

The user has a connected Sui wallet (Sui Wallet, Suiet, Phantom, hardware bridge, or zkLogin via Tapp). They sign a Programmable Transaction Block (PTB) that atomically: (a) splits a `Coin<USDC>` from their wallet and (b) calls `rails::order::create_order(...)` on the Move Gateway with that coin. One signature, one transaction. Coin flows from user wallet → directly into the new `Order` shared object's escrow. No Rails-controlled wallet exists in the path. Fully custody-free.

This is the Sui-native pattern, enabled by PTB atomicity — something EVM lacks (ERC20 transfer and contract call cannot be bundled in one user signature on EVM, which is why the upstream EVM design uses per-order AA smart accounts and accepts a brief custody window).

### Path 2 — Receive address (exchange / external wallet fallback)

The user wants to pay from an exchange (Bybit, Binance, OKX, KuCoin, Bitget, etc.) or any wallet whose UI only supports plain coin transfers. Exchange withdrawal flows cannot sign custom PTBs — they only let users pick "destination address + amount." For this case, Rails generates a per-order receive address:

```
Integrator UI            Rails backend           Exchange (e.g. Bybit)     Sui chain
     │                       │                          │                      │
     │ POST /v1/orders       │                          │                      │
     │──────────────────────▶│                          │                      │
     │                       │ generate fresh Sui       │                      │
     │                       │ keypair, encrypt and     │                      │
     │                       │ store priv key in        │                      │
     │                       │ SuiReceiveAddress.       │                      │
     │                       │ derive address.          │                      │
     │ 200 {pay_to.options:  │                          │                      │
     │  [..., receive_address│                          │                      │
     │  : 0xdead...]}        │                          │                      │
     │◀──────────────────────│                          │                      │
     │                       │                          │                      │
     │ show address + QR     │                          │                      │
     │ to user               │                          │                      │
     │                       │                          │                      │
     │ user opens Bybit,     │                          │                      │
     │ pastes address,       │                          │                      │
     │ clicks Withdraw       │                          │                      │
     │                                                  │ Bybit submits Sui    │
     │                                                  │ transfer tx          │
     │                                                  │─────────────────────▶│
     │                                                  │       USDC arrives   │
     │                                                  │       at receive addr│
     │                       │ WS event subscription    │                      │
     │                       │ detects ObjectChange     │                      │
     │                       │ (sub-second)             │                      │
     │                       │◀─────────────────────────────────────────────────│
     │                       │                          │                      │
     │                       │ construct PTB FROM       │                      │
     │                       │ receive addr calling     │                      │
     │                       │ create_order(...),       │                      │
     │                       │ sign with stored priv    │                      │
     │                       │ key, submit              │                      │
     │                       │─────────────────────────────────────────────────▶│
     │                       │                          │       coin moves     │
     │                       │                          │       from receive   │
     │                       │                          │       addr → Order   │
     │                       │                          │       shared object  │
     │                       │                          │       escrow         │
     │                       │ from here: normal Route B│                      │
     │                       │ (LP match, fiat send,    │                      │
     │                       │ on-chain settle)         │                      │
```

**Custody window:** for the few seconds between deposit detection and `create_order` submission, the coin sits in a Rails-managed wallet. Same in-flight window any EVM-based settlement product accepts with per-order AA smart accounts. Unavoidable for exchange-source payments — no settlement product has eliminated it.

**Blast-radius minimization:**
- Per-order keypair (compromise scope = one order's value, never a shared hot wallet)
- WebSocket-based event detection (sub-second, not polling)
- Pre-built `create_order` PTB ready before deposit lands (submission is instant)
- Hard `valid_until` expiration on each receive address (default 30 min); expired addresses sweep any unexpected late deposits to a designated recovery sweeper

### Schema addition

```
SuiReceiveAddress
  id                string (PK)
  address           string  unique  (the Sui address derived from the keypair)
  encrypted_priv_key bytea          (KMS-wrapped Ed25519 private key)
  order_id          string  → PaymentOrder.id
  status            enum    "unused" | "deposited" | "forwarded" | "expired"
  valid_until       timestamp
  deposit_tx_hash   string  nullable, set when deposit detected
  forward_tx_hash   string  nullable, set when create_order PTB lands
  created_at, updated_at
```

Mirrors the legacy `ReceiveAddress` schema (`ent/schema/receiveaddress.go`), Sui-native fields. Service: `services/sui_receive_address.go` (generate, monitor, forward).

### Phase 1 spike — `transfer_to_object` as a possibly-custody-free Path 2

Sui's `transfer_to_object` primitive sends coin to a Move object's ID rather than a wallet address. In theory we could pre-create the `Order` shared object in `awaiting_deposit` state, give the user the **Order object's ID** as the destination, and let the user send from exchange to that ID. The Sui contract receives the coin via Sui's `Receiving<T>` pattern. No per-order keypair, no Rails-controlled wallet, no custody — even for exchange users.

Sui object IDs and wallet addresses share the same 32-byte format at the protocol level, so the exchange's withdrawal form likely accepts an object ID as input. But exchanges sometimes apply extra validation (e.g., requiring the destination to have an existing balance, or to look like a "wallet" address) that could reject this.

**Phase 1 must spike this:** test Sui USDC testnet withdrawal from Bybit, Binance, OKX, KuCoin, Bitget to a Move object ID. If all five accept it, ship the custody-free design as Path 2 for those exchanges. If even one rejects, fall back to the keypair receive-address pattern above for that exchange — the spec already documents both, so either outcome ships.

### Rails-operated wallets summary

| Wallet | Purpose | Holds whose funds? |
|---|---|---|
| **Aggregator wallet** | Holds `AggregatorCap`; signs `settle_order` / `refund_order` calls on Sui Gateway. | Nothing — it's a permission capability. |
| **Gas sponsorship wallet** | Pays SUI gas for Tapp / zkLogin user PTBs and for the Path 2 `create_order` forwarding tx. | Our SUI float, not user funds. |
| **Per-order Sui receive wallet** | One per order using Path 2 (keypair fallback). Holds the user's USDC briefly between exchange deposit and `create_order` forwarding. | User funds, briefly (seconds). Path 2 keypair-fallback only. |
| **Route A Sui hot wallet** | Holds bridged-pending USDC between Sui deposit and LiFi bridge fire. | User funds, briefly. Route A only — see [route-a-spec.md](route-a-spec.md). |
| **Route A BSC hot wallet** | Receives bridged USDC, pays merchant via treasury or BSC Gateway. | User funds, briefly. Route A only. |

Path 1 is fully custody-free. Path 2 (keypair) has brief in-flight custody (seconds). Path 2 (`transfer_to_object`, if Phase 1 spike succeeds) would be custody-free. Route A always has brief custody by nature of bridging. All paths are documented honestly per scenario.

---

## Route A — Bridging (sequence)

The integrator picks `mode: "lp" | "treasury"` at order creation. Both paths bridge Sui → BSC via LiFi first; they differ after that.

```
Integrator        Rails API           Sui Gateway       LiFi          BSC EVM Gateway / Treasury    Merchant bank
    │                │                     │              │                  │                          │
    │ POST /orders   │                     │              │                  │                          │
    │ route_a, mode  │                     │              │                  │                          │
    │───────────────▶│                     │              │                  │                          │
    │                │ derive pay_to       │              │                  │                          │
    │ 200 {orderId,  │                     │              │                  │                          │
    │  pay_to}       │                     │              │                  │                          │
    │◀───────────────│                     │              │                  │                          │
    │                │                     │              │                  │                          │
    │ user pays Sui  │                     │              │                  │                          │
    │───────────────────────────────────▶  │              │                  │                          │
    │                │ indexer sees deposit│              │                  │                          │
    │                │                     │              │                  │                          │
    │                │ get LiFi quote &    │              │                  │                          │
    │                │ execute bridge      │              │                  │                          │
    │                │────────────────────────────────────▶                  │                          │
    │                │                     │              │ BSC USDC arrives │                          │
    │                │                     │              │ at our hot wallet│                          │
    │                │                     │              │                  │                          │
    │                │ if mode = "lp":     │              │                  │                          │
    │                │   call BSC Gateway  │              │                  │                          │
    │                │   createOrder       │              │                  │                          │
    │                │─────────────────────────────────────────────────────▶│                          │
    │                │ (existing EVM rails  │              │                  │ same Route B flow on BSC│
    │                │  fulfil it normally)│              │                  │─────────────────────────▶│
    │                │                     │              │                  │                          │
    │                │ if mode = "treasury":│              │                  │                          │
    │                │   sweep to treasury │              │                  │                          │
    │                │   trigger banking   │              │                  │                          │
    │                │   partner payout    │              │                  │                          │
    │                │─────────────────────────────────────────────────────────────────────────────────▶│
    │                │                     │              │                  │                          │
    │ webhook        │                     │              │                  │                          │
    │ order.settled  │                     │              │                  │                          │
    │◀───────────────│                     │              │                  │                          │
```

---

## Data Flow Summary

| Boundary | Direction | What crosses |
|---|---|---|
| Integrator ↔ Rails | both | REST requests, signed webhook callbacks |
| Rails ↔ Sui chain | both | Tx submissions (sponsored or LP-signed), event subscriptions |
| Rails ↔ BSC chain | both (Route A only) | LiFi-relayed bridge tx, EVM Gateway calls |
| Rails ↔ LP backend | both | New-order webhooks, settlement signatures |
| Rails ↔ BaaS | both | Virtual account provisioning, fiat pull/sweep, status webhooks |
| Rails ↔ KYC (Smile) | both | KYB verification requests + callbacks |
| Rails ↔ Spot oracles | inbound | Polled rates from Binance P2P, Quidax, others |

---

## Trust & Custody Model

| Asset / role | Custody |
|---|---|
| Sui stablecoin in-flight (Route B) | Locked in `Order` shared object on the Move Gateway. Released to LP only on `settle`. |
| Sui stablecoin in-flight (Route A) | Briefly held by our hot wallet between Sui receipt and LiFi bridge execution. Minimize residence time. |
| BSC USDC post-bridge (Route A, treasury mode) | Sweeps to Rails treasury multisig; banking partner pays merchant on our behalf. |
| BSC USDC post-bridge (Route A, lp mode) | Enters existing EVM Gateway; locked there until BSC LP settles. |
| LP fiat balance | LP-owned virtual account at BaaS. We have pull authority (configured during onboarding); we do not custody. |
| Merchant bank account | External, not custodied. We only have payout authority via BaaS / banking partner. |

---

## Why This Shape

- **Route B as the core economic engine, Route A as the resilience layer.** When LP supply on a corridor is thin or rates blow past the ceiling, Route A absorbs flow. Integrators get one API regardless.
- **Sui-native Move design (per-order shared objects).** Sui's parallel execution shines when orders don't contend on a single global table. Each order being its own object means matching, settlement, and refund operations parallelize across the LP set.
- **Ceiling Rate off-chain.** Spot-rate oracles for emerging-market FX pairs (NGN, KES, IDR) are sparse on-chain. Off-chain median from multiple liquid venues is more accurate and faster to update. Enforcement at the matching layer means we can ship rate logic changes without redeploying the Move contract.
- **Fork-and-strip the upstream.** ~80% of the chain-agnostic plumbing (KYC, lifecycle, webhooks, retry, matching) is production-grade in the upstream codebase. Rebuilding it is wasted effort.

---

## Out of Scope for v1

- Sui-native LPs settling Sui orders without bridging at all (could be added once LP density on Sui justifies a separate matching pool).
- Multi-leg routing (e.g., Sui → BSC → ETH → fiat). Single-hop LiFi only.
- Self-serve LP onboarding. v1 ships with operator-gated onboarding.
- Refund initiation by integrator. v1 supports operator-initiated refunds only.
