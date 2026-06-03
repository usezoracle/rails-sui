package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// RefundController exposes operator refund actions on payment orders.
type RefundController struct{}

// NewRefundController constructs the controller.
func NewRefundController() *RefundController { return &RefundController{} }

type refundBody struct {
	Justification string `json:"justification" binding:"required"`
	Confirm       bool   `json:"confirm"`
}

// RefundOrder marks a payment order refunded — a reconciliation state change
// recorded after the funds have been returned out-of-band (on-chain refund or a
// Safe Haven transfer via /funding/transfer). Gated: confirm + justification,
// audited, idempotent. It does NOT move funds itself.
//
//	POST /v1/admin/orders/:id/refund
func (c *RefundController) RefundOrder(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "order id must be a uuid", nil)
		return
	}
	var body refundBody
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "justification is required", nil)
		return
	}

	o, err := storage.Client.PaymentOrder.Query().Where(paymentorder.IDEQ(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "payment order not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load order", nil)
		return
	}

	if o.Status == paymentorder.StatusRefunded {
		u.APIResponse(ctx, http.StatusOK, "success", "already refunded", gin.H{"order_id": id.String()})
		return
	}
	if !body.Confirm {
		u.APIResponse(ctx, http.StatusOK, "success", "dry-run — re-send with confirm=true to mark refunded", gin.H{
			"order_id": id.String(), "current_status": string(o.Status), "will_set": "refunded",
		})
		return
	}

	prev := string(o.Status)
	if _, err := o.Update().SetStatus(paymentorder.StatusRefunded).Save(ctx); err != nil {
		logger.Errorf("admin RefundOrder %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update order", nil)
		return
	}

	detail := map[string]any{"previous_status": prev, "justification": body.Justification}
	writeAudit(ctx, "order.refund", id.String(), detail)
	u.APIResponse(ctx, http.StatusOK, "success", "order marked refunded", gin.H{
		"order_id": id.String(), "previous": prev, "current": "refunded", "justification": body.Justification,
	})
}
