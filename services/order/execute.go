package order

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lockorderfulfillment"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/services/baas"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// ExecuteOrderService drives a matched order through the role a the aggregator
// provider *node* would otherwise play — except here the platform operates the
// LP's delegated BaaS sub-account instead of the LP running a node. The only
// difference from the aggregator is the executor: same fiat-first ordering, same
// accept → pay → fulfil → settle lifecycle.
//
// Fiat-first is deliberate and matches the aggregator: the recipient's Naira is paid
// from the LP's sub-account BEFORE any USDC escrow is released, so a settled LP
// can never have skipped the payout. Settlement (releasing USDC to the LP) is
// the reimbursement, and only fires once the fiat leg is CONFIRMED.
//
// Sequencing:
//
//	Execute (at match time):  float-check → claim/auto-accept → pay fiat
//	  ├─ fiat confirmed sync  → SettleAfterPayout (fulfil + settle)
//	  └─ fiat pending         → wait; webhook/reconcile calls SettleAfterPayout
//	on any failure → exclude this LP + return the order to pending for re-match.
type ExecuteOrderService struct{}

// NewExecuteOrderService constructs the service.
func NewExecuteOrderService() *ExecuteOrderService { return &ExecuteOrderService{} }

// Execute runs the platform-operated LP flow for orderID, assigned to providerID.
// It is launched as a goroutine by the matching engine when the LP runs no node.
// Idempotent via an atomic claim on fiat_payout_status. Returns an error only
// for logging; reassignment is handled internally so the order is never stranded.
func (s *ExecuteOrderService) Execute(ctx context.Context, orderID uuid.UUID, providerID string) error {
	rail := baas.Default()
	if rail == nil {
		logger.Warnf("execute %s: baas rail not configured; cannot pay", orderID)
		return errors.New("baas rail not configured")
	}

	order, err := db.Client.LockPaymentOrder.
		Query().
		Where(lockpaymentorder.IDEQ(orderID)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("execute %s: load order: %w", orderID, err)
	}
	// Already handled (claimed/paid/settled) → no-op.
	if order.FiatPayoutStatus != lockpaymentorder.FiatPayoutStatusNone {
		return nil
	}

	lp, err := db.Client.ProviderProfile.
		Query().
		Where(providerprofile.IDEQ(providerID)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("execute %s: load provider %s: %w", orderID, providerID, err)
	}
	if lp.SafehavenAccountNumber == "" || lp.SafehavenAccountID == "" {
		s.reassign(ctx, order, providerID, "LP sub-account not provisioned")
		return nil
	}
	if order.Institution == "" || order.AccountIdentifier == "" {
		// Recipient details are intrinsic to the order; reassignment won't help.
		s.fail(ctx, orderID, "missing recipient bank details")
		return nil
	}

	// NGN to pay = crypto amount × rate (the matcher's quote, priority_queue.go).
	amountNGN := order.Amount.Mul(order.Rate).RoundBank(0)

	// Float check against live balance — the fix for "matched but can't pay".
	// An LP whose delegated sub-account can't cover the order is excluded and
	// the order is re-matched to a funded LP, so auto-pay never fails for funds.
	acct, err := rail.GetAccount(ctx, lp.SafehavenAccountID)
	if err != nil {
		logger.Warnf("execute %s: balance read failed for LP %s: %v", orderID, providerID, err)
		s.reassign(ctx, order, providerID, "LP balance unreadable")
		return nil
	}
	if acct.Balance.LessThan(amountNGN) {
		logger.Infof("execute %s: LP %s float %s < %s NGN; reassigning", orderID, providerID, acct.Balance, amountNGN)
		s.reassign(ctx, order, providerID, "insufficient LP float")
		return nil
	}

	// Atomically claim the order: assign the LP (auto-accept → processing) and
	// move fiat payout none → pending. If another goroutine already claimed it,
	// rows==0 and we stop.
	ref := baas.PaymentReference("routeB", order.ID.String())
	claimed, err := db.Client.LockPaymentOrder.Update().
		Where(
			lockpaymentorder.IDEQ(orderID),
			lockpaymentorder.FiatPayoutStatusEQ(lockpaymentorder.FiatPayoutStatusNone),
		).
		SetProviderID(providerID).
		SetStatus(lockpaymentorder.StatusProcessing).
		SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusPending).
		SetFiatPayoutReference(ref).
		ClearFiatPayoutError().
		Save(ctx)
	if err != nil {
		return fmt.Errorf("execute %s: claim: %w", orderID, err)
	}
	if claimed == 0 {
		return nil // already claimed by another worker
	}

	// Live feed: the order is now assigned to this LP and processing.
	PublishOrderByID(EventOrderAssigned, orderID)

	// Pay the recipient's Naira from the LP's sub-account.
	ne, err := rail.NameEnquiry(ctx, order.Institution, order.AccountIdentifier)
	if err != nil {
		s.reassign(ctx, order, providerID, "name enquiry: "+err.Error())
		return nil
	}
	tr, err := rail.Transfer(ctx, baas.TransferRequest{
		NameEnquiryReference: ne.Reference,
		DebitAccountNumber:   lp.SafehavenAccountNumber,
		BeneficiaryBankCode:  order.Institution,
		BeneficiaryAccount:   order.AccountIdentifier,
		Amount:               amountNGN,
		Narration:            fmt.Sprintf("Order %s payout", order.ID),
		PaymentReference:     ref,
	})
	if err != nil {
		s.reassign(ctx, order, providerID, "transfer: "+err.Error())
		return nil
	}

	logger.Infof("execute %s: provider=%s ref=%s rail=%s status=%s amount=%s NGN",
		orderID, providerID, ref, rail.Name(), tr.RawStatus, amountNGN)

	switch tr.Status {
	case baas.TransferSuccess:
		s.setPayoutSuccess(ctx, orderID)
		PublishOrderByID(EventOrderPayout, orderID)
		return s.SettleAfterPayout(ctx, orderID)
	case baas.TransferFailed:
		s.reassign(ctx, order, providerID, "rail rejected: "+tr.RawStatus)
		return nil
	default:
		// Pending — record the session id; webhook/reconcile converges it and
		// then calls SettleAfterPayout. Do NOT settle yet.
		if err := db.Client.LockPaymentOrder.UpdateOneID(orderID).
			SetFiatPayoutSessionID(tr.Reference).
			Exec(ctx); err != nil {
			logger.Errorf("execute %s: persist session id: %v", orderID, err)
		}
		PublishOrderByID(EventOrderPayout, orderID)
		return nil
	}
}

