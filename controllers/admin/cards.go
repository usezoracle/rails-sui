package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/sui"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/controllers/cards"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
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

func cardView(ctx *gin.Context, card *ent.TappCard) gin.H {
	owner := ""
	if card.Edges.User != nil {
		owner = card.Edges.User.Email
	}
	lockedUntil := ""
	if card.LockedUntil != nil {
		lockedUntil = card.LockedUntil.Format(tsLayout)
	}

	cardMap := gin.H{
		"id":                        card.ID.String(),
		"status":                    card.Status.String(),
		"owner":                     owner,
		"pin_attempts_remaining":    card.PinAttemptsRemaining,
		"token_mismatch_count":      card.TokenMismatchCount,
		"locked_until":              lockedUntil,
		"needs_resync":              card.NeedsResync,
		"created_at":                card.CreatedAt.Format(tsLayout),
		"cap_object_id":             "",
		"coin_type":                 "",
		"on_chain_balance":          "0",
		"daily_limit_subunit":       card.DailyLimitSubunit,
		"per_tap_limit_subunit":     card.PerTapLimitSubunit,
		"step_up_threshold_subunit": card.StepUpThresholdSubunit,
		"spent_today_subunit":       card.SpentTodaySubunit,
	}

	if card.CapObjectID != nil {
		cardMap["cap_object_id"] = *card.CapObjectID
	}
	if card.CoinType != nil {
		cardMap["coin_type"] = *card.CoinType
	}

	// Query on-chain CardSpendingCap details if cap_object_id is set
	if card.CapObjectID != nil && *card.CapObjectID != "" {
		client := sui.NewSuiClient(config.OrderConfig().SuiRpcURL)
		resp, err := client.SuiGetObject(ctx, models.SuiGetObjectRequest{
			ObjectId: *card.CapObjectID,
			Options: models.SuiObjectDataOptions{
				ShowOwner:   true,
				ShowContent: true,
			},
		})
		if err == nil && resp.Data != nil && resp.Data.Content != nil && resp.Data.Content.Fields != nil {
			fields := resp.Data.Content.Fields
			// balance — Balance<T> serializes as a scalar u64 string, parsed
			// centrally so this can't silently fall through to "0" again.
			cardMap["on_chain_balance"] = cards.ParseCapBalanceField(fields)
			// limits
			if dl, ok := fields["daily_limit_subunit"]; ok {
				cardMap["daily_limit_subunit"] = parseUint64(dl)
			}
			if pl, ok := fields["per_tap_limit_subunit"]; ok {
				cardMap["per_tap_limit_subunit"] = parseUint64(pl)
			}
			if su, ok := fields["step_up_threshold_subunit"]; ok {
				cardMap["step_up_threshold_subunit"] = parseUint64(su)
			}
			if st, ok := fields["spent_today_subunit"]; ok {
				cardMap["spent_today_subunit"] = parseUint64(st)
			}
		}
	}

	return cardMap
}

// GetCards lists all cards, newest first, paginated.
// Optional filters: ?status=issued|claimed|live|revoked|locked, ?search= (card ID or owner email)
// GET /v1/admin/cards
func (c *CardOpsController) GetCards(ctx *gin.Context) {
	page, offset, limit := u.Paginate(ctx)

	q := storage.Client.TappCard.Query().WithUser()
	if status := strings.TrimSpace(ctx.Query("status")); status != "" {
		q = q.Where(tappcard.StatusEQ(tappcard.Status(status)))
	}
	if s := strings.TrimSpace(ctx.Query("search")); s != "" {
		if uid, err := uuid.Parse(s); err == nil {
			q = q.Where(tappcard.IDEQ(uid))
		} else {
			q = q.Where(tappcard.HasUserWith(userEnt.EmailContainsFold(s)))
		}
	}

	total, err := q.Clone().Count(ctx)
	if err != nil {
		logger.Errorf("admin GetCards: count: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to count cards", nil)
		return
	}
	rows, err := q.Order(ent.Desc(tappcard.FieldCreatedAt)).Offset(offset).Limit(limit).All(ctx)
	if err != nil {
		logger.Errorf("admin GetCards: query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load cards", nil)
		return
	}

	out := make([]gin.H, 0, len(rows))
	for _, card := range rows {
		out = append(out, cardView(ctx, card))
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"total": total,
		"page":  page,
		"count": len(out),
		"cards": out,
	})
}

// GetCard returns one card's operational state.
//
//	GET /v1/admin/cards/:id
func (c *CardOpsController) GetCard(ctx *gin.Context) {
	card, ok := c.load(ctx)
	if !ok {
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", cardView(ctx, card))
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
	u.APIResponse(ctx, http.StatusOK, "success", "card unlocked", cardView(ctx, fresh))
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
	u.APIResponse(ctx, http.StatusOK, "success", "card status updated", cardView(ctx, fresh))
}
