package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// AdminAuditLog records every admin write action — config changes, funding
// money-movement, refunds — so operator actions are attributable and reviewable.
// Append-only: rows are immutable once written.
type AdminAuditLog struct {
	ent.Schema
}

// Fields of the AdminAuditLog.
func (AdminAuditLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		// actor: the operator identity (request IP for now; becomes a user id
		// once RBAC lands).
		field.String("actor").Immutable(),
		// action: dotted verb, e.g. "config.currency.update", "funding.transfer",
		// "order.refund".
		field.String("action").Immutable(),
		// target: the affected entity id (order id, account number, currency id…).
		field.String("target").Optional().Immutable(),
		// detail: structured before/after + justification.
		field.JSON("detail", map[string]interface{}{}).Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Edges of the AdminAuditLog.
func (AdminAuditLog) Edges() []ent.Edge { return nil }
