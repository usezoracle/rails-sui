package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TappCard is a programmable NFC card (typically NTAG215) that, once
// linked to a user's zkLogin wallet via the Zoracle PWA, can pay a
// Tapp Merchant on tap.
//
// This is the **PoC-shippable subset** — just enough to mint opaque
// activation URLs, track claim state, and route the /c/:token
// redirect for the NFC-Tools hand-test. The post-PoC fields
// (card_uid_hash, cap_object_id, pin_verifier, current_token_ct,
// daily limits, …) land on this same table in a follow-up migration
// when the full Move + PWA pipeline is greenlit. See
// docs/tapp-card-spec.md for the eventual full schema.
type TappCard struct {
	ent.Schema
}

// Mixin of the TappCard.
func (TappCard) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

// Fields of the TappCard.
func (TappCard) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New),

		// Opaque token in the URL written to the card sector.
		// 16 base32 chars ≈ 80 bits entropy — fine for PoC; the URL
		// itself is not a secret, just a routing identifier (auth is
		// still by zkLogin on the PWA).
		field.String("activation_token").
			NotEmpty().
			Unique(),

		// issued  → URL minted, card not yet claimed by any user
		// claimed → user signed in via zkLogin and claimed this card
		// live    → full link complete: K written, cap_object_id set
		// revoked → user kill switch
		// locked  → too many failed PIN attempts; needs admin recovery
		field.Enum("status").
			Values("issued", "claimed", "live", "revoked", "locked").
			Default("issued"),
	}
}

// Edges of the TappCard.
func (TappCard) Edges() []ent.Edge {
	return []ent.Edge{
		// Nullable: an issued-but-unclaimed card has no owner yet.
		// Once claimed, the edge is set and never changes.
		edge.From("user", User.Type).
			Ref("tapp_cards").
			Unique(),
	}
}

// Indexes of the TappCard.
func (TappCard) Indexes() []ent.Index {
	return []ent.Index{
		// Fast lookup by activation_token for the /c/:token route.
		index.Fields("activation_token"),
	}
}
