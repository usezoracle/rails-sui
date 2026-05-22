/// Tapp Card spending cap.
///
/// Non-custodial: the user funds a `CardSpendingCap<T>` they own,
/// scoped to a single physical card (committed via `card_uid_hash`).
/// Rails — with the global `AggregatorCap` — can call `debit(cap, …)`
/// against it, but only within Move-enforced daily / per-tap limits.
/// The user destroys the cap any time to reclaim the balance, without
/// any cooperation from Rails.
///
/// See `rails/docs/tapp-card-spec.md` for the surrounding spec
/// (HMAC PIN protocol, secret-token rotation, three-tier auth, etc.).
module rails::tapp_card;

use std::ascii;
use std::type_name;
use sui::balance::{Self, Balance};
use sui::clock::{Self, Clock};
use sui::coin::{Self, Coin};

use rails::config::AggregatorCap;
use rails::events;

// ----- Constants -----

const MS_PER_DAY: u64 = 86_400_000;

// ----- Error codes -----

const EWrongOwner: u64 = 1;
const ERevoked: u64 = 2;
const EOverPerTapLimit: u64 = 3;
const EOverDailyLimit: u64 = 4;
const EZeroAmount: u64 = 5;
const EInsufficientBalance: u64 = 6;
const EInvalidUidHash: u64 = 7;
const ELimitConfigInvalid: u64 = 8;

// ----- Struct -----

/// Per-user, per-card spending cap.
///
/// `balance` is the actual pre-funded `Balance<T>` debits draw from.
/// Limits are enforced inside `debit`; daily rollover is computed
/// from the on-chain `Clock` (UTC ms / 86 400 000).
public struct CardSpendingCap<phantom T> has key {
    id: UID,
    owner: address,
    balance: Balance<T>,
    daily_limit_subunit: u64,
    spent_today_subunit: u64,
    day_index: u64,
    per_tap_limit_subunit: u64,
    /// sha256 of the NTAG215 factory UID. Committed at create_cap time
    /// so swapping the card-side secret doesn't let an attacker bind
    /// a different physical card to the same cap.
    card_uid_hash: vector<u8>,
    revoked: bool,
}

// ----- Cardholder-facing entries (signed via zkLogin in the PWA) -----

/// Create a new cap for one card. The caller is the cap's `owner`;
/// only they (via destroy_and_reclaim) can recover the funded balance.
public entry fun create_cap<T>(
    funding: Coin<T>,
    daily_limit_subunit: u64,
    per_tap_limit_subunit: u64,
    card_uid_hash: vector<u8>,
    ctx: &mut TxContext,
) {
    assert!(per_tap_limit_subunit > 0, ELimitConfigInvalid);
    assert!(daily_limit_subunit >= per_tap_limit_subunit, ELimitConfigInvalid);
    assert!(card_uid_hash.length() == 32, EInvalidUidHash);

    let cap = CardSpendingCap<T> {
        id: object::new(ctx),
        owner: ctx.sender(),
        balance: funding.into_balance(),
        daily_limit_subunit,
        spent_today_subunit: 0,
        day_index: 0,
        per_tap_limit_subunit,
        card_uid_hash,
        revoked: false,
    };
    transfer::share_object(cap);
}

/// Top up the funded balance.
public entry fun top_up<T>(cap: &mut CardSpendingCap<T>, more: Coin<T>, ctx: &TxContext) {
    assert!(cap.owner == ctx.sender(), EWrongOwner);
    cap.balance.join(more.into_balance());
}

/// Update the limits. Both must remain > 0 and daily ≥ per-tap.
public entry fun update_limits<T>(
    cap: &mut CardSpendingCap<T>,
    new_daily: u64,
    new_per_tap: u64,
    ctx: &TxContext,
) {
    assert!(cap.owner == ctx.sender(), EWrongOwner);
    assert!(new_per_tap > 0, ELimitConfigInvalid);
    assert!(new_daily >= new_per_tap, ELimitConfigInvalid);
    cap.daily_limit_subunit = new_daily;
    cap.per_tap_limit_subunit = new_per_tap;
}

