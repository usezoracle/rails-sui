#[test_only]
module rails::tapp_card_tests;

use sui::clock;
use sui::coin::{Self, Coin};
use sui::test_scenario::{Self as ts, Scenario};

use rails::config::{Self, AdminCap, AggregatorCap};
use rails::tapp_card::{Self, CardSpendingCap};

public struct USDC has drop {}

const ADMIN: address = @0xA;
const AGGREGATOR: address = @0xB;
const ALICE: address = @0xA11CE;
const MERCHANT: address = @0xBEEF;
const BOB: address = @0xB0B;

const MS_PER_DAY: u64 = 86_400_000;

// 32-byte hash placeholder — content doesn't matter for tests beyond length.
fun uid_hash(): vector<u8> {
    let mut h: vector<u8> = vector::empty();
    let mut i: u64 = 0;
    while (i < 32) {
        h.push_back(((i as u8) + 1) % 251);
        i = i + 1;
    };
    h
}

fun setup(): Scenario {
    let mut sc = ts::begin(ADMIN);
    {
        config::init_for_testing(sc.ctx());
    };
    sc.next_tx(ADMIN);
    {
        let admin_cap = sc.take_from_sender<AdminCap>();
        config::mint_aggregator_cap(&admin_cap, AGGREGATOR, sc.ctx());
        sc.return_to_sender(admin_cap);
    };
    sc
}

fun mint_usdc(amount: u64, ctx: &mut TxContext): Coin<USDC> {
    coin::mint_for_testing<USDC>(amount, ctx)
}

fun create_cap_for(
    sc: &mut Scenario,
    funding: u64,
    daily: u64,
    per_tap: u64,
) {
    sc.next_tx(ALICE);
    {
        let funding_coin = mint_usdc(funding, sc.ctx());
        tapp_card::create_cap<USDC>(
            funding_coin,
            daily,
            per_tap,
            uid_hash(),
            sc.ctx(),
        );
    };
}

#[test]
fun create_then_debit_happy_path() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 500_000, 100_000);

    sc.next_tx(AGGREGATOR);
    {
        let agg = sc.take_from_sender<AggregatorCap>();
        let mut cap = sc.take_shared<CardSpendingCap<USDC>>();
        let clk = clock::create_for_testing(sc.ctx());

        tapp_card::debit<USDC>(
            &agg,
            &mut cap,
            50_000,
            MERCHANT,
            b"order-uuid-bytes",
            &clk,
            sc.ctx(),
        );

        assert!(tapp_card::balance_value(&cap) == 950_000, 0);
        assert!(tapp_card::spent_today(&cap) == 50_000, 1);

        clock::destroy_for_testing(clk);
        ts::return_shared(cap);
        sc.return_to_sender(agg);
    };

    sc.end();
}

#[test]
#[expected_failure(abort_code = tapp_card::EOverPerTapLimit)]
fun debit_over_per_tap_aborts() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 500_000, 100_000);

    sc.next_tx(AGGREGATOR);
    {
        let agg = sc.take_from_sender<AggregatorCap>();
        let mut cap = sc.take_shared<CardSpendingCap<USDC>>();
        let clk = clock::create_for_testing(sc.ctx());

        tapp_card::debit<USDC>(
            &agg,
            &mut cap,
            100_001, // > per_tap_limit
            MERCHANT,
            b"order",
            &clk,
            sc.ctx(),
        );

        clock::destroy_for_testing(clk);
        ts::return_shared(cap);
        sc.return_to_sender(agg);
    };

    sc.end();
}

