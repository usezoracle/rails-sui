package accounts

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/ent/verificationtoken"
	svc "github.com/usezoracle/rails-sui/services"
	authSvc "github.com/usezoracle/rails-sui/services/auth"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/crypto"
	"github.com/usezoracle/rails-sui/utils/logger"
	"github.com/usezoracle/rails-sui/utils/token"
)

var authConf = config.AuthConfig()
var serverConf = config.ServerConfig()

type AuthController struct {
	apiKeyService *svc.APIKeyService
	emailService  *svc.EmailService
}

func NewAuthController() *AuthController {
	return &AuthController{
		apiKeyService: svc.NewAPIKeyService(),
		emailService:  svc.NewEmailService(svc.DefaultMailProvider()),
	}
}

func (ctrl *AuthController) Register(ctx *gin.Context) {
	var payload types.RegisterPayload

	serverConf := config.ServerConfig()

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	tx, err := db.Client.Tx(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to create new user", nil)
		return
	}

	// Check if user with email already exists
	userTmp, _ := tx.User.
		Query().
		Where(
			userEnt.EmailEQ(strings.ToLower(payload.Email)),
		).
		Only(ctx)

	if userTmp != nil {
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"User with email already exists", nil)
		return
	}

	// Save the user
	scope := strings.Join(payload.Scopes, " ")
	userCreate := tx.User.
		Create().
		SetFirstName(payload.FirstName).
		SetLastName(payload.LastName).
		SetEmail(strings.ToLower(payload.Email)).
		SetPassword(payload.Password).
		SetScope(scope)

	// Auto-verify ONLY in true local dev. Every deployed environment requires
	// the user to verify their email before the account is usable.
	if serverConf.Environment == "local" {
		userCreate = userCreate.
			SetIsEmailVerified(true)
	}

	// Providers (LPs) are onboarded partners, not part of the consumer beta —
	// so they must not be blocked by the early-access gate at login. Google
	// sign-in already grants this; keep email/password registration consistent
	// so an LP who registers can actually log in (after verifying their email).
	if u.ContainsString(payload.Scopes, "provider") {
		userCreate = userCreate.SetHasEarlyAccess(true)
	}

	user, err := userCreate.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to create new user", nil)
		return
	}

	// Issue verification OTP. We store SHA-256(code) in the DB and email
	// the 6-digit code to the user. On confirm, we hash the submitted code
	// and compare (with a per-email attempt cap — see otp_guard.go).
	rawToken, err := token.GenerateOTP()
	if err != nil {
		logger.Errorf("error: %v", err)
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to create new user", nil)
		return
	}
	_, vtErr := tx.VerificationToken.
		Create().
		SetOwner(user).
		SetToken(token.HashToken(rawToken)).
		SetScope(verificationtoken.ScopeEmailVerification).
		SetExpiryAt(time.Now().Add(authConf.EmailVerificationLifespan)).
		Save(ctx)
	if vtErr != nil {
		logger.Errorf("error: %v", vtErr)
	}

	// Send the verification email in every deployed environment (not just prod).
	if serverConf.Environment != "local" && vtErr == nil {
		// Fresh code → clear any stale attempt counter for this email.
		clearOTPAttempts(ctx, string(verificationtoken.ScopeEmailVerification), user.Email)
		if _, err := ctrl.emailService.SendVerificationEmail(ctx, rawToken, user.Email, user.FirstName); err != nil {
			logger.Errorf("error: %v", err)
		}
	}

	scopes := payload.Scopes

	// Create a provider profile
	if u.ContainsString(scopes, "provider") {
		// Fetch currency
		if payload.Currency == "" {
			_ = tx.Rollback()
			u.APIResponse(ctx, http.StatusBadRequest, "error",
				"Currency is required for provider account", nil)
			return
		}
		currency, err := tx.FiatCurrency.
			Query().
			Where(
				fiatcurrency.IsEnabledEQ(true),
				fiatcurrency.CodeEQ(payload.Currency),
			).
			Only(ctx)
		if err != nil {
			_ = tx.Rollback()
			if ent.IsNotFound(err) {
				u.APIResponse(ctx, http.StatusBadRequest, "error",
					"Failed to validate payload", []types.ErrorData{{
						Field:   "Currency",
						Message: "Currency is not supported",
					}})
				return
			}
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to create new user", nil)
			return
		}

		provider, err := tx.ProviderProfile.
			Create().
			SetCurrency(currency).
			SetVisibilityMode(providerprofile.VisibilityModePrivate).
			SetUser(user).
			SetProvisionMode(providerprofile.ProvisionModeAuto).
			Save(ctx)
		if err != nil {
			_ = tx.Rollback()
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to create new user", nil)
			return
		}

		// Generate the API key using the service
		_, _, err = ctrl.apiKeyService.GenerateAPIKey(ctx, tx, nil, provider)
		if err != nil {
			_ = tx.Rollback()
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to create new user", nil)
			return
		}
	}

	// Create a sender profile
	if u.ContainsString(scopes, "sender") {
		sender, err := tx.SenderProfile.
			Create().
			SetUser(user).
			Save(ctx)
		if err != nil {
			_ = tx.Rollback()
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to create new user", nil)
			return
		}

		// Generate the API key using the service
		_, _, err = ctrl.apiKeyService.GenerateAPIKey(ctx, tx, sender, nil)
		if err != nil {
			_ = tx.Rollback()
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to create new user", nil)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to create new user", nil)
		return
	}

	// Auto-login (return tokens) ONLY in local dev. In every deployed
	// environment the user must verify their email, then log in — register
	// never returns usable credentials for an unverified account.
	var accessToken, refreshToken string
	if serverConf.Environment == "local" {
		var err error
		accessToken, err = token.GenerateAccessJWT(user.ID.String(), user.Scope)
		if err != nil {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to generate access token on registration", nil)
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
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to generate refresh token on registration", nil)
			return
		}
		refreshToken = issued.Raw
	}

	response := &types.RegisterResponse{
		ID:           user.ID,
		CreatedAt:    user.CreatedAt,
		UpdatedAt:    user.UpdatedAt,
		FirstName:    user.FirstName,
		LastName:     user.LastName,
		Email:        user.Email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}

	u.APIResponse(ctx, http.StatusCreated, "success", "User created successfully", response)
}

