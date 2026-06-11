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
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
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

func hasSuccessfulRouteAEvent(ctx context.Context, orderID int, step EventStep) bool {
	ok, err := db.Client.RouteAEvent.
		Query().
		Where(
			routeaevent.StepEQ(step),
			routeaevent.StatusEQ(StatusSucceeded),
			routeaevent.HasRouteAOrderWith(routeaorder.IDEQ(orderID)),
		).
		Exist(ctx)
	if err != nil {
		logger.Errorf("route-a events: lookup %s/%s for order %d: %v",
			step, StatusSucceeded, orderID, err)
		return false
	}
	return ok
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

	// Sampling state (TimeSampled / TimePollSampled). A lazy timer
	// wrote no `started` row and suppresses its terminal row unless
	// forced (Milestone), or — for submit-style steps — the attempt
	// succeeded. Keeps hot 10-second retry loops from writing
	// thousands of identical rows (the 2026-06-11 order produced a
	// 1,000+ step timeline overnight).
	lazy      bool
	force     bool
	pollStyle bool
}

// attemptCounters tracks consecutive attempts per (order, step) for
// sampling. Reset when a submit-style step succeeds (the loop exits).
var (
	attemptCountersMu sync.Mutex
	attemptCounters   = map[string]int{}
)

func nextAttempt(orderID int, step EventStep) int {
	key := fmt.Sprintf("%d/%s", orderID, step)
	attemptCountersMu.Lock()
	defer attemptCountersMu.Unlock()
	attemptCounters[key]++
	return attemptCounters[key]
}

func resetAttempts(orderID int, step EventStep) {
	key := fmt.Sprintf("%d/%s", orderID, step)
	attemptCountersMu.Lock()
	defer attemptCountersMu.Unlock()
	delete(attemptCounters, key)
}

// TimeSampled is Time() for submit-style retry loops (bridge_submit,
// evm_create_order): attempts 1–3 and every 10th are fully recorded;
// the rest record only if they succeed. A success always writes (and
// resets the counter) — the moment an order finally bridges or
// dispatches must never be invisible.
func TimeSampled(ctx context.Context, orderID int, step EventStep, actor EventActor) *Timer {
	n := nextAttempt(orderID, step)
	if n <= 3 || n%10 == 0 {
		t := Time(ctx, orderID, step, actor)
		t.payload["attempt"] = n
		return t
	}
	return &Timer{
		ctx: ctx, orderID: orderID, step: step, actor: actor,
		startedAt: time.Now(), correlationID: uuid.NewString(),
		payload: map[string]any{"attempt": n}, lazy: true,
	}
}

// TimePollSampled is Time() for poll-style loops (bridge_poll,
// bridge_uncertain): most polls are boring ("still pending") and are
// suppressed; a heartbeat row lands every 30th attempt (~5 min at the
// 10s tick), and call sites mark state-changing polls with
// Milestone() to force the write.
func TimePollSampled(ctx context.Context, orderID int, step EventStep, actor EventActor) *Timer {
	n := nextAttempt(orderID, step)
	if n <= 1 || n%30 == 0 {
		t := Time(ctx, orderID, step, actor)
		t.payload["attempt"] = n
		t.pollStyle = true
		return t
	}
	return &Timer{
		ctx: ctx, orderID: orderID, step: step, actor: actor,
		startedAt: time.Now(), correlationID: uuid.NewString(),
		payload: map[string]any{"attempt": n}, lazy: true, pollStyle: true,
	}
}

// Milestone marks this attempt as state-changing — its row is written
// even if sampling would have suppressed it.
func (t *Timer) Milestone() *Timer {
	if t != nil {
		t.force = true
	}
	return t
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

	// Submit-style sampled steps: success ends the retry loop — reset
	// the attempt counter and always record it.
	if !t.pollStyle && status == StatusSucceeded {
		resetAttempts(t.orderID, t.step)
	}
	if t.lazy && !t.force {
		if t.pollStyle || status != StatusSucceeded {
			return // suppressed boring/repeat attempt
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
