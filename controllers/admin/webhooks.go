package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	fastshot "github.com/opus-domini/fast-shot"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/webhookretryattempt"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// WebhooksController lets operators inspect the merchant-webhook retry queue and
// force an immediate delivery instead of waiting for the hourly cron.
type WebhooksController struct{}

// NewWebhooksController constructs the controller.
func NewWebhooksController() *WebhooksController { return &WebhooksController{} }

// GetWebhookAttempts lists retry attempts, newest first. Optional ?status=
// (failed|success|expired) and ?limit= (default 100, max 500).
//
//	GET /v1/admin/webhooks
func (c *WebhooksController) GetWebhookAttempts(ctx *gin.Context) {
	q := storage.Client.WebhookRetryAttempt.Query().Order(ent.Desc(webhookretryattempt.FieldNextRetryTime))
	if s := strings.TrimSpace(ctx.Query("status")); s != "" {
		q = q.Where(webhookretryattempt.StatusEQ(webhookretryattempt.Status(s)))
	}
	_, _, limit := u.Paginate(ctx)
	if limit > 500 {
		limit = 500
	}
	rows, err := q.Limit(limit).All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load webhook attempts", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id":              r.ID,
			"webhook_url":     r.WebhookURL,
			"status":          r.Status.String(),
			"attempt_number":  r.AttemptNumber,
			"next_retry_time": r.NextRetryTime.Format(tsLayout),
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{"count": len(out), "attempts": out})
}

// RetryWebhook delivers a single webhook attempt right now. On a 2xx the row is
// marked success; otherwise it's left for the cron (next_retry_time pulled
// forward so the cron picks it up on its next tick). Audited.
//
//	POST /v1/admin/webhooks/:id/retry
func (c *WebhooksController) RetryWebhook(ctx *gin.Context) {
	id, err := strconv.Atoi(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be an integer", nil)
		return
	}
	attempt, err := storage.Client.WebhookRetryAttempt.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "webhook attempt not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load attempt", nil)
		return
	}

	resp, err := fastshot.NewClient(attempt.WebhookURL).
		Config().SetTimeout(30*time.Second).
		Header().Add("X-Rails-Signature", attempt.Signature).
		Build().POST("").
		Body().AsJSON(attempt.Payload).
		Send()

	delivered := err == nil && resp.StatusCode() < 205
	detail := map[string]any{"webhook_url": attempt.WebhookURL, "delivered": delivered}
	if err != nil {
		detail["error"] = err.Error()
	} else {
		detail["status_code"] = resp.StatusCode()
	}

	upd := attempt.Update().AddAttemptNumber(1)
	if delivered {
		upd = upd.SetStatus(webhookretryattempt.StatusSuccess)
	} else {
		// Pull the next scheduled retry forward so the cron re-attempts soon.
		upd = upd.SetStatus(webhookretryattempt.StatusFailed).SetNextRetryTime(time.Now())
	}
	if _, e := upd.Save(ctx); e != nil {
		logger.Errorf("admin webhook retry %d: persist: %v", id, e)
	}

	writeAudit(ctx, "webhook.retry", strconv.Itoa(id), detail)
	if delivered {
		u.APIResponse(ctx, http.StatusOK, "success", "webhook delivered", detail)
		return
	}
	u.APIResponse(ctx, http.StatusBadGateway, "error", "webhook delivery failed — re-queued", detail)
}
