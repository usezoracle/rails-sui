package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// CardOpsController gives operators break-glass control over Tapp cards: inspect
// state, recover a card the lock machinery has bricked (5 PIN fails / 3 token
// mismatches), and force a status (revoke a lost card, lock a compromised one).
// All writes audited.
type CardOpsController struct{}

// NewCardOpsController constructs the controller.
func NewCardOpsController() *CardOpsController { return &CardOpsController{} }

func (c *CardOpsController) load(ctx *gin.Context) (*ent.TappCard, bool) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return nil, false
	}
	card, err := storage.Client.TappCard.Query().
		Where(tappcard.IDEQ(id)).WithUser().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "card not found", nil)
			return nil, false
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load card", nil)
		return nil, false
	}
	return card, true
}

func cardView(card *ent.TappCard) gin.H {
	owner := ""
	if card.Edges.User != nil {
		owner = card.Edges.User.Email
	}
	lockedUntil := ""
	if card.LockedUntil != nil {
		lockedUntil = card.LockedUntil.Format(tsLayout)
	}
	return gin.H{
		"id":                     card.ID.String(),
		"status":                 card.Status.String(),
		"owner":                  owner,
		"pin_attempts_remaining": card.PinAttemptsRemaining,
		"token_mismatch_count":   card.TokenMismatchCount,
		"locked_until":           lockedUntil,
		"needs_resync":           card.NeedsResync,
		"created_at":             card.CreatedAt.Format(tsLayout),
	}
}

// GetCard returns one card's operational state.
//
//	GET /v1/admin/cards/:id
func (c *CardOpsController) GetCard(ctx *gin.Context) {
	card, ok := c.load(ctx)
	if !ok {
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", cardView(card))
}

// Unlock recovers a card from the lock state: clears locked_until, resets PIN
// attempts and the token-mismatch counter, and restores it to live. Refuses if
// the card is revoked (revocation is intentional and on-chain — un-revoking is
// not an admin operation).
//
//	POST /v1/admin/cards/:id/unlock
func (c *CardOpsController) Unlock(ctx *gin.Context) {
	card, ok := c.load(ctx)
	if !ok {
		return
	}
	if card.Status == tappcard.StatusRevoked {
		u.APIResponse(ctx, http.StatusConflict, "error", "card is revoked — cannot unlock", nil)
		return
	}
	_, err := card.Update().
		SetStatus(tappcard.StatusLive).
		SetPinAttemptsRemaining(5).
		SetTokenMismatchCount(0).
		ClearLockedUntil().
		Save(ctx)
	if err != nil {
		logger.Errorf("admin card unlock %s: %v", card.ID, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to unlock card", nil)
		return
	}
	writeAudit(ctx, "card.unlock", card.ID.String(), map[string]any{
		"previous_status": card.Status.String(),
	})
	fresh, _ := c.load(ctx)
	u.APIResponse(ctx, http.StatusOK, "success", "card unlocked", cardView(fresh))
}

type cardStatusReq struct {
	Status string `json:"status" binding:"required"` // revoked | locked
	Reason string `json:"reason"`
}

// SetStatus forces a card status — revoke a lost/stolen card or lock a
// compromised one. Lock sets a 24h auto-unlock window (matching the PIN-lock
// behaviour); operators can Unlock early. Only revoked|locked are allowed —
// restoring to live goes through Unlock.
//
//	POST /v1/admin/cards/:id/status
func (c *CardOpsController) SetStatus(ctx *gin.Context) {
	card, ok := c.load(ctx)
	if !ok {
		return
	}
	var body cardStatusReq
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "status is required", nil)
		return
	}
	target := strings.ToLower(strings.TrimSpace(body.Status))

	upd := card.Update()
	switch target {
	case "revoked":
		upd = upd.SetStatus(tappcard.StatusRevoked)
	case "locked":
		upd = upd.SetStatus(tappcard.StatusLocked).
			SetLockedUntil(time.Now().Add(24 * time.Hour)).
			SetPinAttemptsRemaining(0)
	default:
		u.APIResponse(ctx, http.StatusBadRequest, "error", "status must be revoked or locked (use unlock to restore)", nil)
		return
	}

	if _, err := upd.Save(ctx); err != nil {
		logger.Errorf("admin card set-status %s -> %s: %v", card.ID, target, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to set card status", nil)
		return
	}
	writeAudit(ctx, "card.set_status", card.ID.String(), map[string]any{
		"previous_status": card.Status.String(),
		"new_status":      target,
		"reason":          body.Reason,
	})
	fresh, _ := c.load(ctx)
	u.APIResponse(ctx, http.StatusOK, "success", "card status updated", cardView(fresh))
}
