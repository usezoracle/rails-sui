package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/shopspring/decimal"
)

// RouteAOrder tracks one Route-A bridge order (Sui USDC → LiFi → BSC USDC →
// fiat). One row per integrator order that picks route="route_a"; lives
// alongside the standard PaymentOrder.
//
// State machine:
//
//	pending      → quote retrieved, awaiting source-chain tx submit
//	bridging     → source tx submitted, polling LiFi /status
//	bridged      → LiFi reports DONE — BSC USDC sitting in our hot wallet
//	dispatching  → post-bridge dispatch in flight (LP-on-BSC or treasury)
//	settled      → fiat hit merchant; SuiEventIndexer can settle the Sui Order
//	failed       → bridge or dispatch failed; reconciliation will refund
type RouteAOrder struct {
	ent.Schema
}

func (RouteAOrder) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (RouteAOrder) Fields() []ent.Field {
	return []ent.Field{
		// Post-bridge target: "lp" (BSC EVM Gateway re-entry) or "treasury"
		// (centralized BaaS payout). Set by the integrator at order creation.
		field.Enum("mode").
			Values("lp", "treasury").
			Default("treasury"),

		// LiFi's quote ID for this bridge run; useful for /status correlation
		// and support tickets.
		field.String("lifi_quote_id").Optional(),

		// LiFi-chosen bridge tool (e.g. "wormhole", "mayan"); the /status
		// endpoint wants this as a hint.
		field.String("lifi_tool").Optional(),

		// Source-chain (Sui) tx digest of the bridge submission.
		field.String("bridge_tx_sui").Optional(),

		// Destination-chain (BSC) tx hash, populated when LiFi reports the
		// destination leg completing.
		field.String("bridge_tx_bsc").Optional(),

		field.Enum("bridge_status").
			Values("pending", "bridging", "bridged", "dispatching", "settled", "failed").
			Default("pending"),

		// Set for mode=lp once we re-enter the EVM Gateway on BSC. This is
		// the bytes32 orderId returned by the BSC Gateway's createOrder call.
		field.String("bsc_order_id").Optional(),

		// Set for mode=treasury once the BaaS partner confirms fiat payout.
		field.String("treasury_payout_ref").Optional(),

		// Actual amount of BSC USDC received post-bridge (after slippage + LiFi fees).
		// Used to compare against the expected ceiling rate.
		field.Float("bridged_amount").
			GoType(decimal.Decimal{}).
			Optional(),

		// Non-nil if bridge_status == "failed"; human-readable cause.
		field.String("failure_reason").Optional(),
	}
}

func (RouteAOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("payment_order", PaymentOrder.Type).
			Ref("route_a_order").
			Unique().
			Required(),
	}
}
