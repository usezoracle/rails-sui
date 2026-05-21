#[test_only]
module rails::order_tests;

use std::string;
use sui::clock;
use sui::coin::{Self, Coin};
use sui::test_scenario::{Self as ts, Scenario};

use rails::config::{Self, Gateway, AdminCap, AggregatorCap};
use rails::order::{Self, Order};

public struct USDC has drop {}

const ADMIN: address = @0xA;
const AGGREGATOR: address = @0xB;
const ALICE: address = @0xA11CE;
const LP1: address = @0x11;
const LP2: address = @0x12;
const SENDER_FEE_RECIPIENT: address = @0xF;
const REFUND_ADDR: address = @0xCAFE;

const FOUR_DIGITS_SCALE: u64 = 1_000_000;

fun setup(): Scenario {
    let mut sc = ts::begin(ADMIN);
    {
        config::init_for_testing(sc.ctx());
    };
    sc.next_tx(ADMIN);
    {
        let admin_cap = sc.take_from_sender<AdminCap>();
        let mut gw = sc.take_shared<Gateway>();
        config::add_supported_coin<USDC>(&admin_cap, &mut gw);
        config::mint_aggregator_cap(&admin_cap, AGGREGATOR, sc.ctx());
        ts::return_shared(gw);
        sc.return_to_sender(admin_cap);
    };
    sc
}

fun mint_usdc(amount: u64, ctx: &mut TxContext): Coin<USDC> {
    coin::mint_for_testing<USDC>(amount, ctx)
}

#[test]
fun create_order_happy_path() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"msg_hash_abc"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.next_tx(ALICE);
    {
        let order = sc.take_shared<Order<USDC>>();
        assert!(order::status(&order) == order::status_pending(), 0);
        assert!(order::amount(&order) == 100 * FOUR_DIGITS_SCALE, 1);
        assert!(order::remaining(&order) == 100 * FOUR_DIGITS_SCALE, 2);
        assert!(order::sender(&order) == ALICE, 3);
        ts::return_shared(order);
    };
    sc.end();
}

#[test]
#[expected_failure(abort_code = 2, location = rails::order)]
fun create_order_rejects_unsupported_coin() {
    let mut sc = ts::begin(ADMIN);
    {
        config::init_for_testing(sc.ctx());
    };
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"msg"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.end();
}

#[test]
#[expected_failure(abort_code = 3, location = rails::order)]
fun create_order_rejects_zero_amount() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(0, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.end();
}

#[test]
#[expected_failure(abort_code = 4, location = rails::order)]
fun create_order_rejects_zero_refund_address() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            0,
            SENDER_FEE_RECIPIENT,
            @0x0,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.end();
}

#[test]
fun settle_order_full_to_single_lp() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.next_tx(AGGREGATOR);
    {
        let cap = sc.take_from_sender<AggregatorCap>();
        let gw = sc.take_shared<Gateway>();
        let mut order = sc.take_shared<Order<USDC>>();
        order::settle_order<USDC>(
            &cap,
            &gw,
            &mut order,
            LP1,
            10_000,
            b"split_1",
            sc.ctx(),
        );
        assert!(order::status(&order) == order::status_settled(), 0);
        assert!(order::settled_lp_amount(&order) == 100 * FOUR_DIGITS_SCALE, 1);
        assert!(order::remaining(&order) == 0, 2);
        ts::return_shared(order);
        ts::return_shared(gw);
        sc.return_to_sender(cap);
    };
    sc.next_tx(LP1);
    {
        let received = sc.take_from_sender<Coin<USDC>>();
        assert!(received.value() == 100 * FOUR_DIGITS_SCALE, 0);
        sc.return_to_sender(received);
    };
    sc.end();
}

#[test]
fun settle_order_partial_then_complete() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.next_tx(AGGREGATOR);
    {
        let cap = sc.take_from_sender<AggregatorCap>();
        let gw = sc.take_shared<Gateway>();
        let mut order = sc.take_shared<Order<USDC>>();
        order::settle_order<USDC>(
            &cap,
            &gw,
            &mut order,
            LP1,
            6_000,
            b"split_1",
            sc.ctx(),
        );
        assert!(order::status(&order) == order::status_pending(), 0);
        order::settle_order<USDC>(
            &cap,
            &gw,
            &mut order,
            LP2,
            4_000,
            b"split_2",
            sc.ctx(),
        );
        assert!(order::status(&order) == order::status_settled(), 1);
        assert!(order::remaining(&order) == 0, 2);
        ts::return_shared(order);
        ts::return_shared(gw);
        sc.return_to_sender(cap);
    };
    sc.next_tx(LP1);
    {
        let received = sc.take_from_sender<Coin<USDC>>();
        assert!(received.value() == 60 * FOUR_DIGITS_SCALE, 0);
        sc.return_to_sender(received);
    };
    sc.next_tx(LP2);
    {
        let received = sc.take_from_sender<Coin<USDC>>();
        assert!(received.value() == 40 * FOUR_DIGITS_SCALE, 0);
        sc.return_to_sender(received);
    };
    sc.end();
}

