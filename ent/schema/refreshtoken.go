// RefreshToken — server-side refresh tokens with rotation + family
// tracking + replay detection.
//
// Design (industry standard, see RFC 6749 §10.4, OWASP refresh rotation):
//
//   1. Issue:  raw = crypto/rand(32). DB stores SHA-256(raw); client
//      keeps the raw. token_hash is unique. family_id ties together
//      tokens generated from a single login chain.
//
//   2. Refresh: client presents raw refresh → hash + lookup. On success
//      we issue a NEW raw refresh in the same family, revoke the old
//      one, and link parent → child via replaced_by_id. Old refresh is
//      now invalid.
//
//   3. Replay detection: if a refresh token comes in whose row is
//      already revoked, that's a replay — someone else got a copy of an
//      old refresh and is using it. Revoke the entire family so neither
//      the attacker nor the (now-distinct) victim's chain stays alive.
//      User must re-login.
//
//   4. Logout: revoke the presented token's family. Idempotent — no
//      info leak about validity.
//
// Why the access token stays a JWT: short TTL (15min) + stateless
// validation means no DB hit per request. Refresh tokens need state
// (revocation, rotation chain) so they're opaque random + indexed.

package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type RefreshToken struct {
	ent.Schema
}

func (RefreshToken) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (RefreshToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),

		// SHA-256(raw) hex. Unique so duplicate raws (cryptographically
		// improbable) still surface as a constraint violation rather
		// than silent overwrite.
		field.String("token_hash").Immutable().Unique(),

		// Groups all tokens issued from one login (login → refresh →
		// refresh → … all share family_id). Replay of any revoked
		// token in the family revokes the whole family.
		field.UUID("family_id", uuid.UUID{}).Immutable(),

		// Previous token this one replaced. Null for the first token
		// in a family (the one issued at /auth/login). Lets us walk
		// the rotation chain backwards for audit/debug.
		field.UUID("parent_id", uuid.UUID{}).Optional().Nillable().Immutable(),

		// The token that REPLACED this one (rotation pointer). Set
		// when this token is consumed by /auth/refresh.
		field.UUID("replaced_by_id", uuid.UUID{}).Optional().Nillable(),

		// Expiry. Independent of the access token's exp.
		field.Time("expires_at").Immutable(),

		// Set when the token is consumed (rotated) or revoked (logout,
		// family compromise). A non-nil revoked_at means the token
		// MUST NOT be accepted again.
		field.Time("revoked_at").Optional().Nillable(),

		// Optional context — useful for an "active sessions" UI later.
		// Not used for security decisions.
		field.String("user_agent").Optional().MaxLen(255),
		field.String("ip_address").Optional().MaxLen(45),
	}
}

func (RefreshToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("owner", User.Type).
			Ref("refresh_tokens").
			Unique().
			Required().
			Immutable(),
	}
}

func (RefreshToken) Indexes() []ent.Index {
	return []ent.Index{
		// Fast family lookups for replay-revoke + logout.
		index.Fields("family_id"),
		// Filtering active tokens per user.
		index.Fields("revoked_at"),
	}
}
