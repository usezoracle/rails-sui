package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// RouteAEvent is the append-only audit log for a Route-A order's lifecycle.
//
// Every state transition or side-effect (watcher detection, indexer call,
// dispatcher action, operator override) writes one or more rows here so
// an operator can reconstruct exactly what happened to an order without
// chasing on-chain transactions, structured logs, and DB columns
// independently.
//
// Pattern:
//
//	defer events.Time(ctx, orderID, StepBridgeSubmit, ActorDispatcher)(&err)
//
// writes a `started` row at function entry and a `succeeded` or `failed`
// row at exit, with the elapsed duration captured automatically.
//
// Spawned by docs/route-a-hardening.md Phase 1; consumed by the
// /admin/orders/route-a/:id/events endpoint and the order-status CLI.
type RouteAEvent struct {
	ent.Schema
}

func (RouteAEvent) Mixin() []ent.Mixin {
	// Only created_at — events are append-only, no updates.
	return []ent.Mixin{}
}

func (RouteAEvent) Fields() []ent.Field {
	return []ent.Field{
		// Which step of the pipeline produced this event. New values
		// can be added without a destructive migration; old rows
		// referring to deprecated steps are left as historical record.
		field.Enum("step").Values(
			// Deposit-watcher path (services/sui_deposit_watcher.go)
			"deposit_check",
			"deposit_detected",
			"create_order",
			// Indexer path (services/sui_event_indexer.go)
			"order_created_event",
			"self_settle",
			// Dispatcher path (services/route_a_dispatcher.go)
			"awaiting_funds",
			"bridge_quote",
			"bridge_submit",
			"bridge_poll",
			"bridge_done",
			"bridge_uncertain",
			"evm_approve",
			"evm_create_order",
			"settlement_poll",
			"settlement_terminal",
			// Route C (managed float, services/route_a_treasury.go)
			"treasury_payout",
			"float_reload",
			// Refund + operator override
			"refund_attempt",
			"refund_done",
			"manual_override",
		),

		// Outcome. `started` is written at entry; one of the others is
		// written at exit. `skipped` is critical — it makes the
		// previously-silent "didn't match, moved on" path visible.
		field.Enum("status").Values(
			"started", "succeeded", "failed", "skipped", "retrying",
		),

		// Who or what produced this event. Lets us filter the timeline
		// by subsystem (e.g. "show me all dispatcher actions" vs
		// "show me everything the operator did manually").
		field.Enum("actor").Values(
			"watcher", "indexer", "dispatcher", "reconciler",
			"operator", "system",
		),

		field.Time("at").
			Immutable(),

		// Elapsed millis between matching `started` and terminal row.
		// Nullable because `started` rows haven't elapsed anything yet,
		// and terminal-without-started events (e.g. async webhook
		// receipts) skip the timing pair.
		field.Int64("duration_ms").Optional().Nillable(),

		// Free-form context: tx digests, quote IDs, amounts, upstream
		// HTTP status, balances seen, etc. JSONB so we can index/filter
		// on fields later without schema changes.
		field.JSON("payload", map[string]any{}).
			SchemaType(map[string]string{
				dialect.Postgres: "jsonb",
			}).
			Optional(),

		field.Text("error_msg").Optional(),

		// Sticky across a single chain of steps so a multi-step
		// operation can be grouped (e.g., one tick of the dispatcher
		// might emit quote + submit + poll events under the same id).
		field.String("correlation_id").Optional(),

		// created_at is part of the table; we use `at` as the canonical
		// event timestamp because it's set at the moment the event
		// happens (which may pre-date the row insert by a few ms under
		// load). Both fields are kept for debugging clock skew.
		field.Time("created_at").
			Immutable().
			Default(time.Now),
	}
}

func (RouteAEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("route_a_order", RouteAOrder.Type).
			Ref("events").
			Unique().
			Required(),
	}
}

func (RouteAEvent) Indexes() []ent.Index {
	return []ent.Index{
		// Primary query: "give me this order's timeline."
		index.Fields("at").Edges("route_a_order"),
		// Secondary: "find all failed events of this step in a window."
		index.Fields("step", "status", "at"),
	}
}
