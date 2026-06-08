package order

import (
	"context"

	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/services/livefeed"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// Provider live-feed event names (SSE `event:` field on /v1/provider/events).
const (
	EventOrderAssigned = "order.assigned" // matched + claimed → appears in the LP's feed
	EventOrderPayout   = "order.payout"   // fiat payout status changed (pending/success/failed)
	EventOrderSettled  = "order.settled"  // USDC released on-chain to the LP
)

// PublishProviderEvent pushes a snapshot of an order to the assigned provider's
// live dashboard stream. Best-effort: a missing provider id is a no-op, and the
// bus never blocks the caller. Payload mirrors the orders-list response shape so
// the frontend can update a row in place by id.
func PublishProviderEvent(providerID, event string, o *ent.LockPaymentOrder) {
	if providerID == "" || o == nil {
		return
	}
	livefeed.Default().Publish(providerID, event, map[string]any{
		"orderId":          o.ID.String(),
		"status":           o.Status,
		"fiatPayoutStatus": o.FiatPayoutStatus,
		"amount":           o.Amount.String(),
		"rate":             o.Rate.String(),
		"institution":      o.Institution,
		"accountName":      o.AccountName,
		"updatedAt":        o.UpdatedAt,
	})
}

// PublishOrderByID loads the current order + assigned provider and pushes a
// fresh snapshot to that provider's live stream. Best-effort; safe to call in a
// goroutine after any state change.
func PublishOrderByID(event string, orderID uuid.UUID) {
	o, err := db.Client.LockPaymentOrder.
		Query().
		Where(lockpaymentorder.IDEQ(orderID)).
		WithProvider().
		Only(context.Background())
	if err != nil {
		logger.Errorf("live feed %s: load order %s: %v", event, orderID, err)
		return
	}
	if o.Edges.Provider == nil {
		return
	}
	PublishProviderEvent(o.Edges.Provider.ID, event, o)
}
