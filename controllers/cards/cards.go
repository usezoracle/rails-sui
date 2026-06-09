// Package cards holds the controllers for the Tapp Card vertical.
//
// PoC scope (what's shipped today):
//   - POST /v1/cards/issue-batch   admin: mint N opaque activation URLs
//   - GET  /c/:token               public: 302 redirect to the PWA
//
// Post-PoC (per docs/tapp-card-spec.md, awaiting sign-off): link/claim,
// link/complete, resync, the merchant-facing debit endpoint, etc.

package cards

import (
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// Controller handles Tapp Card endpoints.
type Controller struct{}

// NewController returns a fresh Controller.
func NewController() *Controller { return &Controller{} }

// -----------------------------------------------------------------------------
// PoC: mint activation URLs the team can write to blank cards via NFC Tools.
// -----------------------------------------------------------------------------

type issueBatchPayload struct {
	Count int `json:"count" binding:"required,min=1,max=500"`
}

type issueBatchResponse struct {
	URLs []string `json:"urls"`
}

// IssueBatch creates `count` TappCard rows with fresh opaque activation
// tokens and returns the URLs ready to paste into NFC Tools. PoC-only;
// admin-gated by the ADMIN_API_TOKEN header. Replace with proper RBAC
// once the cardholder flow is built out.
func (ctrl *Controller) IssueBatch(ctx *gin.Context) {
	var payload issueBatchPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	bulk := make([]*ent.TappCardCreate, 0, payload.Count)
	tokens := make([]string, 0, payload.Count)
	for i := 0; i < payload.Count; i++ {
		token, err := generateActivationToken()
		if err != nil {
			logger.Errorf("cards.IssueBatch: token generation: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to mint cards", nil)
			return
		}
		bulk = append(bulk, storage.Client.TappCard.Create().SetActivationToken(token))
		tokens = append(tokens, token)
	}

	if _, err := storage.Client.TappCard.CreateBulk(bulk...).Save(ctx); err != nil {
		logger.Errorf("cards.IssueBatch: bulk persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to mint cards", nil)
		return
	}

	host := strings.TrimRight(config.ServerConfig().HostDomain, "/")
	resp := issueBatchResponse{URLs: make([]string, 0, len(tokens))}
	for _, t := range tokens {
		resp.URLs = append(resp.URLs, host+"/c/"+t)
	}
	u.APIResponse(ctx, http.StatusCreated, "success",
		"Issued cards — paste URLs into NFC Tools", resp)
}

// -----------------------------------------------------------------------------
// PoC: redirect a tapped URL to the right PWA route based on card state.
// -----------------------------------------------------------------------------

// Resolve handles GET /c/:token. Public route — anyone who taps a card
// hits this. We resolve the token → look up the row → 302 to the PWA
// with intent-appropriate route:
//
//   - issued       → PWA /link?token=…       (claim flow)
//   - claimed/live → PWA /dashboard/cards/:id (their card)
//   - revoked      → PWA /cards/revoked      (informational)
//
// 404 for unknown tokens, so an attacker pasting random URLs gets no
// signal about whether a token exists.
func (ctrl *Controller) Resolve(ctx *gin.Context) {
	token := ctx.Param("token")
	if token == "" {
		ctx.Status(http.StatusNotFound)
		return
	}

	card, err := storage.Client.TappCard.
		Query().
		Where(tappcard.ActivationTokenEQ(token)).
		Only(ctx)
	if err != nil {
		// ent.IsNotFound included — opaque 404 either way.
		ctx.Status(http.StatusNotFound)
		return
	}

	pwa := strings.TrimRight(config.ServerConfig().PWABaseURL, "/")
	var target string
	switch card.Status {
	case tappcard.StatusIssued:
		target = pwa + "/link?token=" + token
	case tappcard.StatusClaimed:
		// Claimed but linking isn't finished — there's no on-chain cap yet, so
		// the card can't transact. Send the holder back into the link flow to
		// finish (set PIN/limits → write K → create_cap), not to the dashboard.
		target = pwa + "/link/configure?card=" + card.ID.String()
	case tappcard.StatusLive:
		target = pwa + "/dashboard/cards/" + card.ID.String()
	case tappcard.StatusRevoked, tappcard.StatusLocked:
		target = pwa + "/cards/unavailable?status=" + string(card.Status)
	default:
		target = pwa + "/cards/unavailable"
	}
	ctx.Redirect(http.StatusFound, target)
}

// -----------------------------------------------------------------------------
// Cardholder: claim a freshly-issued card (PoC end-to-end).
// -----------------------------------------------------------------------------

type claimPayload struct {
	Token string `json:"token" binding:"required"`
}

type claimResponse struct {
	CardID string `json:"card_id"`
	Status string `json:"status"`
}

// Claim handles POST /v1/cards/link/claim. Authenticated via the
// shared JWTMiddleware which puts `user_id` in the gin context.
//
// State machine:
//   issued                 → flip to `claimed`, bind user_id, return.
//   claimed by this user   → idempotent, return the same card.
//   claimed by another user → 409 card_already_claimed_by_other.
//   anything else          → 409 with the actual status as the error code.
//
// Surface the structured `code` field so the PWA can branch on
// `card_already_claimed_by_you` vs generic failure without parsing
// human-facing copy.
func (ctrl *Controller) Claim(ctx *gin.Context) {
	var payload claimPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	userIDStr, exists := ctx.Get("user_id")
	if !exists {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Not authenticated", nil)
		return
	}
	userID, err := uuid.Parse(userIDStr.(string))
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Invalid session", nil)
		return
	}

	user, err := storage.Client.User.
		Query().
		Where(userEnt.IDEQ(userID)).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"User not found", nil)
		return
	}

	card, err := storage.Client.TappCard.
		Query().
		Where(tappcard.ActivationTokenEQ(payload.Token)).
		WithUser().
		Only(ctx)
	if err != nil {
		// ent.IsNotFound included — opaque 404, no oracle.
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"Card not recognized",
			map[string]any{"code": "card_not_found"})
		return
	}

	// Already claimed?
	if card.Edges.User != nil {
		if card.Edges.User.ID == user.ID {
			u.APIResponse(ctx, http.StatusOK, "error",
				"This card is already linked to your account",
				map[string]any{
					"code":    "card_already_claimed_by_you",
					"card_id": card.ID.String(),
				})
			return
		}
		u.APIResponse(ctx, http.StatusConflict, "error",
			"This card belongs to someone else",
			map[string]any{"code": "card_already_claimed_by_other"})
		return
	}

	if card.Status != tappcard.StatusIssued {
		u.APIResponse(ctx, http.StatusConflict, "error",
			"This card is not available for claiming",
			map[string]any{"code": "card_unavailable", "status": string(card.Status)})
		return
	}

	saved, err := card.Update().
		SetStatus(tappcard.StatusClaimed).
		SetUser(user).
		Save(ctx)
	if err != nil {
		logger.Errorf("cards.Claim: persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to claim card", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Card claimed",
		claimResponse{
			CardID: saved.ID.String(),
			Status: string(saved.Status),
		})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// Crockford base32 alphabet — case-insensitive friendly, no ambiguous
// glyphs (no I, L, O, U). 16 chars × 5 bits = 80 bits of entropy.
const activationAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func generateActivationToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 16)
	for i, b := range buf {
		out[i] = activationAlphabet[int(b)&0x1F]
	}
	return string(out), nil
}

// AdminTokenMiddleware gates admin endpoints with a single shared
// secret in the X-Admin-Token header. PoC-grade — swap for proper
// RBAC + audit trail before exposing externally.
func AdminTokenMiddleware(ctx *gin.Context) {
	want := config.ServerConfig().AdminAPIToken
	if want == "" {
		// Misconfigured: refuse rather than allow.
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error",
			"Admin endpoints disabled — ADMIN_API_TOKEN not set", nil)
		ctx.Abort()
		return
	}
	got := ctx.GetHeader("X-Admin-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Invalid admin token", nil)
		ctx.Abort()
		return
	}
	ctx.Next()
}
