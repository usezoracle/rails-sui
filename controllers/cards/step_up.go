// WebAuthn step-up grant endpoint.
//
// Flow (full picture in docs/tap-card-pin-flow.md "Step-up QR"):
//
//   1. Merchant POSTs debit with amount above the step-up threshold.
//   2. Server responds 402 step_up_required + step_up_token (the
//      CardServerNonce UUID — see merchant_tap.go's TapCardNonce).
//   3. Merchant app renders a QR pointing at the PWA:
//      https://tapp.zoracle.com/cards/step-up?token=<UUID>
//   4. Cardholder scans, signs in (zkLogin), runs WebAuthn platform
//      authenticator (Face ID / Touch ID), and POSTs here.
//   5. We mark step_up_granted_at on the matching CardServerNonce.
//   6. Merchant's GET /v1/sender/me/tap-card/step-up?token=… poll
//      returns 200 granted; merchant app re-submits the debit with
//      step_up_token; TapCardDebit checks the grant flag and proceeds.
//
// v1 trust model for the WebAuthn assertion: we accept any POST from
// an authenticated cardholder owning the target card. Per-card
// credential registration + signature verification via
// `github.com/go-webauthn/webauthn` is the right next step but lives
// behind a registration flow we haven't built yet. Marked TODO inline.

package cards

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/cardservernonce"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

type stepUpGrantRequest struct {
	Token string `json:"token" binding:"required"`
	// WebAuthn assertion the PWA produced from the platform
	// authenticator. v1 accepts but doesn't verify — TODO below.
	WebauthnAssertion map[string]any `json:"webauthn_assertion"`
}

type stepUpParseRequest struct {
	Token string `json:"token" binding:"required"`
}

type stepUpParseResponse struct {
	Amount       string    `json:"amount"`
	Currency     string    `json:"currency"`
	ExpiresAt    time.Time `json:"expires_at"`
	CardID       string    `json:"card_id"`
	MerchantName string    `json:"merchant_name"`
}

// StepUpParse returns details about the step-up token for display in the PWA.
func (ctrl *Controller) StepUpParse(ctx *gin.Context) {
	var req stepUpParseRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}

	nonceID, err := uuid.Parse(req.Token)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Invalid step-up token", nil)
		return
	}

	// Load the nonce + its card + its sender profile + sender user, verify the card belongs to the
	// authenticated user. Prevents one user from reading another
	// user's step-up.
	nonceRow, err := storage.Client.CardServerNonce.
		Query().
		Where(
			cardservernonce.IDEQ(nonceID),
			cardservernonce.TierEQ(cardservernonce.TierStepUp),
			cardservernonce.HasCardWith(
				tappcard.HasUserWith(userEnt.IDEQ(user.ID)),
			),
		).
		WithCard().
		WithSenderProfile(func(q *ent.SenderProfileQuery) {
			q.WithUser()
		}).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"Step-up token not found for this account",
			map[string]any{"code": "step_up_token_invalid"})
		return
	}

	if nonceRow.ExpiresAt.Before(time.Now()) {
		u.APIResponse(ctx, http.StatusGone, "error",
			"Step-up token expired",
			map[string]any{"code": "step_up_token_expired"})
		return
	}

	merchantName := "Unknown Merchant"
	if nonceRow.Edges.SenderProfile != nil && nonceRow.Edges.SenderProfile.Edges.User != nil {
		mu := nonceRow.Edges.SenderProfile.Edges.User
		merchantName = mu.FirstName + " " + mu.LastName
	}

	cardID := ""
	if nonceRow.Edges.Card != nil {
		cardID = nonceRow.Edges.Card.ID.String()
	}

	resp := stepUpParseResponse{
		Amount:       nonceRow.Amount,
		Currency:     nonceRow.Currency,
		ExpiresAt:    nonceRow.ExpiresAt,
		CardID:       cardID,
		MerchantName: merchantName,
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Step-up details loaded", resp)
}

// StepUpGrant flips the step_up_granted_at flag on the nonce so the
// merchant-side poll picks it up.
func (ctrl *Controller) StepUpGrant(ctx *gin.Context) {
	var req stepUpGrantRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}

	nonceID, err := uuid.Parse(req.Token)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Invalid step-up token", nil)
		return
	}

	// Load the nonce + its card, verify the card belongs to the
	// authenticated user. Prevents one user from granting another
	// user's step-up.
	nonceRow, err := storage.Client.CardServerNonce.
		Query().
		Where(
			cardservernonce.IDEQ(nonceID),
			cardservernonce.TierEQ(cardservernonce.TierStepUp),
			cardservernonce.HasCardWith(
				tappcard.HasUserWith(userEnt.IDEQ(user.ID)),
			),
		).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"Step-up token not found for this account",
			map[string]any{"code": "step_up_token_invalid"})
		return
	}

	if nonceRow.ExpiresAt.Before(time.Now()) {
		u.APIResponse(ctx, http.StatusGone, "error",
			"Step-up token expired",
			map[string]any{"code": "step_up_token_expired"})
		return
	}
	if nonceRow.StepUpGrantedAt != nil {
		u.APIResponse(ctx, http.StatusOK, "success",
			"Already granted", map[string]any{"acknowledged": true})
		return
	}

	// TODO(webauthn): verify req.WebauthnAssertion against the
	// cardholder's registered platform credential via
	// github.com/go-webauthn/webauthn. Requires a registration flow
	// that lands as part of the cardholder onboarding spike.

	if _, err := nonceRow.Update().
		SetStepUpGrantedAt(time.Now()).
		Save(ctx); err != nil {
		logger.Errorf("StepUpGrant: persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to record grant", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Granted",
		map[string]any{"acknowledged": true})
}
