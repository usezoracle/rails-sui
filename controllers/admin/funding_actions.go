package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/services/baas"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// transferRequest funds a beneficiary from a Safe Haven account (main float by
// default, or an LP sub-account via DebitAccount). MONEY MOVEMENT.
type transferRequest struct {
	DebitAccount        string `json:"debit_account"` // default: SAFEHAVEN_DEBIT_ACCOUNT_NUMBER
	BeneficiaryBankCode string `json:"beneficiary_bank_code" binding:"required"`
	BeneficiaryAccount  string `json:"beneficiary_account" binding:"required"`
	Amount              string `json:"amount" binding:"required"`
	Narration           string `json:"narration"`
	Reference           string `json:"reference" binding:"required"` // idempotency key (operator-supplied)
	Confirm             bool   `json:"confirm"`                      // false → dry-run (no money moves)
}

// Transfer moves NGN out of a Safe Haven account to a beneficiary. SAFE BY
// DEFAULT: with confirm=false it only name-enquiries and returns the plan; with
// confirm=true it executes the transfer (idempotent on the supplied reference)
// and audits it.
//
//	POST /v1/admin/funding/transfer
func (c *FundingController) Transfer(ctx *gin.Context) {
	var body transferRequest
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "missing required fields", nil)
		return
	}
	amount, err := decimal.NewFromString(body.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "amount must be a positive number", nil)
		return
	}

	shConf := config.SafehavenConfig()
	// Guard against fat-finger / stolen-token catastrophe.
	if shConf.MaxTransferNGN.IsPositive() && amount.GreaterThan(shConf.MaxTransferNGN) {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"amount "+amount.String()+" exceeds max single transfer "+shConf.MaxTransferNGN.String()+" (raise SAFEHAVEN_MAX_TRANSFER_NGN)", nil)
		return
	}

	client := baas.Default()
	if client == nil {
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "baas rail not configured", nil)
		return
	}

	debit := body.DebitAccount
	if debit == "" {
		debit = shConf.DebitAccountNumber
	}
	if debit == "" {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "no debit_account and SAFEHAVEN_DEBIT_ACCOUNT_NUMBER unset", nil)
		return
	}

	// Always resolve the beneficiary name first (read-only) — also the
	// idempotency-safe gate Safe Haven requires before a transfer.
	enq, err := client.NameEnquiry(ctx, body.BeneficiaryBankCode, body.BeneficiaryAccount)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadGateway, "error", "name enquiry failed: "+err.Error(), nil)
		return
	}

	plan := gin.H{
		"debit_account":       debit,
		"beneficiary_account": body.BeneficiaryAccount,
		"beneficiary_name":    enq.AccountName,
		"beneficiary_bank":    body.BeneficiaryBankCode,
		"amount":              amount.String(),
		"reference":           baas.PaymentReference("admin", body.Reference),
	}

	if !body.Confirm {
		u.APIResponse(ctx, http.StatusOK, "success", "dry-run — re-send with confirm=true to execute", plan)
		return
	}

	res, err := client.Transfer(ctx, baas.TransferRequest{
		NameEnquiryReference: enq.Reference,
		DebitAccountNumber:   debit,
		BeneficiaryBankCode:  body.BeneficiaryBankCode,
		BeneficiaryAccount:   body.BeneficiaryAccount,
		Amount:               amount,
		Narration:            body.Narration,
		PaymentReference:     baas.PaymentReference("admin", body.Reference),
		SaveBeneficiary:      false,
	})
	if err != nil {
		logger.Errorf("[admin] funding transfer failed ref=%s: %v", body.Reference, err)
		writeAudit(ctx, "funding.transfer.failed", body.BeneficiaryAccount, map[string]any{"plan": plan, "error": err.Error()})
		u.APIResponse(ctx, http.StatusBadGateway, "error", "transfer failed: "+err.Error(), nil)
		return
	}

	plan["status"] = res.RawStatus
	plan["session_id"] = res.Reference
	writeAudit(ctx, "funding.transfer", body.BeneficiaryAccount, plan)
	u.APIResponse(ctx, http.StatusOK, "success", "transfer submitted", plan)
}
