// Google OAuth (one-tap / button-style) entrypoint for the Tapp PWA.
//
// Flow:
//   1. PWA opens Google's sign-in UI via @react-oauth/google.
//   2. Google returns an ID token (JWT) signed by Google.
//   3. PWA POSTs the ID token to `/v1/auth/google`.
//   4. We verify the token against Google's JWKS via the official
//      `google.golang.org/api/idtoken` package — handles JWKS caching
//      + signature + audience + expiry checks in one call.
//   5. Find or create a User row keyed on the verified email,
//      tagged with the `cardholder` scope.
//   6. Return a Rails JWT pair (access + refresh) so the PWA can call
//      cardholder-scoped endpoints with `Authorization: Bearer`.

package accounts

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/api/idtoken"

	"github.com/usezoracle/rails-sui/config"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	authSvc "github.com/usezoracle/rails-sui/services/auth"
	db "github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
	"github.com/usezoracle/rails-sui/utils/token"
)

// authConf is declared in auth.go (same package).
var _ = config.AuthConfig

const cardholderScope = "cardholder"

type googleAuthPayload struct {
	IDToken string `json:"id_token" binding:"required"`
}

type googleAuthResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email"`
	Scope        string `json:"scope"`
	IsNewUser    bool   `json:"is_new_user"`
}

// GoogleAuth handles POST /v1/auth/google.
func (ctrl *AuthController) GoogleAuth(ctx *gin.Context) {
	var payload googleAuthPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	clientID := config.ServerConfig().GoogleOAuthClientID
	if clientID == "" {
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error",
			"Google sign-in not configured — set GOOGLE_OAUTH_CLIENT_ID", nil)
		return
	}

	// Validates signature against Google's JWKS, audience match, expiry.
	gPayload, err := idtoken.Validate(ctx, payload.IDToken, clientID)
	if err != nil {
		logger.Errorf("google id_token validate: %v", err)
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Invalid Google credential", nil)
		return
	}

	email, _ := gPayload.Claims["email"].(string)
	emailVerified, _ := gPayload.Claims["email_verified"].(bool)
	givenName, _ := gPayload.Claims["given_name"].(string)
	familyName, _ := gPayload.Claims["family_name"].(string)
	if email == "" || !emailVerified {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Google account email is not verified", nil)
		return
	}
	email = strings.ToLower(email)

	// Upsert by email — same person logging back in keeps the same User.
	user, err := db.Client.User.
		Query().
		Where(userEnt.EmailEQ(email)).
		Only(ctx)

	isNewUser := false
	if err != nil {
		// Create. No password — Google is the credential.
		isNewUser = true
		user, err = db.Client.User.Create().
			SetEmail(email).
			SetFirstName(givenName).
			SetLastName(familyName).
			SetPassword("google-oauth").
			SetScope(cardholderScope).
			SetIsEmailVerified(true).
			SetHasEarlyAccess(true).
			Save(ctx)
		if err != nil {
			logger.Errorf("create cardholder user: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to create user", nil)
			return
		}
	} else if !strings.Contains(user.Scope, cardholderScope) {
		// Existing email (signed up via a different path) — grant the
		// cardholder scope alongside whatever they already have.
		scopes := strings.Fields(user.Scope)
		scopes = append(scopes, cardholderScope)
		user, err = user.Update().
			SetScope(strings.Join(scopes, " ")).
			SetIsEmailVerified(true).
			Save(ctx)
		if err != nil {
			logger.Errorf("grant cardholder scope: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to update user", nil)
			return
		}
	}

	// Stateless access JWT + stateful opaque refresh token (rotation).
	access, err := token.GenerateAccessJWT(user.ID.String(), user.Scope)
	if err != nil {
		logger.Errorf("jwt mint: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to mint session", nil)
		return
	}
	refreshTTL := time.Duration(authConf.JwtRefreshLifespan) * time.Minute
	issued, err := authSvc.IssueNewFamily(
		ctx,
		user.ID,
		refreshTTL,
		ctx.GetHeader("User-Agent"),
		ctx.ClientIP(),
	)
	if err != nil {
		logger.Errorf("refresh mint: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to mint session", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Signed in with Google",
		&googleAuthResponse{
			AccessToken:  access,
			RefreshToken: issued.Raw,
			Email:        user.Email,
			Scope:        user.Scope,
			IsNewUser:    isNewUser,
		})
}
