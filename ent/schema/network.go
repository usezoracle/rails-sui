package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/shopspring/decimal"
)

// Network holds the schema definition for the Network entity.
//
// Sui-only after the port. Multi-chain fields (chain_id_hex,
// gateway_contract_address) were dropped — Sui doesn't use an EVM-style chain
// id hex, and Gateway addressing on Sui is config-driven (SuiGatewayPackageID
// + SuiGatewayObjectID in OrderConfig) rather than per-network on this entity.
type Network struct {
	ent.Schema
}

func (Network) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (Network) Fields() []ent.Field {
	return []ent.Field{
		// Sui chainType=MVM. Sui mainnet's chain id is 9270000000000000.
		field.Int64("chain_id"),
		// Stable network key, e.g. "sui-mainnet" or "sui-testnet".
		field.String("identifier").Unique(),
		// Sui fullnode JSON-RPC endpoint, e.g. https://fullnode.mainnet.sui.io:443
		field.String("rpc_endpoint"),
		field.Bool("is_testnet"),
		// Per-tx protocol fee on this network, in basis points equivalent.
		field.Float("fee").GoType(decimal.Decimal{}),
	}
}

func (Network) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("tokens", Token.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}
