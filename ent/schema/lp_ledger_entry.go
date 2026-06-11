package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// LpLedgerEntry is one movement on an LP's platform balance — the
// replayable audit trail behind LpAccount.balance.
//
//	deposit     — NGN arrived at the LP's virtual account
//	              (charge.success webhook; confirmed on insert)
//	withdrawal  — LP cashing out to their bank
//	              (pending reserves the balance; transfer.success →
//	              confirmed; transfer.failed → failed + re-credit)
//	adjustment  — operator correction (audited elsewhere)
//
// provider_ref is the rail's reference (Korapay deposit reference, or
// our deterministic withdrawal payment reference). UNIQUE — webhook
// redelivery and crash-replays insert-or-skip instead of double-
// crediting. That single constraint is the ledger's idempotency.
type LpLedgerEntry struct {
	ent.Schema
}

func (LpLedgerEntry) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (LpLedgerEntry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New),
		// fill — the LP settled a Route B order: their NGN paid the
		// merchant (debit), and they received the order's USDC on Sui.
		field.Enum("entry_type").
			Values("deposit", "withdrawal", "adjustment", "fill"),
		// Always positive; entry_type carries the direction.
		field.Float("amount").
			GoType(decimal.Decimal{}),
		field.String("currency").Default("NGN"),
		field.String("provider_ref").Unique(),
		field.Enum("status").
			Values("pending", "confirmed", "failed"),
		field.String("raw_status").Optional(),
		// Free-form context: payer account for deposits, beneficiary
		// for withdrawals, operator note for adjustments.
		field.String("note").Optional(),
	}
}

func (LpLedgerEntry) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("lp_account", LpAccount.Type).
			Ref("ledger_entries").
			Unique().
			Required(),
	}
}
