package admin

import (
	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// writeAudit appends an append-only admin audit row and mirrors it to logs.
// Every admin write action (config change, funding move, refund) must call this.
func writeAudit(ctx *gin.Context, action, target string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["remote_addr"] = ctx.ClientIP()
	if _, err := storage.Client.AdminAuditLog.Create().
		SetActor(ctx.ClientIP()).
		SetAction(action).
		SetTarget(target).
		SetDetail(detail).
		Save(ctx); err != nil {
		logger.Errorf("[admin-audit] write failed (%s %s): %v", action, target, err)
	}
	logger.Infof("[admin-audit] actor=%s action=%s target=%s", ctx.ClientIP(), action, target)
}
