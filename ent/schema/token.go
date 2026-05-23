package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Token holds the schema definition for the Token entity.
type Token struct {
	ent.Schema
}

// Mixin of the Token.
func (Token) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

// Fields of the Token.
func (Token) Fields() []ent.Field {
	return []ent.Field{
		field.String("symbol").MaxLen((10)),
		// Holds either an EVM hex address (42 chars) or a Sui Move
		// coin-type string (`0x<64>::<module>::<Type>`, often 80+
		// chars). 200 leaves room for long module/type names.
		field.String("contract_address").MaxLen(200),
		field.Int8("decimals"),
		field.Bool("is_enabled").Default(false),
	}
}

// Edges of the Token.
func (Token) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("network", Network.Type).
			Ref("tokens").
			Required().
			Unique(),
		edge.To("payment_orders", PaymentOrder.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("lock_payment_orders", LockPaymentOrder.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("sender_settings", SenderOrderToken.Type),
	}
}