/// Kill switch / unpause. `debit` aborts when revoked.
public entry fun set_revoked<T>(cap: &mut CardSpendingCap<T>, revoked: bool, ctx: &TxContext) {
    assert!(cap.owner == ctx.sender(), EWrongOwner);
    cap.revoked = revoked;
}

/// Destroy the cap and pay the remaining balance back to the owner.
/// Only the owner can call this — non-custodial guarantee.
public entry fun destroy_and_reclaim<T>(cap: CardSpendingCap<T>, ctx: &mut TxContext) {
    assert!(cap.owner == ctx.sender(), EWrongOwner);
    let CardSpendingCap<T> { id, owner, balance, .. } = cap;
    let refund = coin::from_balance(balance, ctx);
    transfer::public_transfer(refund, owner);
    id.delete();
}

// ----- Aggregator-only entry -----

/// Debit the cap. Reachable only with `&AggregatorCap`, which Rails
/// owns. Move enforces the limits authoritatively — even if Rails'
/// off-chain pre-check is bypassed, the chain still rejects an
/// over-cap debit.
public entry fun debit<T>(
    _: &AggregatorCap,
    cap: &mut CardSpendingCap<T>,
    amount_subunit: u64,
    merchant_recipient: address,
    fiat_reference: vector<u8>,
    clock: &Clock,
    ctx: &mut TxContext,
) {
    assert!(!cap.revoked, ERevoked);
    assert!(amount_subunit > 0, EZeroAmount);
    assert!(amount_subunit <= cap.per_tap_limit_subunit, EOverPerTapLimit);
    assert!(cap.balance.value() >= amount_subunit, EInsufficientBalance);

    // Day rollover. We don't need a separate "today" entry function —
    // any debit that crosses a day boundary resets the counter
    // atomically in the same Move call.
    let now_ms = clock::timestamp_ms(clock);
    let day_now = now_ms / MS_PER_DAY;
    if (day_now != cap.day_index) {
        cap.day_index = day_now;
        cap.spent_today_subunit = 0;
    };

    assert!(
        cap.spent_today_subunit + amount_subunit <= cap.daily_limit_subunit,
        EOverDailyLimit,
    );

    cap.spent_today_subunit = cap.spent_today_subunit + amount_subunit;

    // Pay the merchant. The recipient is the Rails treasury / per-
    // merchant address selected by the off-chain matching engine —
    // the chain doesn't need to know which merchant; it just sends to
    // the address the aggregator chose.
    let payout = coin::from_balance(cap.balance.split(amount_subunit), ctx);
    transfer::public_transfer(payout, merchant_recipient);

    let coin_type_str = type_name::with_defining_ids<T>().into_string();
    let coin_type_bytes = ascii::into_bytes(coin_type_str);

    events::emit_card_debited(
        object::id(cap),
        cap.owner,
        merchant_recipient,
        amount_subunit,
        fiat_reference,
        now_ms,
        coin_type_bytes,
    );
}

// ----- Read-only accessors (used by tests + on-chain views) -----

public fun owner<T>(cap: &CardSpendingCap<T>): address { cap.owner }
public fun balance_value<T>(cap: &CardSpendingCap<T>): u64 { cap.balance.value() }
public fun daily_limit<T>(cap: &CardSpendingCap<T>): u64 { cap.daily_limit_subunit }
public fun per_tap_limit<T>(cap: &CardSpendingCap<T>): u64 { cap.per_tap_limit_subunit }
public fun spent_today<T>(cap: &CardSpendingCap<T>): u64 { cap.spent_today_subunit }
public fun day_index<T>(cap: &CardSpendingCap<T>): u64 { cap.day_index }
public fun is_revoked<T>(cap: &CardSpendingCap<T>): bool { cap.revoked }
public fun card_uid_hash<T>(cap: &CardSpendingCap<T>): &vector<u8> { &cap.card_uid_hash }
