package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/shopspring/decimal"
)

// RouteAOrder tracks one Route-A bridge order (Sui USDC → LiFi → EVM USDC
// → settlement Gateway → fiat). v1 targets Base (chain ids 8453 mainnet /
// 84532 Sepolia). One row per integrator order that picks route="route_a";
// lives alongside the standard PaymentOrder.
//
// State machine:
//
//	pending      → quote retrieved, awaiting source-chain tx submit
//	bridging     → source tx submitted, polling LiFi /status
//	bridged      → LiFi reports DONE — destination USDC sitting in our hot wallet
//	dispatching  → approve+createOrder submitted, waiting for settlement LP fill
//	settled      → settlement aggregator reports settled (fiat at merchant)
//	failed       → bridge or dispatch failed; reconciliation will refund
//	refunded     → settlement aggregator could not fill within window; USDC bounced to our wallet
type RouteAOrder struct {
	ent.Schema
}

func (RouteAOrder) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (RouteAOrder) Fields() []ent.Field {
	return []ent.Field{
		// Post-bridge target: "lp" (settlement Gateway re-entry) or "treasury"
		// (own BaaS payout). Set by the integrator at order creation.
		field.Enum("mode").
			Values("lp", "treasury").
			Default("treasury"),

		field.String("lifi_quote_id").Optional(),
		field.String("lifi_tool").Optional(),
		// Which bridge rail executed (or is executing) this order's
		// Sui→Base leg. "lifi" = the normal LiFi-orchestrated path;
		// "cctp" = the direct Circle CCTP fallback that kicks in when
		// LiFi can't quote (services/route_a_cctp.go). The dispatcher
		// branches its polling on this field, so existing rows (default
		// "lifi") flow through exactly the code they always did.
		field.String("bridge_provider").Default("lifi"),
		field.String("bridge_tx_sui").Optional(),
		// Destination-chain tx hash where LiFi delivered the bridged USDC.
		// Chain-agnostic: holds a Base tx hash today; if we ever bridge
		// somewhere else, no schema change required.
		field.String("bridge_tx_dest").Optional(),

		// State machine — see docs/route-a-hardening.md Phase 2.
		//
		//   pending          → row exists, awaiting deposit
		//   awaiting_funds   → deposit detected, NOT yet at aggregator (indexer/self-settle in flight)
		//   bridging         → source-chain bridge tx submitted, polling LiFi /status
		//   bridge_uncertain → polled past per-tool timeout without DONE/FAILED;
		//                      late-arrival poller keeps re-checking up to 24h
		//                      AND watches the destination wallet for incoming USDC
		//                      that bypassed LiFi's /status indexer.
		//   bridged          → destination USDC sitting in our hot wallet, ready to dispatch
		//   dispatching      → approve + createOrder submitted, polling settlement
		//   settled          → fiat at merchant
		//   failed           → terminal failure that reconciliation can't recover
		//   refunded         → funds returned to user (manual or auto)
		field.Enum("bridge_status").
			Values(
				"pending",
				"awaiting_funds",
				"bridging",
				"bridge_uncertain",
				"bridged",
				"dispatching",
				"settled",
				"failed",
				"refunded",
			).
			Default("pending"),

		// Set for mode=lp once we re-enter the EVM Gateway. The bytes32
		// orderId returned by the settlement Gateway's createOrder, stored
		// 0x-prefixed hex. Chain identified by gateway_chain_id.
		field.String("gateway_order_id").Optional(),

		// EVM chain id (e.g. 8453 Base mainnet, 84532 Base Sepolia).
		// Needed by the settlement aggregator's /orders/:chainId/:orderId endpoint.
		field.Uint64("gateway_chain_id").Optional(),

		// Sender fee charged by us on top of the order amount, denominated
		// in the destination token's subunit. We collect this to
		// senderFeeRecipient (also our wallet). Persisted for accounting.
		// Nillable so NULL rows scan to *decimal.Decimal(nil) instead of
		// erroring (shopspring/decimal can't Scan NULL).
		field.Float("sender_fee_subunit").
			GoType(decimal.Decimal{}).
			Optional().
			Nillable(),

		// Last status returned by the settlement aggregator's /orders
		// endpoint. Granular (pending/fulfilling/settling/settled/etc.) —
		// used to push live status updates over SSE without bouncing
		// bridge_status on every intermediate state.
		field.String("settlement_status").Optional(),
		field.Time("settlement_polled_at").Optional().Nillable(),

		field.String("treasury_payout_ref").Optional(),

		field.Float("bridged_amount").
			GoType(decimal.Decimal{}).
			Optional().
			Nillable(),

		field.String("failure_reason").Optional(),
	}
}

func (RouteAOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("payment_order", PaymentOrder.Type).
			Ref("route_a_order").
			Unique().
			Required(),
		// Per-order audit log. See ent/schema/route_a_event.go +
		// services/route_a_events.go + docs/route-a-hardening.md.
		edge.To("events", RouteAEvent.Type),
	}
}
