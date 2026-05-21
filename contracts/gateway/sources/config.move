module rails::config;

use std::type_name::{Self, TypeName};
use sui::vec_set::{Self, VecSet};

const MAX_BPS: u64 = 10_000;

const EFeeTooHigh: u64 = 100;

public struct Gateway has key {
    id: UID,
    supported_coins: VecSet<TypeName>,
    protocol_fee_bps: u64,
    max_bps: u64,
    treasury: address,
    paused: bool,
}

public struct AdminCap has key, store {
    id: UID,
}

public struct AggregatorCap has key, store {
    id: UID,
}

fun init(ctx: &mut TxContext) {
    let gateway = Gateway {
        id: object::new(ctx),
        supported_coins: vec_set::empty(),
        protocol_fee_bps: 0,
        max_bps: MAX_BPS,
        treasury: ctx.sender(),
        paused: false,
    };
    transfer::share_object(gateway);

    let admin_cap = AdminCap { id: object::new(ctx) };
    transfer::public_transfer(admin_cap, ctx.sender());
}

public fun add_supported_coin<T>(_: &AdminCap, gw: &mut Gateway) {
    gw.supported_coins.insert(type_name::with_defining_ids<T>());
}

public fun remove_supported_coin<T>(_: &AdminCap, gw: &mut Gateway) {
    gw.supported_coins.remove(&type_name::with_defining_ids<T>());
}

public fun set_protocol_fee(_: &AdminCap, gw: &mut Gateway, bps: u64) {
    assert!(bps <= MAX_BPS, EFeeTooHigh);
    gw.protocol_fee_bps = bps;
}

public fun set_max_bps(_: &AdminCap, gw: &mut Gateway, bps: u64) {
    assert!(bps <= MAX_BPS, EFeeTooHigh);
    gw.max_bps = bps;
}

public fun set_treasury(_: &AdminCap, gw: &mut Gateway, t: address) {
    gw.treasury = t;
}

public fun pause(_: &AdminCap, gw: &mut Gateway) {
    gw.paused = true;
}

public fun unpause(_: &AdminCap, gw: &mut Gateway) {
    gw.paused = false;
}

public fun mint_aggregator_cap(_: &AdminCap, recipient: address, ctx: &mut TxContext) {
    let cap = AggregatorCap { id: object::new(ctx) };
    transfer::public_transfer(cap, recipient);
}

public fun is_coin_supported<T>(gw: &Gateway): bool {
    gw.supported_coins.contains(&type_name::with_defining_ids<T>())
}

public fun protocol_fee_bps(gw: &Gateway): u64 { gw.protocol_fee_bps }

public fun max_bps(gw: &Gateway): u64 { gw.max_bps }

public fun treasury(gw: &Gateway): address { gw.treasury }

public fun is_paused(gw: &Gateway): bool { gw.paused }

#[test_only]
public fun init_for_testing(ctx: &mut TxContext) {
    init(ctx)
}
