// GET /v1/me — current authenticated user.
//
// Cross-scope endpoint: returns the User row + the scope-derived
// profile presence flags (has_sender_profile, has_provider_profile,
// has_tapp_card). Lets every client (Tapp Merchant app, Tapp PWA,
// any sender/provider integration) call one endpoint after login to
// know who they're signed in as and which sub-resources exist.

package accounts

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent/identityverificationrequest"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	db "github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

type meResponse struct {
	ID              uuid.UUID `json:"id"`
	Email           string    `json:"email"`
	FirstName       string    `json:"first_name"`
	LastName        string    `json:"last_name"`
	Scopes          []string  `json:"scopes"`
	IsEmailVerified bool      `json:"is_email_verified"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

	// Profile presence flags — let clients route to the right
	// dashboard without a second round trip.
	HasSenderProfile   bool `json:"has_sender_profile"`
	HasProviderProfile bool `json:"has_provider_profile"`
	HasTappCard        bool `json:"has_tapp_card"`

	// KYC lifecycle. "not_started" when the user has no IVR row yet;
	// otherwise mirrors the IVR.status enum (pending|success|failed).
	// Clients use this to gate the onboarding guard.
	KycStatus string `json:"kyc_status"`
}

// updateMePayload — PATCH /v1/me body. Only the fields actually present
// are updated; absent fields are left untouched. (Both nil and "" are
// treated as "no change" — we don't allow blanking the name.)
type updateMePayload struct {
	FirstName *string `json:"firstName"`
	LastName  *string `json:"lastName"`
}

// UpdateMe lets the authenticated user edit their own first/last name.
// Email + scope + verification status are not user-editable here —
// changing those requires its own flow (re-verification, role grant,
// etc.).
func (ctrl *AuthController) UpdateMe(ctx *gin.Context) {
	v, exists := ctx.Get("user_id")
	if !exists {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Not authenticated", nil)
		return
	}
	userID, err := uuid.Parse(v.(string))
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid session", nil)
		return
	}

	var payload updateMePayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", err.Error())
		return
	}

	update := db.Client.User.UpdateOneID(userID)
	touched := false
	if payload.FirstName != nil {
		trimmed := strings.TrimSpace(*payload.FirstName)
		if trimmed == "" {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "First name cannot be empty", nil)
			return
		}
		update = update.SetFirstName(trimmed)
		touched = true
	}
	if payload.LastName != nil {
		trimmed := strings.TrimSpace(*payload.LastName)
		if trimmed == "" {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "Last name cannot be empty", nil)
			return
		}
		update = update.SetLastName(trimmed)
		touched = true
	}
	if !touched {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Nothing to update", nil)
		return
	}

	if _, err := update.Save(ctx); err != nil {
		logger.Errorf("UpdateMe: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to update profile", nil)
		return
	}

	// Return the fresh /me payload so the client can replace its cache
	// without an extra round trip.
	ctrl.Me(ctx)
}

// Me returns the authenticated user. Lives behind JWTMiddleware which
// sets `user_id` in the gin context (see routers/middleware/auth.go).
func (ctrl *AuthController) Me(ctx *gin.Context) {
	v, exists := ctx.Get("user_id")
	if !exists {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Not authenticated", nil)
		return
	}
	userID, err := uuid.Parse(v.(string))
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Invalid session", nil)
		return
	}

	user, err := db.Client.User.
		Query().
		Where(userEnt.IDEQ(userID)).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"User not found", nil)
		return
	}

	// Presence queries — Count is faster than loading the edges.
	hasSender, _ := db.Client.SenderProfile.
		Query().
		Where(senderprofile.HasUserWith(userEnt.IDEQ(userID))).
		Exist(ctx)
	hasProvider, _ := db.Client.ProviderProfile.
		Query().
		Where(providerprofile.HasUserWith(userEnt.IDEQ(userID))).
		Exist(ctx)
	hasCard, _ := db.Client.TappCard.
		Query().
		Where(tappcard.HasUserWith(userEnt.IDEQ(userID))).
		Exist(ctx)

	// KYC status. IVR rows are keyed by `wallet_address`, which for the
	// email-flow merchant app is set to the user UUID string by the
	// /v1/kyc handler. Wallet-flow callers do the same with their Sui
	// address. Missing row → "not_started".
	kycStatus := "not_started"
	if ivr, err := db.Client.IdentityVerificationRequest.
		Query().
		Where(identityverificationrequest.WalletAddressEQ(userID.String())).
		Only(ctx); err == nil && ivr != nil {
		kycStatus = ivr.Status.String()
	}

	resp := meResponse{
		ID:                 user.ID,
		Email:              user.Email,
		FirstName:          user.FirstName,
		LastName:           user.LastName,
		Scopes:             splitScopes(user.Scope),
		IsEmailVerified:    user.IsEmailVerified,
		CreatedAt:          user.CreatedAt,
		UpdatedAt:          user.UpdatedAt,
		HasSenderProfile:   hasSender,
		HasProviderProfile: hasProvider,
		HasTappCard:        hasCard,
		KycStatus:          kycStatus,
	}

	logger.Infof("/me: user_id=%s scopes=%v", userID, resp.Scopes)
	u.APIResponse(ctx, http.StatusOK, "success", "OK", resp)
}

// splitScopes turns the stored "sender provider cardholder" string
// into a clean slice. Empty values are dropped — historical rows may
// have trailing whitespace.
func splitScopes(s string) []string {
	parts := strings.Fields(s)
	if parts == nil {
		return []string{}
	}
	return parts
}
