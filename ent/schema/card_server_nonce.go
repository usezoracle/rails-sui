package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// CardServerNonce is the per-debit, single-use nonce the merchant app
// fetches via `GET /v1/sender/me/tap-card/nonce` and echoes back in
// the debit POST. Atomic consumption on the debit side (`UPDATE …
// WHERE consumed_at IS NULL RETURNING …`) makes captured PIN responses
// useless — they're bound to exactly one nonce, and that nonce dies on
// first use.
//
// 60s TTL — clients should consume promptly or refetch.
type CardServerNonce struct {
	ent.Schema
}

// Mixin of the CardServerNonce.
func (CardServerNonce) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

// Fields of the CardServerNonce.
func (CardServerNonce) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New),

		// The 32 random bytes the merchant app sees as hex.
		field.Bytes("nonce").
			MaxLen(32).
			Unique(),

		// Tier the server resolved at issuance — the debit handler
		// trusts this rather than re-resolving (defends against amount
		// tampering between /nonce and /tap-card).
		field.Enum("tier").
			Values("none", "pin", "step_up"),

		// Amount in fiat subunit (NGN kobo) the nonce was issued for.
		// Stored alongside the tier so the debit handler can reject
		// amount changes.
		field.String("amount").
			NotEmpty(),

		field.String("currency").
			MaxLen(3).
			Default("NGN"),

		field.Time("expires_at"),

		// Set to non-null at atomic consumption time.
		field.Time("consumed_at").
			Optional().
			Nillable(),
	}
}

// Edges of the CardServerNonce.
func (CardServerNonce) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("card", TappCard.Type).
			Ref("server_nonces").
			Unique().
			Required(),
		// Sender (merchant) the nonce was issued to — locks each
		// nonce to one merchant context.
		edge.From("sender_profile", SenderProfile.Type).
			Ref("card_server_nonces").
			Unique().
			Required(),
	}
}

// Indexes of the CardServerNonce.
func (CardServerNonce) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("nonce"),
	}
}
