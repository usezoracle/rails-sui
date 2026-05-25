// Package services — route_a_events.go is the write-side helper for
// the per-order audit log table (ent/schema/route_a_event.go).
//
// Usage from a step entry point:
//
//	defer events.Time(ctx, order.ID, events.StepBridgeSubmit,
//	    events.ActorDispatcher).
//	    With("from_amount", fromAmount).
//	    With("tool", quote.Tool).
//	    End(&err)
//
// Writes one `started` row immediately and one terminal
// (`succeeded` / `failed` / `skipped`) row when the deferred call
// fires, capturing the elapsed duration in `duration_ms`.
//
// For one-off events that don't need a duration pair, call
// events.LogOnce directly.
//
// Spawned by docs/route-a-hardening.md Phase 1. Read by
// /admin/orders/route-a/:id/events and scripts/order-status.

package services

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent/routeaevent"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// Convenience re-exports so callers depend on this package, not on
// the generated ent enum — keeps the call sites short and lets us
// rename a step value here if we ever rework the pipeline without
// rewriting every call site.
type (
	EventStep   = routeaevent.Step
	EventActor  = routeaevent.Actor
	EventStatus = routeaevent.Status
)

const (
	StepDepositCheck       EventStep = routeaevent.StepDepositCheck
	StepDepositDetected    EventStep = routeaevent.StepDepositDetected
	StepCreateOrder        EventStep = routeaevent.StepCreateOrder
	StepOrderCreatedEvent  EventStep = routeaevent.StepOrderCreatedEvent
	StepSelfSettle         EventStep = routeaevent.StepSelfSettle
	StepAwaitingFunds      EventStep = routeaevent.StepAwaitingFunds
	StepBridgeQuote        EventStep = routeaevent.StepBridgeQuote
	StepBridgeSubmit       EventStep = routeaevent.StepBridgeSubmit
	StepBridgePoll         EventStep = routeaevent.StepBridgePoll
	StepBridgeDone         EventStep = routeaevent.StepBridgeDone
	StepBridgeUncertain    EventStep = routeaevent.StepBridgeUncertain
	StepEvmApprove         EventStep = routeaevent.StepEvmApprove
	StepEvmCreateOrder     EventStep = routeaevent.StepEvmCreateOrder
	StepSettlementPoll     EventStep = routeaevent.StepSettlementPoll
	StepSettlementTerminal EventStep = routeaevent.StepSettlementTerminal
	StepRefundAttempt      EventStep = routeaevent.StepRefundAttempt
	StepRefundDone         EventStep = routeaevent.StepRefundDone
	StepManualOverride     EventStep = routeaevent.StepManualOverride
)

const (
	ActorWatcher    EventActor = routeaevent.ActorWatcher
	ActorIndexer    EventActor = routeaevent.ActorIndexer
	ActorDispatcher EventActor = routeaevent.ActorDispatcher
	ActorReconciler EventActor = routeaevent.ActorReconciler
	ActorOperator   EventActor = routeaevent.ActorOperator
	ActorSystem     EventActor = routeaevent.ActorSystem
)

const (
	StatusStarted   EventStatus = routeaevent.StatusStarted
	StatusSucceeded EventStatus = routeaevent.StatusSucceeded
	StatusFailed    EventStatus = routeaevent.StatusFailed
	StatusSkipped   EventStatus = routeaevent.StatusSkipped
	StatusRetrying  EventStatus = routeaevent.StatusRetrying
)

// LogOnce writes a single terminal event row (no start/end pair).
// Use for fire-and-forget observations — e.g., a reconciler noting
// that a stuck address still has a balance, or the operator forcing
// a state transition.
//
// Errors writing the audit log are logged but never returned —
// observability must never block business logic.
func LogOnce(
	ctx context.Context,
	orderID int,
	step EventStep,
	status EventStatus,
	actor EventActor,
	payload map[string]any,
	errMsg string,
	correlationID string,
) {
	if payload == nil {
		payload = map[string]any{}
	}
	_, err := db.Client.RouteAEvent.
		Create().
		SetStep(step).
		SetStatus(status).
		SetActor(actor).
		SetAt(time.Now()).
		SetPayload(payload).
		SetNillableErrorMsg(strPtrOrNil(errMsg)).
		SetCorrelationID(correlationID).
		SetRouteAOrderID(orderID).
		Save(ctx)
	if err != nil {
		logger.Errorf("route-a events: write %s/%s for order %d: %v",
			step, status, orderID, err)
	}
}

// Timer is the builder returned by Time(). Chain With(k, v) calls to
// attach payload fields and call End(&err) (typically via defer) to
// write the terminal row.
type Timer struct {
	ctx           context.Context
	orderID       int
	step          EventStep
	actor         EventActor
	startedAt     time.Time
	correlationID string
	payload       map[string]any
}

// Time writes a `started` row immediately and returns a Timer.
// Defer-call End(&err) to write the matching terminal row.
//
// The correlation_id is auto-generated UUID so all rows from a single
// invocation cluster together in the timeline view.
func Time(
	ctx context.Context,
	orderID int,
	step EventStep,
	actor EventActor,
) *Timer {
	t := &Timer{
		ctx:           ctx,
		orderID:       orderID,
		step:          step,
		actor:         actor,
		startedAt:     time.Now(),
		correlationID: uuid.NewString(),
		payload:       map[string]any{},
	}
	LogOnce(ctx, orderID, step, StatusStarted, actor, nil, "", t.correlationID)
	return t
}

// With attaches a payload field. Cheap — just buffers locally.
// Idiomatic to chain: events.Time(...).With("k", v).With("k2", v2).End(&err).
func (t *Timer) With(key string, value any) *Timer {
	if t == nil {
		return nil
	}
	t.payload[key] = value
	return t
}

// End writes the terminal row. Pass a pointer to the function's
// returned error; nil → succeeded, non-nil → failed (with err.Error()
// stored on the row).
//
// Pass a pointer to a SkipReason sentinel to write `skipped` instead.
func (t *Timer) End(err *error) {
	if t == nil {
		return
	}
	status := StatusSucceeded
	var msg string
	if err != nil && *err != nil {
		switch {
		case IsSkipSentinel(*err):
			status = StatusSkipped
			msg = (*err).Error()
		default:
			status = StatusFailed
			msg = (*err).Error()
		}
	}
	t.payload["duration_ms"] = time.Since(t.startedAt).Milliseconds()
	_, dberr := db.Client.RouteAEvent.
		Create().
		SetStep(t.step).
		SetStatus(status).
		SetActor(t.actor).
		SetAt(time.Now()).
		SetPayload(t.payload).
		SetDurationMs(time.Since(t.startedAt).Milliseconds()).
		SetNillableErrorMsg(strPtrOrNil(msg)).
		SetCorrelationID(t.correlationID).
		SetRouteAOrderID(t.orderID).
		Save(t.ctx)
	if dberr != nil {
		logger.Errorf("route-a events: write %s/%s for order %d: %v",
			t.step, status, t.orderID, dberr)
	}
}

// SkipSentinel is the marker interface for errors that should be
// recorded as `skipped` rather than `failed`. Existing sentinels
// like ErrAwaitingDepositAtAggregator should embed this.
type SkipSentinel interface {
	error
	skipSentinel()
}

// IsSkipSentinel reports whether err satisfies SkipSentinel — used
// by Timer.End to map an expected non-failure into a `skipped` row.
func IsSkipSentinel(err error) bool {
	type sentinel interface{ skipSentinel() }
	_, ok := err.(sentinel)
	return ok
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
