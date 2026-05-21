module rails::events;

use std::string::String;
use sui::event;

public struct OrderCreated has copy, drop {
    order_id: ID,
    sender: address,
    coin_type: vector<u8>,
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

public(package) fun emit_order_created(
    order_id: ID,
    sender: address,
    coin_type: vector<u8>,
    amount: u64,
    protocol_fee: u64,
    rate: u64,
    institution_code: vector<u8>,
    message_hash: String,
) {
    event::emit(OrderCreated {
        order_id,
        sender,
        coin_type,
        amount,
        protocol_fee,
        rate,
        institution_code,
        message_hash,
    })
}

public(package) fun emit_order_settled(
    split_order_id: vector<u8>,
    order_id: ID,
    liquidity_provider: address,
    settle_percent: u64,
    amount_released: u64,
) {
    event::emit(OrderSettled {
        split_order_id,
        order_id,
        liquidity_provider,
        settle_percent,
        amount_released,
    })
}

public(package) fun emit_order_refunded(fee: u64, order_id: ID, amount_refunded: u64) {
    event::emit(OrderRefunded { fee, order_id, amount_refunded })
}

public(package) fun emit_sender_fee_transferred(sender_fee_recipient: address, amount: u64) {
    event::emit(SenderFeeTransferred { sender_fee_recipient, amount })
}