// Login controller validates the payload and creates a new user.
func (ctrl *AuthController) Login(ctx *gin.Context) {
	var payload types.LoginPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	// Fetch user by email
	user, err := db.Client.User.
		Query().
		Where(userEnt.EmailEQ(strings.ToLower(payload.Email))).
		Only(ctx)

	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Email and password do not match any user", nil,
		)
		return
	}

	// Check if the password is correct
	passwordMatch := crypto.CheckPasswordHash(payload.Password, user.Password)
	if !passwordMatch {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Email and password do not match any user", nil,
		)
		return
	}

	// Check if user has early access. Providers (LPs) are onboarded partners,
	// not part of the consumer beta, so the early-access wall never applies to
	// them — this also unblocks LP accounts created before early access was
	// granted on registration.
	environment := serverConf.Environment
	isProvider := u.ContainsString(strings.Fields(user.Scope), "provider")
	if !user.HasEarlyAccess && !isProvider && (environment == "production" || environment == "staging") {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Your early access request is still pending", nil,
		)
		return
	}

	// Email verification is COMPULSORY in every deployed environment — block
	// login until verified. Local dev auto-verifies at registration, so this
	// never blocks there. Fail-closed: a misconfigured ENVIRONMENT still
	// enforces verification rather than silently disabling it.
	if serverConf.Environment != "local" && !user.IsEmailVerified {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Email is not verified, please verify your email", nil,
		)
		return
	}

	// Stateless short-lived access JWT.
	accessToken, err := token.GenerateAccessJWT(user.ID.String(), user.Scope)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to create access token", nil,
		)
		return
	}

	// Stateful opaque refresh token — issued in a new family. Revocable
	// via /auth/logout, rotated on every /auth/refresh.
	refreshTTL := time.Duration(authConf.JwtRefreshLifespan) * time.Minute
	issued, err := authSvc.IssueNewFamily(
		ctx,
		user.ID,
		refreshTTL,
		ctx.GetHeader("User-Agent"),
		ctx.ClientIP(),
	)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to create refresh token", nil,
		)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Successfully logged in", &types.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: issued.Raw,
		Scopes:       strings.Split(user.Scope, " "),
	})
}

