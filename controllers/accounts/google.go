// Google OAuth (one-tap / button-style) entrypoint shared by the Tapp PWA
// (cardholder) and the LP dashboard (provider).
//
// Flow:
//   1. Client opens Google's sign-in UI via @react-oauth/google.
//   2. Google returns an ID token (JWT) signed by Google.
//   3. Client POSTs the ID token (and, for the LP dashboard, scope="provider"
//      plus a currency for first-time sign-up) to `/v1/auth/google`.
//   4. We verify the token against Google's JWKS via the official
//      `google.golang.org/api/idtoken` package — handles JWKS caching
//      + signature + audience + expiry checks in one call.
//   5. Find or create a User keyed on the verified email, tagged with the
//      requested scope. For providers we also create the ProviderProfile +
//      API key (same as email/password registration). Google-verified email
//      means no separate OTP step is needed.
//   6. Return a Rails JWT pair (access + refresh) so the client can call
//      scope-gated endpoints with `Authorization: Bearer`.

package accounts

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/api/idtoken"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	authSvc "github.com/usezoracle/rails-sui/services/auth"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
	"github.com/usezoracle/rails-sui/utils/token"
)

// authConf is declared in auth.go (same package).
var _ = config.AuthConfig

const (
	cardholderScope = "cardholder"
	providerScope   = "provider"
)

type googleAuthPayload struct {
	IDToken string `json:"id_token" binding:"required"`
	// Scope selects the account type: "provider" (LP dashboard) or the default
	// "cardholder" (Tapp PWA). Empty → cardholder, preserving existing behaviour.
	Scope string `json:"scope"`
	// Currency (e.g. "NGN") is required only when creating a NEW provider account
	// (first provider sign-in). Ignored for cardholders and returning providers.
	Currency string `json:"currency"`
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

	requestedScope := cardholderScope
	if strings.EqualFold(strings.TrimSpace(payload.Scope), providerScope) {
		requestedScope = providerScope
	}

	// Upsert by email — same person logging back in keeps the same User across
	// methods and scopes. Load the provider profile to know whether we must
	// create one.
	user, lookupErr := db.Client.User.
		Query().
		Where(userEnt.EmailEQ(email)).
		WithProviderProfile().
		Only(ctx)
	if lookupErr != nil && !ent.IsNotFound(lookupErr) {
		logger.Errorf("google user lookup: %v", lookupErr)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to sign in", nil)
		return
	}
	isNewUser := ent.IsNotFound(lookupErr)

	if requestedScope == providerScope {
		// Provider (LP) sign-in. Ensure a provider account exists with a
		// ProviderProfile + API key — mirrors the provider branch of
		// email/password registration. All writes run in one transaction.
		needsProfile := isNewUser || user.Edges.ProviderProfile == nil
		if needsProfile && strings.TrimSpace(payload.Currency) == "" {
			u.APIResponse(ctx, http.StatusBadRequest, "error",
				"Currency is required to create a provider account",
				[]types.ErrorData{{Field: "Currency", Message: "Currency is required"}})
			return
		}

		tx, txErr := db.Client.Tx(ctx)
		if txErr != nil {
			logger.Errorf("google provider tx: %v", txErr)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to sign in", nil)
			return
		}

		if isNewUser {
			user, err = tx.User.Create().
				SetEmail(email).
				SetFirstName(givenName).
				SetLastName(familyName).
				SetPassword("google-oauth").
				SetScope(providerScope).
				SetIsEmailVerified(true). // Google already verified the email
				Save(ctx)
			if err != nil {
				_ = tx.Rollback()
				logger.Errorf("google create provider user: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to create user", nil)
				return
			}
		} else if !strings.Contains(user.Scope, providerScope) {
			user, err = tx.User.UpdateOne(user).
				SetScope(strings.TrimSpace(user.Scope + " " + providerScope)).
				SetIsEmailVerified(true).
				Save(ctx)
			if err != nil {
				_ = tx.Rollback()
				logger.Errorf("google grant provider scope: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to update user", nil)
				return
			}
		}

		if needsProfile {
			currency, err := tx.FiatCurrency.Query().
				Where(fiatcurrency.IsEnabledEQ(true), fiatcurrency.CodeEQ(payload.Currency)).
				Only(ctx)
			if err != nil {
				_ = tx.Rollback()
				if ent.IsNotFound(err) {
					u.APIResponse(ctx, http.StatusBadRequest, "error",
						"Failed to validate payload",
						[]types.ErrorData{{Field: "Currency", Message: "Currency is not supported"}})
					return
				}
				logger.Errorf("google currency lookup: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to sign in", nil)
				return
			}

			provider, err := tx.ProviderProfile.Create().
				SetCurrency(currency).
				SetVisibilityMode(providerprofile.VisibilityModePrivate).
				SetUser(user).
				SetProvisionMode(providerprofile.ProvisionModeAuto).
				Save(ctx)
			if err != nil {
				_ = tx.Rollback()
				logger.Errorf("google create provider profile: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to sign in", nil)
				return
			}
			if _, _, err := ctrl.apiKeyService.GenerateAPIKey(ctx, tx, nil, provider); err != nil {
				_ = tx.Rollback()
				logger.Errorf("google provider api key: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to sign in", nil)
				return
			}
		}

		if err := tx.Commit(); err != nil {
			logger.Errorf("google provider commit: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to sign in", nil)
			return
		}
	} else {
		// Cardholder (Tapp PWA) — unchanged behaviour.
		if isNewUser {
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
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to create user", nil)
				return
			}
		} else if !strings.Contains(user.Scope, cardholderScope) {
			scopes := strings.Fields(user.Scope)
			scopes = append(scopes, cardholderScope)
			user, err = user.Update().
				SetScope(strings.Join(scopes, " ")).
				SetIsEmailVerified(true).
				Save(ctx)
			if err != nil {
				logger.Errorf("grant cardholder scope: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to update user", nil)
				return
			}
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
