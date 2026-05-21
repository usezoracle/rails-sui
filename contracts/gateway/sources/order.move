module rails::order;

use std::ascii;
use std::string::String;
use std::type_name;
use sui::balance::{Self, Balance};
use sui::clock::{Self, Clock};
use sui::coin::{Self, Coin};

use rails::config::{Self, Gateway, AggregatorCap};
use rails::events;

const STATUS_PENDING: u8 = 0;
const STATUS_SETTLED: u8 = 1;
const STATUS_REFUNDED: u8 = 2;

const EPaused: u64 = 1;
const EUnsupportedCoin: u64 = 2;
const EZeroAmount: u64 = 3;
const EZeroRefundAddress: u64 = 4;
const EFeesExceedAmount: u64 = 5;
const ENotPending: u64 = 6;
const ESettlePercentTooHigh: u64 = 7;
const ESettleExceedsRemaining: u64 = 8;
const ERefundFeeTooHigh: u64 = 9;

public struct Order<phantom T> has key {
    id: UID,
    sender: address,
    amount: u64,
    rate: u64,
    institution_code: vector<u8>,
    message_hash: String,
    sender_fee: u64,
    sender_fee_recipient: address,
    protocol_fee: u64,
    refund_address: address,
    escrow: Balance<T>,
    settled_lp_amount: u64,
    status: u8,
    created_at_ms: u64,
}

public fun create_order<T>(
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
    assert!(!config::is_paused(gw), EPaused);
    assert!(config::is_coin_supported<T>(gw), EUnsupportedCoin);
    assert!(refund_address != @0x0, EZeroRefundAddress);

    let amount = payment.value();
    assert!(amount > 0, EZeroAmount);

    let protocol_fee = (amount * config::protocol_fee_bps(gw)) / config::max_bps(gw);
    assert!(sender_fee + protocol_fee < amount, EFeesExceedAmount);

    let coin_type_str = type_name::with_defining_ids<T>().into_string();
    let coin_type_bytes = ascii::into_bytes(coin_type_str);

    let order = Order<T> {
        id: object::new(ctx),
        sender: ctx.sender(),
        amount,
        rate,
        institution_code,
        message_hash,
        sender_fee,
        sender_fee_recipient,
        protocol_fee,
        refund_address,
        escrow: payment.into_balance(),
        settled_lp_amount: 0,
        status: STATUS_PENDING,
        created_at_ms: clock.timestamp_ms(),
    };

    let order_id = object::id(&order);

    events::emit_order_created(
        order_id,
        order.sender,
        coin_type_bytes,
        amount,
        protocol_fee,
        rate,
        order.institution_code,
        order.message_hash,
    );

    transfer::share_object(order);
}

public fun settle_order<T>(
    _: &AggregatorCap,
    gw: &Gateway,
    order: &mut Order<T>,
    liquidity_provider: address,
    settle_percent: u64,
    split_order_id: vector<u8>,
    ctx: &mut TxContext,
) {
    assert!(order.status == STATUS_PENDING, ENotPending);
    assert!(settle_percent <= config::max_bps(gw), ESettlePercentTooHigh);

    let lp_distributable = order.amount - order.protocol_fee - order.sender_fee;
    let lp_amount = (lp_distributable * settle_percent) / config::max_bps(gw);

    assert!(order.settled_lp_amount + lp_amount <= lp_distributable, ESettleExceedsRemaining);

    let lp_balance = order.escrow.split(lp_amount);
    transfer::public_transfer(coin::from_balance(lp_balance, ctx), liquidity_provider);

    order.settled_lp_amount = order.settled_lp_amount + lp_amount;

    let order_id = object::id(order);

    events::emit_order_settled(
        split_order_id,
        order_id,
        liquidity_provider,
        settle_percent,
        lp_amount,
    );

    if (order.settled_lp_amount == lp_distributable) {
        if (order.sender_fee > 0) {
            let sender_fee_balance = order.escrow.split(order.sender_fee);
            transfer::public_transfer(
                coin::from_balance(sender_fee_balance, ctx),
                order.sender_fee_recipient,
            );
            events::emit_sender_fee_transferred(order.sender_fee_recipient, order.sender_fee);
        };
        if (order.protocol_fee > 0) {
            let protocol_fee_balance = order.escrow.split(order.protocol_fee);
            transfer::public_transfer(
                coin::from_balance(protocol_fee_balance, ctx),
                config::treasury(gw),
            );
        };
        order.status = STATUS_SETTLED;
    }
}

public fun refund_order<T>(
    _: &AggregatorCap,
    gw: &Gateway,
    order: &mut Order<T>,
    fee: u64,
    ctx: &mut TxContext,
) {
    assert!(order.status == STATUS_PENDING, ENotPending);

    let remaining = order.escrow.value();
    assert!(fee <= remaining, ERefundFeeTooHigh);

    let refund_amount = remaining - fee;

    if (fee > 0) {
        let fee_balance = order.escrow.split(fee);
        transfer::public_transfer(coin::from_balance(fee_balance, ctx), config::treasury(gw));
    };

    let refund_balance = order.escrow.withdraw_all();
    transfer::public_transfer(coin::from_balance(refund_balance, ctx), order.refund_address);

    order.status = STATUS_REFUNDED;

    let order_id = object::id(order);
    events::emit_order_refunded(fee, order_id, refund_amount);
}

public fun status<T>(o: &Order<T>): u8 { o.status }

public fun amount<T>(o: &Order<T>): u64 { o.amount }

public fun remaining<T>(o: &Order<T>): u64 { o.escrow.value() }

public fun sender<T>(o: &Order<T>): address { o.sender }

public fun rate<T>(o: &Order<T>): u64 { o.rate }

public fun settled_lp_amount<T>(o: &Order<T>): u64 { o.settled_lp_amount }

public fun protocol_fee<T>(o: &Order<T>): u64 { o.protocol_fee }

public fun sender_fee<T>(o: &Order<T>): u64 { o.sender_fee }

public fun status_pending(): u8 { STATUS_PENDING }

public fun status_settled(): u8 { STATUS_SETTLED }

public fun status_refunded(): u8 { STATUS_REFUNDED }