// RefreshJWT rotates the refresh token and returns a fresh (access,
// refresh) pair. The old refresh is revoked atomically with the issue.
// Replay of a revoked token revokes the whole family — the user (and
// any attacker holding a copy) must re-login.
func (ctrl *AuthController) RefreshJWT(ctx *gin.Context) {
	var payload types.RefreshJWTPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	refreshTTL := time.Duration(authConf.JwtRefreshLifespan) * time.Minute
	issued, user, err := authSvc.Rotate(
		ctx,
		payload.RefreshToken,
		refreshTTL,
		ctx.GetHeader("User-Agent"),
		ctx.ClientIP(),
	)
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid or expired refresh token", nil)
		return
	}

	accessToken, err := token.GenerateAccessJWT(user.ID.String(), user.Scope)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to generate access token", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Successfully refreshed access token", &types.RefreshResponse{
		AccessToken:  accessToken,
		RefreshToken: issued.Raw,
	})
}

// Logout revokes the presented refresh-token family. Unauthenticated +
// idempotent — always returns 200 so:
//
//   - a client whose access JWT has expired can still sign out cleanly
//   - we don't leak whether the refresh token was valid/known
//   - hitting it without a body is safe (no-op)
//
// Rate-limited at the route level to stop someone hammering it as a
// crude revocation oracle.
func (ctrl *AuthController) Logout(ctx *gin.Context) {
	var payload types.LogoutPayload
	_ = ctx.ShouldBindJSON(&payload)

	if payload.RefreshToken != "" {
		_ = authSvc.RevokeByRaw(ctx, payload.RefreshToken)
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Logged out", nil)
}

// ConfirmEmail controller validates the payload and confirm the users email.
func (ctrl *AuthController) ConfirmEmail(ctx *gin.Context) {
	var payload types.ConfirmEmailPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	scope := string(verificationtoken.ScopeEmailVerification)

	// Brute-force guard: a 6-digit code is only safe behind an attempt cap.
	if otpAttemptsExceeded(ctx, scope, payload.Email) {
		u.APIResponse(ctx, http.StatusTooManyRequests, "error",
			"Too many incorrect attempts — request a new code", nil)
		return
	}

	// Hash the submitted code and compare against the at-rest hash.
	// `verificationtoken.token` column holds SHA-256(raw), not raw.
	verificationToken, vtErr := db.Client.VerificationToken.
		Query().
		Where(
			verificationtoken.TokenEQ(token.HashToken(payload.Token)),
			verificationtoken.HasOwnerWith(userEnt.EmailEQ(payload.Email)),
		).
		WithOwner().
		Only(ctx)
	if vtErr != nil {
		recordOTPFailure(ctx, scope, payload.Email, authConf.EmailVerificationLifespan)
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid verification code", nil)
		return
	}

	if time.Now().After(verificationToken.ExpiryAt) {
		err := db.Client.VerificationToken.
			DeleteOneID(verificationToken.ID).Exec(ctx)
		if err != nil {
			logger.Errorf("ConfirmEmailError.VerificationToken.Delete: %v", err)
		}
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Token is expired", nil)
		return
	}

	// Update User IsEmailVerified to true
	_, setIfVerifiedErr := verificationToken.Edges.Owner.
		Update().
		SetIsEmailVerified(true).
		Save(ctx)
	if setIfVerifiedErr != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to verify user email", setIfVerifiedErr.Error())
		return
	}

	err := db.Client.VerificationToken.
		DeleteOneID(verificationToken.ID).Exec(ctx)
	if err != nil {
		logger.Errorf("ConfirmEmailError.VerificationToken.Delete: %v", err)
	}
	clearOTPAttempts(ctx, scope, payload.Email)

	// Return a success response
	u.APIResponse(ctx, http.StatusOK, "success", "User email verified successfully", nil)
}

