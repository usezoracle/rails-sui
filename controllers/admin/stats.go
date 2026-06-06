package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
)

// StatsController exposes a platform-overview dashboard for operators.
type StatsController struct{}

// NewStatsController constructs the controller.
func NewStatsController() *StatsController { return &StatsController{} }

// GetStats returns headline counts + volumes across users, orders, and LPs.
//
//	GET /v1/admin/stats
func (c *StatsController) GetStats(ctx *gin.Context) {
	cl := storage.Client

	users, _ := cl.User.Query().Count(ctx)
	senders, _ := cl.SenderProfile.Query().Count(ctx)
	providers, _ := cl.ProviderProfile.Query().Count(ctx)
	activeProviders, _ := cl.ProviderProfile.Query().Where(providerprofile.IsActiveEQ(true)).Count(ctx)
	kybVerified, _ := cl.ProviderProfile.Query().Where(providerprofile.IsKybVerifiedEQ(true)).Count(ctx)

	// Orders by status + settled volume.
	statuses := []paymentorder.Status{
		paymentorder.StatusInitiated, paymentorder.StatusPending, paymentorder.StatusExpired,
		paymentorder.StatusCancelled, paymentorder.StatusSettled, paymentorder.StatusRefunded,
	}
	byStatus := gin.H{}
	for _, st := range statuses {
		n, _ := cl.PaymentOrder.Query().Where(paymentorder.StatusEQ(st)).Count(ctx)
		byStatus[string(st)] = n
	}
	totalOrders, _ := cl.PaymentOrder.Query().Count(ctx)

	settledVol := decimal.Zero
	if settled, err := cl.PaymentOrder.Query().Where(paymentorder.StatusEQ(paymentorder.StatusSettled)).All(ctx); err == nil {
		for _, o := range settled {
			settledVol = settledVol.Add(o.Amount)
		}
	}

	lockOrders, _ := cl.LockPaymentOrder.Query().Count(ctx)
	lockSettled, _ := cl.LockPaymentOrder.Query().Where(lockpaymentorder.StatusEQ(lockpaymentorder.StatusSettled)).Count(ctx)

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"users":               users,
		"senders":             senders,
		"providers":           providers,
		"active_providers":    activeProviders,
		"kyb_verified":        kybVerified,
		"orders_total":        totalOrders,
		"orders_by_status":    byStatus,
		"settled_volume":      settledVol.String(),
		"lock_orders_total":   lockOrders,
		"lock_orders_settled": lockSettled,
	})
}
