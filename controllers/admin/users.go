package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// UsersController manages user administration operations.
type UsersController struct{}

// NewUsersController constructs the controller.
func NewUsersController() *UsersController { return &UsersController{} }

// GetUsers lists users, newest first, paginated. Optional filters:
// ?scope=sender|provider|cardholder, ?search= (email or name, case-insensitive),
// ?verified=true|false, ?early_access=true|false. Page via ?page=&pageSize=.
//
//	GET /v1/admin/users
func (c *UsersController) GetUsers(ctx *gin.Context) {
	page, offset, limit := u.Paginate(ctx)

	q := storage.Client.User.Query()
	if s := strings.TrimSpace(ctx.Query("scope")); s != "" {
		q = q.Where(userEnt.ScopeEQ(s))
	}
	if s := strings.TrimSpace(ctx.Query("search")); s != "" {
		q = q.Where(userEnt.Or(
			userEnt.EmailContainsFold(s),
			userEnt.FirstNameContainsFold(s),
			userEnt.LastNameContainsFold(s),
		))
	}
	switch ctx.Query("verified") {
	case "true":
		q = q.Where(userEnt.IsEmailVerifiedEQ(true))
	case "false":
		q = q.Where(userEnt.IsEmailVerifiedEQ(false))
	}
	switch ctx.Query("early_access") {
	case "true":
		q = q.Where(userEnt.HasEarlyAccessEQ(true))
	case "false":
		q = q.Where(userEnt.HasEarlyAccessEQ(false))
	}

	total, err := q.Clone().Count(ctx)
	if err != nil {
		logger.Errorf("admin GetUsers: count: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to count users", nil)
		return
	}
	rows, err := q.Order(ent.Desc(userEnt.FieldCreatedAt)).Offset(offset).Limit(limit).All(ctx)
	if err != nil {
		logger.Errorf("admin GetUsers: query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load users", nil)
		return
	}

	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id":                r.ID.String(),
			"email":             r.Email,
			"first_name":        r.FirstName,
			"last_name":         r.LastName,
			"scope":             r.Scope,
			"is_email_verified": r.IsEmailVerified,
			"has_early_access":  r.HasEarlyAccess,
			"created_at":        r.CreatedAt.Format(tsLayout),
		})
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"total": total,
		"page":  page,
		"count": len(out),
		"users": out,
	})
}

type earlyAccessPatch struct {
	HasEarlyAccess *bool `json:"has_early_access" binding:"required"`
}

// UpdateEarlyAccess sets a user's early access status.
//
//	PATCH /v1/admin/users/:id/early-access
func (c *UsersController) UpdateEarlyAccess(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	var body earlyAccessPatch
	if err := ctx.ShouldBindJSON(&body); err != nil || body.HasEarlyAccess == nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "has_early_access field is required", nil)
		return
	}

	user, err := storage.Client.User.UpdateOneID(id).SetHasEarlyAccess(*body.HasEarlyAccess).Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "user not found", nil)
			return
		}
		logger.Errorf("admin UpdateEarlyAccess %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update user early access", nil)
		return
	}

	detail := map[string]any{"has_early_access": *body.HasEarlyAccess}
	writeAudit(ctx, "user.early_access.update", user.Email, detail)
	u.APIResponse(ctx, http.StatusOK, "success", "user early access updated", detail)
}
