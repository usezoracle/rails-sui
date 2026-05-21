# Sui Move Gateway — Spec

## Purpose

The Move package that custodies a sender's Sui stablecoin while an LP fulfills the corresponding fiat leg, then releases the coin to the LP on settlement (or back to the sender on refund). Functional analog of the upstream EVM `IGateway` (`/Users/mac/Downloads/_reference.md`), restructured for Sui's object model.

Two design decisions, locked in the plan:
- **Each `Order` is its own shared object.** No global `Table<ID, Order>`. Settlement and refund take the order object directly, enabling parallel execution.
- **A `Gateway` shared object** holds protocol-wide config (supported coins, fees, treasury, aggregator caps).

---

## Package Layout

```
contracts/gateway/
├── Move.toml
├── sources/
│   ├── config.move      // Gateway shared object + AdminCap
│   ├── order.move       // Order shared object + create/settle/refund entry funcs
│   ├── events.move      // Event types
│   └── errors.move      // Abort codes
└── tests/
    ├── order_tests.move
    └── flow_tests.move
```

---

## Module: `rails::config`

The Gateway is a singleton shared object. Created once at deploy via `init`. Configuration is mutated only with the `AdminCap`.

```move
module rails::config {
    use sui::object::{Self, UID};
    use sui::tx_context::TxContext;
    use sui::transfer;
    use sui::vec_set::{Self, VecSet};
    use std::string::String;
    use std::type_name::{Self, TypeName};

    /// Singleton shared object holding protocol config.
    public struct Gateway has key {
        id: UID,
        /// TypeNames of coin types accepted as payment (e.g. USDC, USDT on Sui).
        supported_coins: VecSet<TypeName>,
        /// Protocol fee in basis points (1 = 0.01%). Charged on each order on settle.
        protocol_fee_bps: u64,
        /// Maximum basis points constant (10_000 = 100%).
        max_bps: u64,
        /// Address that receives protocol fees.
        treasury: address,
        /// Whether the protocol is paused (no new orders).
        paused: bool,
    }

    /// Capability granting admin authority over Gateway config.
    public struct AdminCap has key, store {
        id: UID,
    }

    /// Capability granting "aggregator" authority — can call settle/refund on behalf of LPs.
    public struct AggregatorCap has key, store {
        id: UID,
    }

    /// Called once at deploy. Creates and shares the Gateway, transfers AdminCap to deployer.
    fun init(ctx: &mut TxContext) { /* ... */ }

    // --- Admin functions (require AdminCap) ---
    public entry fun add_supported_coin<T>(_: &AdminCap, gw: &mut Gateway) { /* ... */ }
    public entry fun remove_supported_coin<T>(_: &AdminCap, gw: &mut Gateway) { /* ... */ }
    public entry fun set_protocol_fee(_: &AdminCap, gw: &mut Gateway, bps: u64) { /* ... */ }
    public entry fun set_treasury(_: &AdminCap, gw: &mut Gateway, t: address) { /* ... */ }
    public entry fun pause(_: &AdminCap, gw: &mut Gateway) { /* ... */ }
    public entry fun unpause(_: &AdminCap, gw: &mut Gateway) { /* ... */ }
    public entry fun mint_aggregator_cap(_: &AdminCap, recipient: address, ctx: &mut TxContext) { /* ... */ }

    // --- Read helpers ---
    public fun is_coin_supported<T>(gw: &Gateway): bool { /* ... */ }
    public fun protocol_fee_bps(gw: &Gateway): u64 { /* ... */ }
    public fun max_bps(gw: &Gateway): u64 { /* ... */ }
    public fun treasury(gw: &Gateway): address { /* ... */ }
}
```

---

## Module: `rails::order`

Each order is a shared object. The sender's coin sits inside the object as a `Balance<T>` until settled or refunded.