// SettleAfterPayout records the fulfilment and releases the LP's USDC by calling
// settle on-chain. Called once the fiat leg is CONFIRMED (sync success, webhook,
// or reconcile). Idempotent: it only advances an order that is still processing.
func (s *ExecuteOrderService) SettleAfterPayout(ctx context.Context, orderID uuid.UUID) error {
	order, err := db.Client.LockPaymentOrder.
		Query().
		Where(lockpaymentorder.IDEQ(orderID)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("settle-after-payout %s: load: %w", orderID, err)
	}
	// Only a processing order with a confirmed payout advances. Anything already
	// validated/settled is a no-op (idempotent).
	if order.Status != lockpaymentorder.StatusProcessing {
		return nil
	}
	if order.FiatPayoutStatus != lockpaymentorder.FiatPayoutStatusSuccess {
		return nil
	}

	// Record the payout as a fulfilment (the on-chain settle's audit trail),
	// keyed by the idempotent payout reference so retries don't duplicate it.
	ref := order.FiatPayoutReference
	exists, err := db.Client.LockOrderFulfillment.
		Query().
		Where(lockorderfulfillment.TxIDEQ(ref)).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("settle-after-payout %s: check fulfilment: %w", orderID, err)
	}
	if !exists {
		if _, err := db.Client.LockOrderFulfillment.
			Create().
			SetOrderID(orderID).
			SetTxID(ref).
			SetPsp(baas.Default().Name()).
			SetValidationStatus(lockorderfulfillment.ValidationStatusSuccess).
			Save(ctx); err != nil {
			return fmt.Errorf("settle-after-payout %s: create fulfilment: %w", orderID, err)
		}
	}

	if err := db.Client.LockPaymentOrder.UpdateOneID(orderID).
		SetStatus(lockpaymentorder.StatusValidated).
		Exec(ctx); err != nil {
		return fmt.Errorf("settle-after-payout %s: set validated: %w", orderID, err)
	}

	// Release the LP's USDC. The OrderSettled event then drives the order to
	// settled via the indexer.
	if err := NewOrderSui().SettleOrder(ctx, orderID); err != nil {
		logger.Errorf("settle-after-payout %s: SettleOrder: %v", orderID, err)
		return err
	}
	return nil
}

// setPayoutSuccess marks the fiat leg confirmed.
func (s *ExecuteOrderService) setPayoutSuccess(ctx context.Context, orderID uuid.UUID) {
	if err := db.Client.LockPaymentOrder.UpdateOneID(orderID).
		SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusSuccess).
		ClearFiatPayoutSessionID().
		ClearFiatPayoutError().
		Exec(ctx); err != nil {
		logger.Errorf("execute %s: mark payout success: %v", orderID, err)
	}
}

// fail marks a payout terminally failed without reassignment (used when the
// failure is intrinsic to the order, not the LP).
func (s *ExecuteOrderService) fail(ctx context.Context, orderID uuid.UUID, reason string) {
	if err := db.Client.LockPaymentOrder.UpdateOneID(orderID).
		SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusFailed).
		SetFiatPayoutError(reason).
		ClearFiatPayoutSessionID().
		Exec(ctx); err != nil {
		logger.Errorf("execute %s: mark failed: %v", orderID, err)
	}
}

// reassign excludes this LP for this order and returns the order to pending so
// the reassignment crons re-match it to another (funded) LP. Resets the payout
// claim so the next executor can claim cleanly.
func (s *ExecuteOrderService) reassign(ctx context.Context, order *ent.LockPaymentOrder, providerID, reason string) {
	logger.Infof("execute %s: reassigning away from LP %s: %s", order.ID, providerID, reason)

	if err := db.RedisClient.RPush(ctx,
		fmt.Sprintf("order_exclude_list_%s", order.ID), providerID).Err(); err != nil {
		logger.Errorf("execute %s: push exclude list: %v", order.ID, err)
	}
	// Drop the stale order_request key so the reassignment cron re-matches now
	// instead of waiting out its TTL.
	if err := db.RedisClient.Del(ctx, fmt.Sprintf("order_request_%s", order.ID)).Err(); err != nil {
		logger.Errorf("execute %s: clear order_request: %v", order.ID, err)
	}

	if err := db.Client.LockPaymentOrder.UpdateOneID(order.ID).
		SetStatus(lockpaymentorder.StatusPending).
		ClearProvider().
		SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusNone).
		SetFiatPayoutError(reason).
		ClearFiatPayoutSessionID().
		Exec(ctx); err != nil {
		logger.Errorf("execute %s: reset for reassignment: %v", order.ID, err)
	}
}