// ResendVerificationToken controller resends the verification token to the users email.
func (ctrl *AuthController) ResendVerificationToken(ctx *gin.Context) {
	var payload types.ResendTokenPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	// Fetch User account.
	user, userErr := db.Client.User.Query().Where(userEnt.EmailEQ(payload.Email)).Only(ctx)
	if userErr != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid credential", userErr.Error())
		return
	}

	// Generate a fresh OTP — store hash, email the code.
	rawToken, err := token.GenerateOTP()
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to generate verification token", err.Error())
		return
	}
	// Verification tokens get a longer TTL (24h) than password-reset
	// tokens (15min) — different threat model. Verification can sit in
	// an inbox; reset is short-lived because the user should act now.
	ttl := authConf.EmailVerificationLifespan
	if verificationtoken.Scope(payload.Scope) == verificationtoken.ScopeResetPassword {
		ttl = authConf.PasswordResetLifespan
	}
	_, vtErr := db.Client.VerificationToken.
		Create().
		SetOwner(user).
		SetToken(token.HashToken(rawToken)).
		SetScope(verificationtoken.Scope(payload.Scope)).
		SetExpiryAt(time.Now().Add(ttl)).
		Save(ctx)
	if vtErr != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to generate verification token", vtErr.Error())
		return
	}

	// Fresh code → reset the attempt counter for this (scope,email).
	clearOTPAttempts(ctx, payload.Scope, user.Email)

	// Send the email that matches the scope — reset codes get the reset copy.
	var sendErr error
	if verificationtoken.Scope(payload.Scope) == verificationtoken.ScopeResetPassword {
		_, sendErr = ctrl.emailService.SendPasswordResetEmail(ctx, rawToken, user.Email, user.FirstName)
	} else {
		_, sendErr = ctrl.emailService.SendVerificationEmail(ctx, rawToken, user.Email, user.FirstName)
	}
	if sendErr != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to send verification email", sendErr.Error())
		return
	}

	// Return a success response
	u.APIResponse(ctx, http.StatusOK, "success", "Verification token has been sent to your email", nil)
}

