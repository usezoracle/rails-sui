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
