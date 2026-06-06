package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/adminauditlog"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
)

// AuditController exposes the append-only admin audit trail for review.
type AuditController struct{}

// NewAuditController constructs the controller.
func NewAuditController() *AuditController { return &AuditController{} }

// GetAuditLogs returns the most recent audit rows, newest first. Optional
// filters: ?action=funding.transfer&target=<id>&limit=100.
//
//	GET /v1/admin/audit-logs
func (c *AuditController) GetAuditLogs(ctx *gin.Context) {
	q := storage.Client.AdminAuditLog.Query().Order(ent.Desc(adminauditlog.FieldCreatedAt))

	if a := strings.TrimSpace(ctx.Query("action")); a != "" {
		q = q.Where(adminauditlog.ActionEQ(a))
	}
	if t := strings.TrimSpace(ctx.Query("target")); t != "" {
		q = q.Where(adminauditlog.TargetEQ(t))
	}

	limit := 100
	if l := ctx.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	rows, err := q.Limit(limit).All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load audit logs", nil)
		return
	}

	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id":         r.ID.String(),
			"actor":      r.Actor,
			"action":     r.Action,
			"target":     r.Target,
			"detail":     r.Detail,
			"created_at": r.CreatedAt.Format(tsLayout),
		})
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"count": len(out),
		"logs":  out,
	})
}