// ResetPassword resets user's password. A valid token is required to set new password
func (ctrl *AuthController) ResetPassword(ctx *gin.Context) {
	var payload types.ResetPasswordPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	scope := string(verificationtoken.ScopeResetPassword)

	// Brute-force guard: a 6-digit reset code grants account access, so the
	// attempt cap is the primary defense.
	if otpAttemptsExceeded(ctx, scope, payload.Email) {
		u.APIResponse(ctx, http.StatusTooManyRequests, "error",
			"Too many incorrect attempts — request a new code", nil)
		return
	}

	// Verify reset code — scoped to the owning email. A 6-digit OTP is not
	// globally unique, so without the email filter two users could share a code
	// (matching the wrong row, or erroring on .Only with multiple matches).
	resetTokenRow, err := db.Client.VerificationToken.
		Query().
		Where(
			verificationtoken.TokenEQ(token.HashToken(payload.ResetToken)),
			verificationtoken.ScopeEQ(verificationtoken.ScopeResetPassword),
			verificationtoken.HasOwnerWith(userEnt.EmailEQ(payload.Email)),
		).
		WithOwner().
		Only(ctx)
	if err != nil || resetTokenRow == nil || resetTokenRow.Edges.Owner == nil {
		recordOTPFailure(ctx, scope, payload.Email, authConf.PasswordResetLifespan)
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid password reset code", nil)
		return
	}

	if time.Now().After(resetTokenRow.ExpiryAt) {
		err := db.Client.VerificationToken.
			DeleteOneID(resetTokenRow.ID).Exec(ctx)
		if err != nil {
			logger.Errorf("ResetPasswordError.VerificationToken.Delete: %v", err)
		}
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Token is expired", nil)
		return
	}

	_, err = db.Client.User.
		UpdateOne(resetTokenRow.Edges.Owner).
		SetPassword(payload.Password).
		Save(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to reset password", nil)
		return
	}

	// Delete verification token — single-use.
	verificationErr := db.Client.VerificationToken.
		DeleteOneID(resetTokenRow.ID).Exec(ctx)
	if verificationErr != nil {
		logger.Errorf("ResetPasswordError.VerificationToken.Delete: %v", verificationErr)
	}
	clearOTPAttempts(ctx, scope, payload.Email)

	// Industry-standard hygiene: revoke EVERY active refresh-token
	// family for this user. If the original account was compromised,
	// the attacker's session is killed alongside the legitimate ones —
	// the user logs back in fresh on each device after the reset.
	if revokeErr := authSvc.RevokeAllForUser(ctx, resetTokenRow.Edges.Owner.ID); revokeErr != nil {
		logger.Errorf("ResetPasswordError.RevokeRefreshTokens: %v", revokeErr)
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Password reset was successful", nil)
}

// ResetPasswordToken sends a reset password token to user's email
func (ctrl *AuthController) ResetPasswordToken(ctx *gin.Context) {
	var payload types.ResetPasswordTokenPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	// Get user account.
	user, userErr := db.Client.User.
		Query().
		Where(userEnt.EmailEQ(payload.Email)).
		Only(ctx)
	if userErr != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Email does not belong to any user", nil)
		return
	}

	// Generate a 6-digit reset OTP — store hash, email the code.
	rawResetToken, err := token.GenerateOTP()
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to generate reset password token", nil)
		return
	}
	if _, rtErr := db.Client.VerificationToken.
		Create().
		SetOwner(user).
		SetToken(token.HashToken(rawResetToken)).
		SetScope(verificationtoken.ScopeResetPassword).
		SetExpiryAt(time.Now().Add(authConf.PasswordResetLifespan)).
		Save(ctx); rtErr != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to generate reset password token", nil)
		return
	}

	// Fresh code → reset the attempt counter for this email.
	clearOTPAttempts(ctx, string(verificationtoken.ScopeResetPassword), user.Email)

	if _, err := ctrl.emailService.SendPasswordResetEmail(ctx, rawResetToken, user.Email, user.FirstName); err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to send reset password token", nil)
		return
	}

	// Return a success response
	u.APIResponse(ctx, http.StatusOK, "success", "A reset token has been sent to your email", nil)
}

// ChangePassword changes user's password. An authorized user is required to change password
func (ctrl *AuthController) ChangePassword(ctx *gin.Context) {
	var payload types.ChangePasswordPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	// get user id from context
	user_id := ctx.GetString("user_id")
	// parse user id to uuid
	userID, err := uuid.Parse(user_id)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid credential", nil)
		return
	}

	// Fetch user account.
	user, err := db.Client.User.
		Query().
		Where(userEnt.IDEQ(userID)).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid credential", nil)
		return
	}

	// Check if the old password is correct
	passwordMatch := crypto.CheckPasswordHash(payload.OldPassword, user.Password)
	if !passwordMatch {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Old password is incorrect", nil)
		return
	}

	// Check if the new password is the same as the old password
	passwordMatch = crypto.CheckPasswordHash(payload.NewPassword, user.Password)
	if passwordMatch {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "New password cannot be the same as old password", nil)
		return
	}

	// Update user password
	_, err = db.Client.User.
		UpdateOne(user).
		SetPassword(payload.NewPassword).
		Save(ctx)

	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to change password", nil)
		return
	}

	// Return a success response
	u.APIResponse(ctx, http.StatusOK, "success", "Password changed successfully", nil)
}
