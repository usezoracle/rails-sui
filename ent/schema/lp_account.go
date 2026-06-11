package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LpAccount is a liquidity provider's float account on the platform
// (Route B). The NGN custody rail is pooled (Korapay virtual accounts
// settle into one merchant balance), so THIS row + LpLedgerEntry are
// the source of truth for the LP's balance — the bank rail only
// attributes deposits via account_reference / account_number on
// charge.success webhooks.
//
// Balance discipline: `balance` is the cached spendable amount,
// mutated ONLY inside the same transaction that inserts the ledger
// entry justifying the change (deposits credit on confirmed webhook;
// withdrawals reserve on request, release on transfer.failed). The
// ledger is replayable truth; the column is the fast read.
type LpAccount struct {
	ent.Schema
}

func (LpAccount) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (LpAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		field.String("name"),
		field.String("email"),
		// Never store the raw BVN — last4 for support lookups only.
		field.String("bvn_last4").MaxLen(4),

		// Korapay virtual-account identity. account_reference is OUR
		// attribution key (we generate it, Korapay echoes it on every
		// deposit webhook); account_number is what the LP wires money to.
		field.String("account_reference").Unique(),
		field.String("account_number"),
		field.String("bank_name"),
		field.String("bank_code"),

		field.Enum("status").
			Values("active", "suspended").
			Default("active"),

		// Where Route B fills deliver the LP's USDC (Sui address).
		// Required before the LP is matchable; set from the LP
		// dashboard.
		field.String("sui_usdc_address").Optional(),

		// Spendable NGN. See balance discipline above. No schema
		// default (ent can't default a decimal GoType) — Onboard sets
		// it to zero explicitly.
		field.Float("balance").
			GoType(decimal.Decimal{}),
	}
}

func (LpAccount) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("lp_account").
			Unique().
			Required(),
		edge.To("ledger_entries", LpLedgerEntry.Type),
	}
}
