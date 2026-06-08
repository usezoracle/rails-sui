package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
)

// TreasuryController gives operators a single consolidated view of where value
// sits: on-chain aggregator wallets, the the BaaS provider NGN float + LP sub-accounts,
// and a DB-side financial summary. Read-only (money movement lives under
// /funding/transfer).
type TreasuryController struct{}

// NewTreasuryController constructs the controller.
func NewTreasuryController() *TreasuryController { return &TreasuryController{} }

// GetOverview returns wallet balances + a financial summary.
//
//	GET /v1/admin/treasury/overview
func (c *TreasuryController) GetOverview(ctx *gin.Context) {
	conf := config.OrderConfig()

	wallets := gin.H{
		"base_aggregator": baseAggregatorBalances(ctx, conf),
		"sui_aggregator":  suiAggregatorBalances(ctx, conf),
		"safehaven":       baasBalances(ctx),
	}

	// DB-side value summary (token units, by order status group).
	settled := sumOrders(ctx, paymentorder.StatusSettled)
	refunded := sumOrders(ctx, paymentorder.StatusRefunded)
	inflight := sumOrders(ctx, paymentorder.StatusInitiated).Add(sumOrders(ctx, paymentorder.StatusPending))

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"wallets": wallets,
		"summary": gin.H{
			"settled_volume":  settled.String(),
			"refunded_total":  refunded.String(),
			"in_flight_value": inflight.String(),
		},
	})
}

// sumOrders sums payment-order amounts for a status (token units).
func sumOrders(ctx *gin.Context, status paymentorder.Status) decimal.Decimal {
	total := decimal.Zero
	rows, err := storage.Client.PaymentOrder.Query().Where(paymentorder.StatusEQ(status)).All(ctx)
	if err != nil {
		return total
	}
	for _, o := range rows {
		total = total.Add(o.Amount)
	}
	return total
}
