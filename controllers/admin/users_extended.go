package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/sui"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/controllers/cards"
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

// GetUser returns a single user with profile presence, KYC status, order
// count, Sui wallet balances, and active card details.
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

	cardList := make([]gin.H, 0, len(user.Edges.TappCards))
	var userSuiAddress string
	var userSuiBalance string
	var userUsdcBalance string

	client := sui.NewSuiClient(config.OrderConfig().SuiRpcURL)

	for _, card := range user.Edges.TappCards {
		cardMap := gin.H{
			"id":                         card.ID.String(),
			"status":                     card.Status.String(),
			"needs_resync":               card.NeedsResync,
			"pin_attempts_remaining":     card.PinAttemptsRemaining,
			"token_mismatch_count":       card.TokenMismatchCount,
			"created_at":                 card.CreatedAt.Format(tsLayout),
			"cap_object_id":              "",
			"coin_type":                  "",
			"on_chain_balance":           "0",
			"daily_limit_subunit":        card.DailyLimitSubunit,
			"per_tap_limit_subunit":      card.PerTapLimitSubunit,
			"step_up_threshold_subunit":  card.StepUpThresholdSubunit,
			"spent_today_subunit":        card.SpentTodaySubunit,
		}

		if card.CapObjectID != nil {
			cardMap["cap_object_id"] = *card.CapObjectID
		}
		if card.CoinType != nil {
			cardMap["coin_type"] = *card.CoinType
		}

		// Query on-chain CardSpendingCap details if cap_object_id is set
		if card.CapObjectID != nil && *card.CapObjectID != "" {
			resp, err := client.SuiGetObject(ctx, models.SuiGetObjectRequest{
				ObjectId: *card.CapObjectID,
				Options: models.SuiObjectDataOptions{
					ShowOwner:   true,
					ShowContent: true,
				},
			})
			if err == nil && resp.Data != nil {
				// Parse owner
				if ownerMap, ok := resp.Data.Owner.(map[string]any); ok {
					if addr, ok := ownerMap["AddressOwner"].(string); ok {
						userSuiAddress = addr
					}
				}

				// Parse on-chain balance and limits from fields
				if resp.Data.Content != nil && resp.Data.Content.Fields != nil {
					fields := resp.Data.Content.Fields
					// balance — scalar u64; parsed centrally (see cards pkg).
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
		}
		cardList = append(cardList, cardMap)
	}

	// If we resolved the user's Sui address, fetch SUI and USDC balances
	if userSuiAddress != "" {
		suiBal, err := client.SuiXGetBalance(ctx, models.SuiXGetBalanceRequest{
			Owner:    userSuiAddress,
			CoinType: "0x2::sui::SUI",
		})
		if err == nil {
			userSuiBalance = suiBal.TotalBalance
		}

		usdcCoinType := "0x5d4b302506645c37ff133b98c4b50a5ae14841659738d6d733d59d0d217a93bf::usdc::USDC"
		for _, card := range user.Edges.TappCards {
			if card.CoinType != nil && *card.CoinType != "" {
				usdcCoinType = *card.CoinType
				break
			}
		}
		usdcBal, err := client.SuiXGetBalance(ctx, models.SuiXGetBalanceRequest{
			Owner:    userSuiAddress,
			CoinType: usdcCoinType,
		})
		if err == nil {
			userUsdcBalance = usdcBal.TotalBalance
		}
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
		"sui_address":       userSuiAddress,
		"sui_balance":       userSuiBalance,
		"usdc_balance":      userUsdcBalance,
		"cards":             cardList,
	})
}

func parseUint64(val any) uint64 {
	switch v := val.(type) {
	case string:
		u, _ := strconv.ParseUint(v, 10, 64)
		return u
	case float64:
		return uint64(v)
	case int:
		return uint64(v)
	case int64:
		return uint64(v)
	case uint64:
		return v
	}
	return 0
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
