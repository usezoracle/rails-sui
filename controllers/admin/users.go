package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// UsersController manages user administration operations.
type UsersController struct{}

// NewUsersController constructs the controller.
func NewUsersController() *UsersController { return &UsersController{} }

// GetUsers lists all users.
//
//	GET /v1/admin/users
func (c *UsersController) GetUsers(ctx *gin.Context) {
	rows, err := storage.Client.User.Query().All(ctx)
	if err != nil {
		logger.Errorf("admin GetUsers: %v", err)
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
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", out)
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