```move
module rails::order {
    use sui::object::{Self, UID, ID};
    use sui::tx_context::TxContext;
    use sui::transfer;
    use sui::coin::{Self, Coin};
    use sui::balance::{Self, Balance};
    use sui::clock::Clock;
    use std::string::String;
    use rails::config::{Self, Gateway, AggregatorCap};
    use rails::events;
    use rails::errors;

    /// One order = one shared object holding the sender's coin in escrow.
    public struct Order<phantom T> has key {
        id: UID,
        sender: address,
        amount: u64,
        /// Rate fiat-per-coin (scaled by 1e6) at the time the order was created.
        rate: u64,
        /// Off-chain reference (institution code, account hash) — kept opaque on-chain.
        institution_code: vector<u8>,
        message_hash: String,
        /// Fee paid to sender_fee_recipient (in coin units of T).
        sender_fee: u64,
        sender_fee_recipient: address,
        /// Protocol fee snapshot (in coin units of T).
        protocol_fee: u64,
        /// Where coin returns on refund.
        refund_address: address,
        /// Coin held in escrow.
        escrow: Balance<T>,
        /// Status: 0 = Pending, 1 = Settled, 2 = Refunded.
        status: u8,
        created_at_ms: u64,
    }

    /// Create an order by depositing coin into a new shared Order object.
    public entry fun create_order<T>(
        gw: &Gateway,
        payment: Coin<T>,
        rate: u64,
        institution_code: vector<u8>,
        message_hash: String,
        sender_fee: u64,
        sender_fee_recipient: address,
        refund_address: address,
        clock: &Clock,
        ctx: &mut TxContext,
    ) {
        // assert !gw.paused, !zero refund_address, T is supported, amount > 0, sender_fee < amount
        // compute protocol_fee = (amount * gw.protocol_fee_bps) / max_bps
        // construct Order, balance::from_coin(payment)
        // events::emit_order_created(...)
        // transfer::share_object(order)
    }

    /// Settle an order to an LP. Aggregator-only.
    /// settle_percent in basis points: portion of remaining escrow to release to this LP.
    public entry fun settle_order<T>(
        _: &AggregatorCap,
        gw: &Gateway,
        order: &mut Order<T>,
        liquidity_provider: address,
        settle_percent: u64,
        split_order_id: vector<u8>,
        ctx: &mut TxContext,
    ) {
        // assert order.status == Pending, settle_percent <= max_bps
        // calculate lp_amount = (order.amount * settle_percent) / max_bps
        // split balance, transfer Coin<T> to lp
        // if remaining escrow is zero: mark Settled, distribute protocol_fee + sender_fee
        // events::emit_order_settled(...)
    }

    /// Refund full remaining escrow to refund_address. Aggregator-only.
    public entry fun refund_order<T>(
        _: &AggregatorCap,
        order: &mut Order<T>,
        fee: u64,
        ctx: &mut TxContext,
    ) {
        // assert status == Pending
        // refund_amount = balance.value - fee
        // transfer Coin<T> to refund_address; fee to treasury
        // mark Refunded
        // events::emit_order_refunded(...)
    }

    // --- Read helpers ---
    public fun status<T>(o: &Order<T>): u8 { o.status }
    public fun amount<T>(o: &Order<T>): u64 { o.amount }
    public fun remaining<T>(o: &Order<T>): u64 { balance::value(&o.escrow) }
    public fun sender<T>(o: &Order<T>): address { o.sender }
    public fun rate<T>(o: &Order<T>): u64 { o.rate }
}
```

### Status Encoding
| Value | Meaning |
|---|---|
| 0 | `Pending` — escrow holds coin |
| 1 | `Settled` — escrow drained to LP(s) |
| 2 | `Refunded` — escrow returned to sender |

### Partial Settlement
`settle_order` may be called multiple times for the same order with different LPs, each taking a `settle_percent` of the *original* amount. Sum of all `settle_percent` calls must equal `max_bps`. Tracked via `remaining` balance — when zero, status flips to `Settled`.

---

## Module: `rails::events`

Mirrors the EVM reference events (`OrderCreated`, `OrderSettled`, `OrderRefunded`, `SenderFeeTransferred`) so the Go indexer can map them with minimal translation. Sui emits events as Move structs.

