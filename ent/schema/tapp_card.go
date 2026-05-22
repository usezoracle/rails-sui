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
// Status lifecycle (see docs/tapp-card-spec.md for the full state
// machine):
//
//	issued  → URL minted via /v1/cards/issue-batch, no user yet
//	claimed → user signed in, called /v1/cards/link/claim
//	live    → PWA wrote K to the card sector + funded the on-chain
//	          CardSpendingCap + posted /v1/cards/link/complete
//	revoked → user kill switch (PWA → /v1/cards/revoke → Move set_revoked)
//	locked  → too many failed PIN attempts (5) or token mismatches (3)
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

		// ----- Lifecycle -----

		// Opaque token in the URL written to the card sector.
		// 16 base32 chars ≈ 80 bits entropy. URL is a routing
		// identifier, not a secret — auth is by zkLogin in the PWA.
		field.String("activation_token").
			NotEmpty().
			Unique(),

		field.Enum("status").
			Values("issued", "claimed", "live", "revoked", "locked").
			Default("issued"),

		// ----- Linking-time fields (set on /v1/cards/link/complete) -----

		// sha256 of the NTAG215 factory UID. Same hash the merchant
		// app computes from the read UID; lets us look up the card
		// without ever storing the raw UID.
		field.Bytes("card_uid_hash").
			MaxLen(32).
			Optional().
			Nillable(),

		// Sui object ID of the user's on-chain CardSpendingCap<T>.
		// 0x-prefixed hex string.
		field.String("cap_object_id").
			Optional().
			Nillable(),

		// Move coin type string (e.g. "0x...::usdc::USDC"). Lets the
		// debit PTB builder construct the right type arg without
		// re-querying the chain.
		field.String("coin_type").
			Optional().
			Nillable(),

		// ----- PIN protocol (HMAC challenge-response, see Appendix A) -----

		// linking_proof = HMAC(HMAC(K, PIN), "linking-anchor-v1")
		// computed by the PWA at link time. Server stores it; on
		// debit we verify pin_response = HMAC(linking_proof,
		// server_nonce). PIN and K never live on the server.
		field.Bytes("linking_proof").
			MaxLen(32).
			Optional().
			Nillable(),

		// pin_verifier = HMAC(K, "tapp-card-verifier-v1") — a second
		// HMAC the PWA also commits. Lets the server distinguish a
		// "no PIN set yet" card from one with bad protocol state
		// without needing to know K. Optional in v1; kept for
		// future-proofing.
		field.Bytes("pin_verifier").
			MaxLen(32).
			Optional().
			Nillable(),

		field.Int("pin_attempts_remaining").
			Default(5).
			NonNegative(),

		field.Time("locked_until").
			Optional().
			Nillable(),

		// 4-byte NTAG215 PWD set during PWA linking. Returned to the
		// merchant app in every debit response so it can PWD_AUTH the
		// card before reading K / writing the new rotation token.
		// Stable per card in v1 (rotating PWD requires PWD_PROG, which
		// adds complexity for marginal security gain — the password
		// only protects writes to this one card).
		field.Bytes("card_password").
			MaxLen(4).
			Optional().
			Nillable(),

		// ----- Token rotation (single canonical token; no sliding window) -----

		// AES-GCM ciphertext of the random rotation token. Server
		// generates a new one after every successful debit; merchant
		// app writes the ciphertext to the card sector. Plaintext
		// never leaves the server.
		field.Bytes("current_token_ciphertext").
			Optional().
			Nillable(),

		field.Time("token_rotated_at").
			Optional().
			Nillable(),

		// Count of how many consecutive token-mismatch reads we've
		// seen. >3 in a sliding 1h window → status=locked.
		field.Int("token_mismatch_count").
			Default(0).
			NonNegative(),

		// ----- Cached limits (Move is source of truth, off-chain mirror for fast pre-checks) -----

		field.Uint64("daily_limit_subunit").
			Default(0),
		field.Uint64("per_tap_limit_subunit").
			Default(0),
		field.Uint64("step_up_threshold_subunit").
			Default(0),
		field.Uint64("spent_today_subunit").
			Default(0),
		field.Uint64("day_index").
			Default(0),

		// Flag the cardholder needs to run the PWA resync flow.
		// Flipped to true when a token-ack reports written=false;
		// cleared on /v1/cards/me/resync/complete.
		field.Bool("needs_resync").
			Default(false),
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

		// Per-debit server nonces issued for this card. Cleared by a
		// periodic sweep (TTL on the row).
		edge.To("server_nonces", CardServerNonce.Type),
	}
}

// Indexes of the TappCard.
func (TappCard) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("activation_token"),
		index.Fields("card_uid_hash"),
	}
}
