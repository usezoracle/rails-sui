package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lpaccount"
	"github.com/usezoracle/rails-sui/ent/lpledgerentry"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// LpOpsController gives operators visibility and control over Route B
// liquidity providers: list accounts with balances, inspect a ledger,
// and suspend/reactivate. All writes audited.
type LpOpsController struct{}

// NewLpOpsController constructs the controller.
func NewLpOpsController() *LpOpsController { return &LpOpsController{} }

func lpView(a *ent.LpAccount) gin.H {
	return gin.H{
		"id":                a.ID.String(),
		"name":              a.Name,
		"email":             a.Email,
		"bvn_last4":         a.BvnLast4,
		"status":            a.Status.String(),
		"balance":           a.Balance.String(),
		"deposit_account":   a.AccountNumber,
		"deposit_bank":      a.BankName,
		"account_reference": a.AccountReference,
		"created_at":        a.CreatedAt.Format(tsLayout),
	}
}

func ledgerView(e *ent.LpLedgerEntry) gin.H {
	return gin.H{
		"id":           e.ID.String(),
		"type":         e.EntryType.String(),
		"amount":       e.Amount.String(),
		"currency":     e.Currency,
		"status":       e.Status.String(),
		"provider_ref": e.ProviderRef,
		"raw_status":   e.RawStatus,
		"note":         e.Note,
		"at":           e.CreatedAt.Format(tsLayout),
	}
}

// GetLPs lists LP accounts, newest first. ?search= matches name/email;
// ?status=active|suspended filters.
//
//	GET /v1/admin/lps
func (c *LpOpsController) GetLPs(ctx *gin.Context) {
	page, offset, limit := u.Paginate(ctx)
	q := storage.Client.LpAccount.Query()
	if s := strings.TrimSpace(ctx.Query("search")); s != "" {
		q = q.Where(lpaccount.Or(
			lpaccount.NameContainsFold(s),
			lpaccount.EmailContainsFold(s),
			lpaccount.AccountNumberEQ(s),
		))
	}
	if s := strings.TrimSpace(ctx.Query("status")); s != "" {
		q = q.Where(lpaccount.StatusEQ(lpaccount.Status(s)))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		logger.Errorf("admin GetLPs count: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to count LPs", nil)
		return
	}
	rows, err := q.Order(ent.Desc(lpaccount.FieldCreatedAt)).Offset(offset).Limit(limit).All(ctx)
	if err != nil {
		logger.Errorf("admin GetLPs query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load LPs", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, a := range rows {
		out = append(out, lpView(a))
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"total": total, "page": page, "count": len(out), "lps": out,
	})
}

// GetLP returns one LP with their recent ledger activity.
//
//	GET /v1/admin/lps/:id
func (c *LpOpsController) GetLP(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	acct, err := storage.Client.LpAccount.Query().Where(lpaccount.IDEQ(id)).Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error", "LP not found", nil)
		return
	}
	entries, err := acct.QueryLedgerEntries().
		Order(ent.Desc(lpledgerentry.FieldCreatedAt)).
		Limit(100).
		All(ctx)
	if err != nil {
		logger.Errorf("admin GetLP ledger %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load ledger", nil)
		return
	}
	ledger := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		ledger = append(ledger, ledgerView(e))
	}
	view := lpView(acct)
	view["ledger"] = ledger
	u.APIResponse(ctx, http.StatusOK, "success", "ok", view)
}

type lpStatusReq struct {
	Status string `json:"status" binding:"required"` // active | suspended
	Reason string `json:"reason"`
}

// SetStatus suspends or reactivates an LP. Suspension blocks nothing
// retroactively (in-flight withdrawals finish via webhooks) but the
// LP surface refuses new withdrawals for suspended accounts.
//
//	POST /v1/admin/lps/:id/status
func (c *LpOpsController) SetStatus(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	var body lpStatusReq
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "status is required", nil)
		return
	}
	target := strings.ToLower(strings.TrimSpace(body.Status))
	if target != "active" && target != "suspended" {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "status must be active|suspended", nil)
		return
	}
	acct, err := storage.Client.LpAccount.Query().Where(lpaccount.IDEQ(id)).Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error", "LP not found", nil)
		return
	}
	prev := acct.Status.String()
	updated, err := acct.Update().SetStatus(lpaccount.Status(target)).Save(ctx)
	if err != nil {
		logger.Errorf("admin LP status %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update LP", nil)
		return
	}
	writeAudit(ctx, "lp.status", id.String(), map[string]any{
		"from": prev, "to": target, "reason": body.Reason,
	})
	u.APIResponse(ctx, http.StatusOK, "success", "LP "+target, lpView(updated))
}