#[test]
#[expected_failure(abort_code = 6, location = rails::order)]
fun settle_aborts_when_already_settled() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.next_tx(AGGREGATOR);
    {
        let cap = sc.take_from_sender<AggregatorCap>();
        let gw = sc.take_shared<Gateway>();
        let mut order = sc.take_shared<Order<USDC>>();
        order::settle_order<USDC>(&cap, &gw, &mut order, LP1, 10_000, b"s1", sc.ctx());
        order::settle_order<USDC>(&cap, &gw, &mut order, LP2, 1, b"s2", sc.ctx());
        ts::return_shared(order);
        ts::return_shared(gw);
        sc.return_to_sender(cap);
    };
    sc.end();
}

#[test]
fun refund_order_happy_path() {
    let mut sc = setup();
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            0,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.next_tx(AGGREGATOR);
    {
        let cap = sc.take_from_sender<AggregatorCap>();
        let gw = sc.take_shared<Gateway>();
        let mut order = sc.take_shared<Order<USDC>>();
        order::refund_order<USDC>(&cap, &gw, &mut order, 0, sc.ctx());
        assert!(order::status(&order) == order::status_refunded(), 0);
        assert!(order::remaining(&order) == 0, 1);
        ts::return_shared(order);
        ts::return_shared(gw);
        sc.return_to_sender(cap);
    };
    sc.next_tx(REFUND_ADDR);
    {
        let refunded = sc.take_from_sender<Coin<USDC>>();
        assert!(refunded.value() == 100 * FOUR_DIGITS_SCALE, 0);
        sc.return_to_sender(refunded);
    };
    sc.end();
}

#[test]
fun create_order_with_protocol_fee_and_sender_fee() {
    let mut sc = setup();
    sc.next_tx(ADMIN);
    {
        let admin_cap = sc.take_from_sender<AdminCap>();
        let mut gw = sc.take_shared<Gateway>();
        config::set_protocol_fee(&admin_cap, &mut gw, 100);
        ts::return_shared(gw);
        sc.return_to_sender(admin_cap);
    };
    sc.next_tx(ALICE);
    {
        let gw = sc.take_shared<Gateway>();
        let payment = mint_usdc(100 * FOUR_DIGITS_SCALE, sc.ctx());
        let clk = clock::create_for_testing(sc.ctx());
        order::create_order<USDC>(
            &gw,
            payment,
            1_530_500_000,
            b"044",
            string::utf8(b"m"),
            500_000,
            SENDER_FEE_RECIPIENT,
            REFUND_ADDR,
            &clk,
            sc.ctx(),
        );
        clock::destroy_for_testing(clk);
        ts::return_shared(gw);
    };
    sc.next_tx(AGGREGATOR);
    {
        let cap = sc.take_from_sender<AggregatorCap>();
        let gw = sc.take_shared<Gateway>();
        let mut order = sc.take_shared<Order<USDC>>();
        assert!(order::protocol_fee(&order) == 1_000_000, 0);
        assert!(order::sender_fee(&order) == 500_000, 1);
        order::settle_order<USDC>(&cap, &gw, &mut order, LP1, 10_000, b"s", sc.ctx());
        assert!(order::status(&order) == order::status_settled(), 2);
        assert!(order::remaining(&order) == 0, 3);
        ts::return_shared(order);
        ts::return_shared(gw);
        sc.return_to_sender(cap);
    };
    sc.next_tx(LP1);
    {
        let received = sc.take_from_sender<Coin<USDC>>();
        assert!(received.value() == 100 * FOUR_DIGITS_SCALE - 1_000_000 - 500_000, 0);
        sc.return_to_sender(received);
    };
    sc.next_tx(SENDER_FEE_RECIPIENT);
    {
        let received = sc.take_from_sender<Coin<USDC>>();
        assert!(received.value() == 500_000, 0);
        sc.return_to_sender(received);
    };
    sc.next_tx(ADMIN);
    {
        let received = sc.take_from_sender<Coin<USDC>>();
        assert!(received.value() == 1_000_000, 0);
        sc.return_to_sender(received);
    };
    sc.end();
}

