package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// MerchantBankAccount stores the bank account a Tapp Merchant has bound
// to their SenderProfile. Every tap-to-pay PaymentOrder this merchant
// creates auto-populates `recipient` from here — the mobile client
// never has to re-enter bank details on each tap.
//
// One-to-one with SenderProfile (each merchant has at most one payout
// account in v1 — NGN only). Adding more rows / multi-currency support
// is a v2 concern.
type MerchantBankAccount struct {
	ent.Schema
}

// Mixin of the MerchantBankAccount.
func (MerchantBankAccount) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

// Fields of the MerchantBankAccount.
func (MerchantBankAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New),
		// ISO 4217 currency code. v1 = "NGN" only — kept on the row so
		// future multi-currency expansion doesn't need a migration.
		field.String("currency").
			MaxLen(3).
			NotEmpty(),
		// Institution code from the Institution table (e.g. NGN bank
		// CBN code). Not a FK to keep this table independent of the
		// catalog churn — code-string match at write time.
		field.String("bank_code").
			NotEmpty(),
		field.String("account_number").
			NotEmpty(),
		field.String("account_name").
			NotEmpty(),
		// Set when the BaaS / name-resolve API returned a matching name.
		// Nil means the account passed local validation but hasn't been
		// re-verified — controllers gate live tap-to-pay on this.
		field.Time("verified_at").
			Optional().
			Nillable(),
	}
}

// Edges of the MerchantBankAccount.
func (MerchantBankAccount) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("sender_profile", SenderProfile.Type).
			Ref("merchant_bank_account").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the MerchantBankAccount.
func (MerchantBankAccount) Indexes() []ent.Index {
	return []ent.Index{
		// At most one bank account per sender — the back side of the
		// 1:1 relationship is enforced here at the DB level too.
		index.Edges("sender_profile").Unique(),
	}
}
