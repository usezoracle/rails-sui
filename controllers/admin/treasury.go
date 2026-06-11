package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/services/baas/korapay"
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

// -----------------------------------------------------------------------------
// Platform float account (Route C) — provisioned at Korapay, no env/DB
// -----------------------------------------------------------------------------

func treasuryKoraClient() *korapay.Client {
	bc := config.BaaSConfig()
	if bc.KorapaySecretKey == "" {
		return nil
	}
	return korapay.New(bc.KorapaySecretKey, bc.KorapayPublicKey, bc.KorapayBaseURL,
		bc.KorapayPayoutEmail, bc.KorapayVBABankCode)
}

// GetFloatAccount returns the platform's float virtual account (the
// Route C reload destination) plus the live Korapay NGN balance.
//
//	GET /v1/admin/treasury/float-account
func (c *TreasuryController) GetFloatAccount(ctx *gin.Context) {
	kc := treasuryKoraClient()
	if kc == nil {
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "Korapay not configured", nil)
		return
	}
	out := gin.H{"provisioned": false, "reference": services.PlatformFloatRef}
	if va, err := kc.GetVirtualAccount(ctx, services.PlatformFloatRef); err == nil {
		out["provisioned"] = true
		out["account_number"] = va.AccountNumber
		out["account_name"] = va.AccountName
		out["bank_name"] = va.BankName
		out["bank_code"] = va.BankCode
		out["status"] = va.AccountStatus
	}
	if balances, err := kc.Balances(ctx); err == nil {
		if ngn, ok := balances["NGN"]; ok {
			out["float_balance"] = ngn.AvailableBalance.String()
			out["pending_balance"] = ngn.PendingBalance.String()
		}
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", out)
}

type provisionFloatReq struct {
	BVN         string `json:"bvn" binding:"required,len=11,numeric"`
	AccountName string `json:"account_name" binding:"required"`
}

// ProvisionFloatAccount creates the platform's float VBA at Korapay —
// a one-time, audited action (BVN is required by the rail and never
// stored by us). Idempotent: returns the existing account if it's
// already provisioned.
//
//	POST /v1/admin/treasury/float-account
func (c *TreasuryController) ProvisionFloatAccount(ctx *gin.Context) {
	kc := treasuryKoraClient()
	if kc == nil {
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "Korapay not configured", nil)
		return
	}
	if va, err := kc.GetVirtualAccount(ctx, services.PlatformFloatRef); err == nil {
		u.APIResponse(ctx, http.StatusOK, "success", "float account already provisioned", gin.H{
			"account_number": va.AccountNumber, "bank_name": va.BankName, "bank_code": va.BankCode,
		})
		return
	}
	var req provisionFloatReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"account_name and an 11-digit bvn are required", u.GetErrorData(err))
		return
	}
	email := config.BaaSConfig().KorapayPayoutEmail
	if email == "" {
		email = "ops@usetapp.xyz"
	}
	va, err := kc.CreateVirtualAccount(ctx, services.PlatformFloatRef, req.AccountName, email, req.BVN)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadGateway, "error",
			"Korapay refused the account", gin.H{"detail": err.Error()})
		return
	}
	writeAudit(ctx, "treasury.float_account.provision", services.PlatformFloatRef, map[string]any{
		"account_number": va.AccountNumber, "bank": va.BankName,
	})
	u.APIResponse(ctx, http.StatusCreated, "success", "float account provisioned", gin.H{
		"account_number": va.AccountNumber, "account_name": va.AccountName,
		"bank_name": va.BankName, "bank_code": va.BankCode,
	})
}