```move
module rails::events {
    use sui::event;
    use sui::object::ID;
    use std::string::String;

    public struct OrderCreated has copy, drop {
        order_id: ID,                // Sui object ID, equivalent to EVM bytes32 orderId
        sender: address,
        coin_type: vector<u8>,       // TypeName as bytes
        amount: u64,
        protocol_fee: u64,
        rate: u64,
        institution_code: vector<u8>,
        message_hash: String,
    }

    public struct OrderSettled has copy, drop {
        split_order_id: vector<u8>,
        order_id: ID,
        liquidity_provider: address,
        settle_percent: u64,
        amount_released: u64,
    }

    public struct OrderRefunded has copy, drop {
        fee: u64,
        order_id: ID,
        amount_refunded: u64,
    }

    public struct SenderFeeTransferred has copy, drop {
        sender_fee_recipient: address,
        amount: u64,
    }

    public(package) fun emit_order_created(...) { event::emit(...) }
    public(package) fun emit_order_settled(...) { event::emit(...) }
    public(package) fun emit_order_refunded(...) { event::emit(...) }
    public(package) fun emit_sender_fee_transferred(...) { event::emit(...) }
}
```

---

## Module: `rails::errors`

```move
module rails::errors {
    const EPaused: u64 = 1;
    const EUnsupportedCoin: u64 = 2;
    const EZeroAmount: u64 = 3;
    const EZeroRefundAddress: u64 = 4;
    const ESenderFeeTooHigh: u64 = 5;
    const ENotPending: u64 = 6;
    const ESettlePercentTooHigh: u64 = 7;
    const ESettleExceedsRemaining: u64 = 8;
    const ERefundFeeTooHigh: u64 = 9;
}
```

---

## Mapping: EVM Reference → Sui Move

| EVM IGateway | Sui Move equivalent | Notes |
|---|---|---|
| `function createOrder(_token, _amount, _institutionCode, _rate, _senderFeeRecipient, _senderFee, _refundAddress, messageHash) returns (bytes32 _orderId)` | `entry fun create_order<T>(gw, payment: Coin<T>, rate, institution_code, message_hash, sender_fee, sender_fee_recipient, refund_address, clock, ctx)` | Token is the `Coin<T>` phantom type. orderId is the new shared object's `ID` (available via `OrderCreated.order_id`). |
| `function settle(bytes32 _splitOrderId, bytes32 _orderId, address _liquidityProvider, uint64 _settlePercent)` | `entry fun settle_order<T>(cap, gw, order: &mut Order<T>, liquidity_provider, settle_percent, split_order_id, ctx)` | Order object passed directly. Aggregator-gated by `AggregatorCap`. |
| `function refund(uint256 _fee, bytes32 _orderId)` | `entry fun refund_order<T>(cap, order: &mut Order<T>, fee, ctx)` | Same. |
| `function isTokenSupported(address _token)` | `public fun is_coin_supported<T>(gw): bool` | Type-checked via `TypeName`. |
| `function getOrderInfo(bytes32 _orderId)` | Read object directly via Sui RPC `getObject({orderId})` | No on-chain reader needed; Sui exposes object state natively. |
| `function getFeeDetails()` | `public fun protocol_fee_bps(gw): u64` + `max_bps(gw): u64` | Two getters instead of a tuple. |
| `event OrderCreated(...)` | `struct OrderCreated has copy, drop {...}` + `event::emit` | Field shapes match. |
| `event OrderSettled(...)` | `struct OrderSettled has copy, drop {...}` | Same. |
| `event OrderRefunded(...)` | `struct OrderRefunded has copy, drop {...}` | Same. |
| `event SenderFeeTransferred(...)` | `struct SenderFeeTransferred has copy, drop {...}` | Same. |
| `getSupportedInstitutionByCode` / `getSupportedInstitutions` | Not on-chain | Institutions live in the Go backend's `FiatCurrency` / `Institution` tables. Move package only carries opaque `institution_code` bytes. |

---

## Coin Type Strategy

`supported_coins: VecSet<TypeName>` holds the type names of accepted coins. Initial supported set:

- `0x...::usdc::USDC` (Sui-native USDC, Circle-issued)
- `0x...::usdt::USDT` (Sui USDT, when available)

Adding a coin: admin calls `add_supported_coin<NEW_COIN>(&AdminCap, &mut Gateway)`. Type is registered; future `create_order<NEW_COIN>` calls are accepted.

