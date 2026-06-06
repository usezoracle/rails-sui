package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/identityverificationrequest"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/refreshtoken"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// GetUser returns a single user with profile presence, KYC status, and order
// count.
//
//	GET /v1/admin/users/:id
func (c *UsersController) GetUser(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	user, err := storage.Client.User.Query().
		Where(userEnt.IDEQ(id)).
		WithSenderProfile().
		WithProviderProfile().
		WithTappCards().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "user not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load user", nil)
		return
	}

	kyc := "not_started"
	if ivr, e := storage.Client.IdentityVerificationRequest.Query().
		Where(identityverificationrequest.WalletAddressEQ(id.String())).Only(ctx); e == nil && ivr != nil {
		kyc = ivr.Status.String()
	}

	orderCount := 0
	if user.Edges.SenderProfile != nil {
		orderCount, _ = storage.Client.PaymentOrder.Query().
			Where(paymentorder.HasSenderProfileWith(senderprofile.HasUserWith(userEnt.IDEQ(id)))).
			Count(ctx)
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"id":                user.ID.String(),
		"email":             user.Email,
		"first_name":        user.FirstName,
		"last_name":         user.LastName,
		"scope":             user.Scope,
		"is_email_verified": user.IsEmailVerified,
		"has_early_access":  user.HasEarlyAccess,
		"created_at":        user.CreatedAt.Format(tsLayout),
		"has_sender":        user.Edges.SenderProfile != nil,
		"has_provider":      user.Edges.ProviderProfile != nil,
		"tapp_cards":        len(user.Edges.TappCards),
		"kyc_status":        kyc,
		"order_count":       orderCount,
	})
}

type userPatch struct {
	Scope           *string `json:"scope"`
	IsEmailVerified *bool   `json:"is_email_verified"`
	HasEarlyAccess  *bool   `json:"has_early_access"`
}

// UpdateUser sets a user's scope, email-verified flag, and/or early access.
//
//	PATCH /v1/admin/users/:id
func (c *UsersController) UpdateUser(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	var body userPatch
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "invalid body", nil)
		return
	}
	upd := storage.Client.User.UpdateOneID(id)
	detail := map[string]any{}
	if body.Scope != nil {
		s := strings.TrimSpace(*body.Scope)
		upd.SetScope(s)
		detail["scope"] = s
	}
	if body.IsEmailVerified != nil {
		upd.SetIsEmailVerified(*body.IsEmailVerified)
		detail["is_email_verified"] = *body.IsEmailVerified
	}
	if body.HasEarlyAccess != nil {
		upd.SetHasEarlyAccess(*body.HasEarlyAccess)
		detail["has_early_access"] = *body.HasEarlyAccess
	}
	if len(detail) == 0 {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "nothing to update", nil)
		return
	}
	if _, err := upd.Save(ctx); err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "user not found", nil)
			return
		}
		logger.Errorf("admin UpdateUser %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update user", nil)
		return
	}
	writeAudit(ctx, "user.update", id.String(), detail)
	u.APIResponse(ctx, http.StatusOK, "success", "user updated", detail)
}

// RevokeSessions revokes all of a user's active refresh tokens — forcing them to
// re-authenticate everywhere. The closest thing to a "suspend" without a schema
// change; a hard block-login suspend needs an is_active column (a migration).
//
//	POST /v1/admin/users/:id/revoke-sessions
func (c *UsersController) RevokeSessions(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	n, err := storage.Client.RefreshToken.Update().
		Where(
			refreshtoken.HasOwnerWith(userEnt.IDEQ(id)),
			refreshtoken.RevokedAtIsNil(),
		).
		SetRevokedAt(time.Now()).
		Save(ctx)
	if err != nil {
		logger.Errorf("admin RevokeSessions %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to revoke sessions", nil)
		return
	}
	detail := map[string]any{"revoked": n}
	writeAudit(ctx, "user.revoke_sessions", id.String(), detail)
	u.APIResponse(ctx, http.StatusOK, "success", "sessions revoked", detail)
}
