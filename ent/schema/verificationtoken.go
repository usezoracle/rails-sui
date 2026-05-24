package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// VerificationToken stores email-verification and password-reset codes.
//
// Security model:
//   - The `token` column is the SHA-256 hash of the raw token sent to
//     the user via email (see utils/token.GenerateOpaqueToken /
//     HashToken). The raw token is never stored.
//   - Lookups MUST hash the submitted token before WHERE token = ?
//     — otherwise authentication breaks.
//   - Rows are single-use: delete after a successful verify.
//
// The previous design generated tokens as `md5(bcrypt(email))` inside
// an ent hook and stored them in plaintext. That was deterministic per
// user (same email → same bcrypt salt produced the same hash chain in
// practice once a token was issued) AND DB-readable. Replaced.
type VerificationToken struct {
	ent.Schema
}

func (VerificationToken) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (VerificationToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		// SHA-256 hash of the raw token. 64 hex chars.
		field.String("token").Immutable(),
		field.Enum("scope").Values("emailVerification", "resetPassword"),
		field.Time("expiry_at").Immutable().Default(time.Now().Add(time.Hour * 9000)),
	}
}

func (VerificationToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("owner", User.Type).
			Ref("verification_token").
			Unique().
			Required().
			Immutable(),
	}
}