`create_order<T>` checks `assert!(config::is_coin_supported<T>(gw), errors::EUnsupportedCoin)`.

---

## Aggregator Pattern

Settlement and refund are aggregator-gated, not LP-gated, to match the upstream model: the Rails backend (the "aggregator") submits settlement transactions on behalf of LPs after validating off-chain proof-of-fiat-transfer.

- `AggregatorCap` is minted by admin and held by the Rails backend's hot wallet.
- LPs do **not** call `settle_order` directly. They report fulfillment to Rails via their existing webhook, Rails validates (`LockOrderFulfillment.validation_status = "success"`), then Rails submits `settle_order` with the LP's wallet address as recipient.

This keeps the Move package narrow (no LP-side identity logic), defers identity to off-chain KYB, and makes settlement atomic with backend validation.

---

## Testing

- **Unit tests** (`tests/order_tests.move`):
  - `create_order` happy path: supported coin, valid params, asserts emitted event + escrow balance.
  - `create_order` rejects: unsupported coin, zero amount, paused gateway, sender_fee >= amount.
  - `settle_order` full settlement: percent = max_bps, escrow drained, LP receives full minus fees.
  - `settle_order` partial: two LPs each take 50%, status flips to Settled after second call.
  - `refund_order`: status changes, fee paid to treasury, remainder to refund_address.
  - `settle_order` after refund: aborts with ENotPending.

- **Integration tests** (`tests/flow_tests.move`):
  - End-to-end: deploy, add coin, create order, settle 100%, verify all events.

- **Devnet integration** (script, not Move tests):
  - Publish package to Sui devnet.
  - Mint mock test-USDC.
  - Run full create → settle flow via `sui client call`.
  - Verify backend indexer picks up events.

---

## Deployment & Upgrade

- v1 deploys with `init` creating `Gateway` (shared) + `AdminCap` (transferred to deployer multisig).
- Aggregator wallet receives an `AggregatorCap` minted by admin post-deploy.
- Upgrade path: Sui Move package upgrades via `UpgradeCap`. Held by same admin multisig. Backwards-compatibility constraints enforced by Sui's verifier.
- Treasury and admin both behind a multisig (Sui native multisig or external like Squads-equivalent when available).

---

## Open Implementation Notes

- **Rate decimal scale:** EVM uses `uint96` for rate. Sui uses `u64` here, scaled by `1e6` (6 decimals of precision on the fiat-per-coin rate). Adequate for FX rates in the realistic range.
- **Sender fee recipient transfer:** EVM emits `SenderFeeTransferred` separately. In Move, we emit it as part of the settle flow once protocol_fee + sender_fee are split out.
- **Clock:** Sui's `Clock` shared object passed to `create_order` for the `created_at_ms` timestamp. Necessary for off-chain expiration logic if added later.
- **No on-chain order expiration in v1.** Off-chain Rails backend can mark orders expired in DB; on-chain coin remains in escrow until aggregator calls `refund_order`.

## Phase 1 Spikes

Before committing the Move package design these must be measured / tested on Sui testnet:

1. **Gas-cost measurement for Route A handoff** — measure the cost of (a) `create_order` + immediate self-`settle_order` (Option A) vs (b) a dedicated `create_route_a_order` entry that puts coin directly in the bridge hot wallet (Option B). Picks the design documented in `route-a-spec.md` (currently deferred).
2. **`transfer_to_object` exchange compatibility** — test Sui USDC withdrawal from Bybit, Binance, OKX, KuCoin, Bitget on testnet to a Move object's ID (the `Order` shared object). If all five accept it, the Order shared object can serve as the receive target directly — making the Path 2 deposit flow custody-free even for exchange users. If any reject, the keypair-based receive address pattern in `rails-architecture.md` remains the fallback for that exchange. Either outcome ships.
3. **LiFi `transactionRequest` shape on Sui** — is the base64 blob LiFi returns a fully-built `TransactionData` we only sign, or a programmable transaction block we must further compose? Confirm with a devnet bridge call. Affects the Sui RPC wrapper thickness.
4. **zkLogin gas sponsorship signing flow** — confirm the exact PTB construction + sponsor signature pattern for zkLogin transactions, end-to-end on testnet.