#[test]
#[expected_failure(abort_code = tapp_card::EOverDailyLimit)]
fun debit_crossing_daily_aborts() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 250_000, 100_000);

    sc.next_tx(AGGREGATOR);
    {
        let agg = sc.take_from_sender<AggregatorCap>();
        let mut cap = sc.take_shared<CardSpendingCap<USDC>>();
        let clk = clock::create_for_testing(sc.ctx());

        // Three 100k debits + the fourth should breach 250k cap.
        tapp_card::debit<USDC>(&agg, &mut cap, 100_000, MERCHANT, b"o1", &clk, sc.ctx());
        tapp_card::debit<USDC>(&agg, &mut cap, 100_000, MERCHANT, b"o2", &clk, sc.ctx());
        tapp_card::debit<USDC>(&agg, &mut cap, 60_000,  MERCHANT, b"o3", &clk, sc.ctx());
        // Total so far = 260_000 > 250_000 daily — aborts here.

        clock::destroy_for_testing(clk);
        ts::return_shared(cap);
        sc.return_to_sender(agg);
    };

    sc.end();
}

#[test]
fun day_rollover_resets_spent_today() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 200_000, 100_000);

    sc.next_tx(AGGREGATOR);
    {
        let agg = sc.take_from_sender<AggregatorCap>();
        let mut cap = sc.take_shared<CardSpendingCap<USDC>>();
        let mut clk = clock::create_for_testing(sc.ctx());

        // Day 0: spend 100k.
        tapp_card::debit<USDC>(&agg, &mut cap, 100_000, MERCHANT, b"o1", &clk, sc.ctx());
        assert!(tapp_card::spent_today(&cap) == 100_000, 0);

        // Advance clock to day 5.
        clock::set_for_testing(&mut clk, 5 * MS_PER_DAY);

        // First debit on the new day should reset the counter.
        tapp_card::debit<USDC>(&agg, &mut cap, 75_000, MERCHANT, b"o2", &clk, sc.ctx());
        assert!(tapp_card::spent_today(&cap) == 75_000, 1);
        assert!(tapp_card::day_index(&cap) == 5, 2);

        clock::destroy_for_testing(clk);
        ts::return_shared(cap);
        sc.return_to_sender(agg);
    };

    sc.end();
}

#[test]
#[expected_failure(abort_code = tapp_card::ERevoked)]
fun debit_on_revoked_aborts() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 200_000, 100_000);

    // Owner revokes.
    sc.next_tx(ALICE);
    {
        let mut cap = sc.take_shared<CardSpendingCap<USDC>>();
        tapp_card::set_revoked<USDC>(&mut cap, true, sc.ctx());
        ts::return_shared(cap);
    };

    sc.next_tx(AGGREGATOR);
    {
        let agg = sc.take_from_sender<AggregatorCap>();
        let mut cap = sc.take_shared<CardSpendingCap<USDC>>();
        let clk = clock::create_for_testing(sc.ctx());

        tapp_card::debit<USDC>(&agg, &mut cap, 50_000, MERCHANT, b"o", &clk, sc.ctx());

        clock::destroy_for_testing(clk);
        ts::return_shared(cap);
        sc.return_to_sender(agg);
    };

    sc.end();
}

#[test]
#[expected_failure(abort_code = tapp_card::EWrongOwner)]
fun non_owner_cannot_destroy() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 200_000, 100_000);

    // BOB tries to destroy Alice's cap — should abort.
    sc.next_tx(BOB);
    {
        let cap = sc.take_shared<CardSpendingCap<USDC>>();
        tapp_card::destroy_and_reclaim<USDC>(cap, sc.ctx());
    };

    sc.end();
}

#[test]
fun owner_can_destroy_and_reclaim() {
    let mut sc = setup();
    create_cap_for(&mut sc, 1_000_000, 200_000, 100_000);

    sc.next_tx(ALICE);
    {
        let cap = sc.take_shared<CardSpendingCap<USDC>>();
        tapp_card::destroy_and_reclaim<USDC>(cap, sc.ctx());
    };

    // Alice should now hold a Coin<USDC> with the full funded amount.
    sc.next_tx(ALICE);
    {
        let refund = sc.take_from_sender<Coin<USDC>>();
        assert!(coin::value(&refund) == 1_000_000, 0);
        coin::burn_for_testing(refund);
    };

    sc.end();
}
