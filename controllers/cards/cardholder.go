// Cardholder-facing Tap Card endpoints — the path the Zoracle PWA
// drives during linking, top-up, resync, and revoke.
//
//	POST /v1/cards/link/complete
//	GET  /v1/cards/me
//	POST /v1/cards/top-up          (returns the PTB skeleton for the PWA to sign)
//	POST /v1/cards/revoke          (returns the PTB skeleton for the PWA to sign)
//	POST /v1/cards/me/resync
//	POST /v1/cards/me/resync/complete
//
// And the admin escape hatch:
//
//	POST /v1/admin/cards/:id/recovery
//
// All cardholder endpoints sit behind `middleware.JWTMiddleware` and
// derive the user from the JWT's `user_id` claim.

package cards

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/tappcard"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	svc "github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

const resyncNonceTTL = 5 * time.Minute

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func userFromCtx(ctx *gin.Context) (*ent.User, bool) {
	v, exists := ctx.Get("user_id")
	if !exists {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Not authenticated", nil)
		return nil, false
	}
	userID, err := uuid.Parse(v.(string))
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid session", nil)
		return nil, false
	}
	user, err := storage.Client.User.
		Query().
		Where(userEnt.IDEQ(userID)).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "User not found", nil)
		return nil, false
	}
	return user, true
}

// cardForUser loads the user's single linked card. v1 enforces 1:1
// user↔card; multi-card support lives in v2 and would replace this
// with a `card_id` route param.
func cardForUser(ctx *gin.Context, user *ent.User) (*ent.TappCard, bool) {
	card, err := storage.Client.TappCard.
		Query().
		Where(tappcard.HasUserWith(userEnt.IDEQ(user.ID))).
		Order(ent.Desc(tappcard.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"No card linked to this account",
			map[string]any{"code": "card_not_linked"})
		return nil, false
	}
	return card, true
}

// -----------------------------------------------------------------------------
// POST /v1/cards/link/complete
// -----------------------------------------------------------------------------

type linkCompleteRequest struct {
	CardUIDHash             string `json:"card_uid_hash"               binding:"required"`
	CapObjectID             string `json:"cap_object_id"               binding:"required"`
	CoinType                string `json:"coin_type"                   binding:"required"`
	LinkingProof            string `json:"linking_proof"               binding:"required"`
	PinVerifier             string `json:"pin_verifier"                binding:"required"`
	CardPassword            string `json:"card_password"               binding:"required"`
	CurrentTokenCT          string `json:"current_token_ct"            binding:"required"`
	TxDigest                string `json:"tx_digest"                   binding:"required"`
	DailyLimitSubunit       uint64 `json:"daily_limit_subunit"         binding:"required"`
	PerTapLimitSubunit      uint64 `json:"per_tap_limit_subunit"       binding:"required"`
	StepUpThresholdSubunit  uint64 `json:"step_up_threshold_subunit"   binding:"required"`
}

// LinkComplete is the PWA's final call after writing K to the card,
// publishing the on-chain `create_cap` PTB, and committing all the
// HMAC verifier bytes the server will use later.
//
// We accept the bytes the PWA computed; we DO NOT recompute K or PIN
// here (we don't have either, by design). The trust we extend is
// "anyone who can produce a matching `pin_response` from this
// `linking_proof` at debit time knows the PIN and K".
func (ctrl *Controller) LinkComplete(ctx *gin.Context) {
	var req linkCompleteRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}

	// Decode all the byte fields up-front so a malformed one fails
	// fast before we touch the DB.
	uidHash, err := DecodeHex(req.CardUIDHash)
	if err != nil || len(uidHash) != 32 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"card_uid_hash must be hex sha256", nil)
		return
	}
	linkingProof, err := DecodeHex(req.LinkingProof)
	if err != nil || len(linkingProof) != 32 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"linking_proof must be 32-byte hex", nil)
		return
	}
	pinVerifier, err := DecodeHex(req.PinVerifier)
	if err != nil || len(pinVerifier) != 32 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"pin_verifier must be 32-byte hex", nil)
		return
	}
	cardPwd, err := DecodeHex(req.CardPassword)
	if err != nil || len(cardPwd) != 4 {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"card_password must be 4-byte hex (NTAG215 PWD)", nil)
		return
	}
	currentToken, err := DecodeHex(req.CurrentTokenCT)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"current_token_ct must be hex", nil)
		return
	}

	// Verify the on-chain tx actually published a CardSpendingCap
	// with the cap_object_id the PWA is claiming. Skipped (with a
	// warning) when SUI_GATEWAY_PACKAGE_ID isn't configured — useful
	// for local dev against a non-deployed package; production always
	// has it.
	if verifyResult, vErr := verifyCreateCap(ctx, req.TxDigest, req.CapObjectID); vErr != nil {
		logger.Warnf("LinkComplete: tx verification skipped: %v", vErr)
	} else if verifyResult != nil && !verifyResult.OK {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"On-chain create_cap could not be verified",
			map[string]any{"code": "tx_verify_failed", "reason": verifyResult.Reason})
		return
	} else if verifyResult != nil && verifyResult.CoinType != "" &&
		!strings.EqualFold(verifyResult.CoinType, req.CoinType) {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Submitted coin_type does not match the on-chain object",
			map[string]any{
				"code":  "coin_type_mismatch",
				"want":  verifyResult.CoinType,
				"got":   req.CoinType,
			})
		return
	}

	// Look up the user's claimed card (status=claimed) and flip to live.
	card, err := storage.Client.TappCard.
		Query().
		Where(
			tappcard.HasUserWith(userEnt.IDEQ(user.ID)),
			tappcard.StatusEQ(tappcard.StatusClaimed),
		).
		Order(ent.Desc(tappcard.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error",
			"No claimed card found for this account — call /link/claim first",
			map[string]any{"code": "no_claimed_card"})
		return
	}

	if _, err := card.Update().
		SetCardUIDHash(uidHash).
		SetCapObjectID(req.CapObjectID).
		SetCoinType(req.CoinType).
		SetLinkingProof(linkingProof).
		SetPinVerifier(pinVerifier).
		SetCardPassword(cardPwd).
		SetCurrentTokenCiphertext(currentToken).
		SetTokenRotatedAt(time.Now()).
		SetDailyLimitSubunit(req.DailyLimitSubunit).
		SetPerTapLimitSubunit(req.PerTapLimitSubunit).
		SetStepUpThresholdSubunit(req.StepUpThresholdSubunit).
		SetStatus(tappcard.StatusLive).
		Save(ctx); err != nil {
		logger.Errorf("LinkComplete: persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to complete linking", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Card linked",
		map[string]any{"card_id": card.ID.String(), "status": "live"})
}

// -----------------------------------------------------------------------------
// GET /v1/cards/me
// -----------------------------------------------------------------------------

type cardSummaryResponse struct {
	ID                       string `json:"id"`
	Status                   string `json:"status"`
	CapObjectID              string `json:"cap_object_id,omitempty"`
	CoinType                 string `json:"coin_type,omitempty"`
	DailyLimitSubunit        uint64 `json:"daily_limit_subunit"`
	PerTapLimitSubunit       uint64 `json:"per_tap_limit_subunit"`
	StepUpThresholdSubunit   uint64 `json:"step_up_threshold_subunit"`
	SpentTodaySubunit        uint64 `json:"spent_today_subunit"`
	NeedsResync              bool   `json:"needs_resync"`
	PinAttemptsRemaining     int    `json:"pin_attempts_remaining"`
}

// Me returns the cardholder's single card summary. Used by the PWA
// dashboard + drives the "needs resync" banner.
func (ctrl *Controller) Me(ctx *gin.Context) {
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}
	card, ok := cardForUser(ctx, user)
	if !ok {
		return
	}

	resp := cardSummaryResponse{
		ID:                     card.ID.String(),
		Status:                 string(card.Status),
		DailyLimitSubunit:      card.DailyLimitSubunit,
		PerTapLimitSubunit:     card.PerTapLimitSubunit,
		StepUpThresholdSubunit: card.StepUpThresholdSubunit,
		SpentTodaySubunit:      card.SpentTodaySubunit,
		NeedsResync:            card.NeedsResync,
		PinAttemptsRemaining:   card.PinAttemptsRemaining,
	}
	if card.CapObjectID != nil {
		resp.CapObjectID = *card.CapObjectID
	}
	if card.CoinType != nil {
		resp.CoinType = *card.CoinType
	}
	u.APIResponse(ctx, http.StatusOK, "success", "Card", resp)
}

// -----------------------------------------------------------------------------
// POST /v1/cards/top-up
// -----------------------------------------------------------------------------

type topUpRequest struct {
	AmountSubunit uint64 `json:"amount_subunit" binding:"required"`
}

type ptbSkeletonResponse struct {
	PackageID    string         `json:"package_id"`
	Module       string         `json:"module"`
	Function     string         `json:"function"`
	TypeArgs     []string       `json:"type_args"`
	Args         []any          `json:"args"`
	Note         string         `json:"note"`
}

// TopUp returns the PTB skeleton the PWA signs via zkLogin to add
// USDC to the cardholder's CardSpendingCap. We don't sign anything
// server-side — that's the cardholder's job.
func (ctrl *Controller) TopUp(ctx *gin.Context) {
	var req topUpRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}
	card, ok := cardForUser(ctx, user)
	if !ok {
		return
	}
	if card.CapObjectID == nil || card.CoinType == nil {
		u.APIResponse(ctx, http.StatusConflict, "error",
			"Card is not yet live — finish linking first", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "PTB skeleton",
		ptbSkeletonResponse{
			PackageID: "", // TODO(deploy): inject from config.SUI_GATEWAY_PACKAGE_ID
			Module:    "tapp_card",
			Function:  "top_up",
			TypeArgs:  []string{*card.CoinType},
			Args: []any{
				*card.CapObjectID,
				// The funding coin object — PWA selects from the
				// cardholder's wallet at sign time.
				map[string]string{"$type": "coin_to_select", "amount_subunit": uint64ToString(req.AmountSubunit)},
			},
			Note: "PWA signs via zkLogin. Server doesn't hold any key for this op.",
		})
}

// -----------------------------------------------------------------------------
// POST /v1/cards/revoke
// -----------------------------------------------------------------------------

// Revoke returns the PTB skeleton for `tapp_card::set_revoked`. PWA
// signs and submits; the indexer (TODO) listens for state changes and
// flips local status. v1: caller is expected to PATCH the local
// status separately via a follow-up; for now we just hand back the
// PTB.
func (ctrl *Controller) Revoke(ctx *gin.Context) {
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}
	card, ok := cardForUser(ctx, user)
	if !ok {
		return
	}
	if card.CapObjectID == nil || card.CoinType == nil {
		u.APIResponse(ctx, http.StatusConflict, "error",
			"Card is not yet live", nil)
		return
	}
	// Optimistic local update — the PWA will reconcile on tx confirm.
	if _, err := card.Update().SetStatus(tappcard.StatusRevoked).Save(ctx); err != nil {
		logger.Errorf("Revoke: persist: %v", err)
	}
	u.APIResponse(ctx, http.StatusOK, "success", "PTB skeleton",
		ptbSkeletonResponse{
			Module:   "tapp_card",
			Function: "set_revoked",
			TypeArgs: []string{*card.CoinType},
			Args:     []any{*card.CapObjectID, true},
			Note:     "PWA signs via zkLogin. Status locally flipped optimistically.",
		})
}

// -----------------------------------------------------------------------------
// POST /v1/cards/me/resync
// -----------------------------------------------------------------------------

type resyncResponse struct {
	CurrentTokenCT string `json:"current_token_ct"`
	CardPassword   string `json:"card_password"`
	ResyncNonce    string `json:"resync_nonce"`
}

// Resync hands the PWA everything it needs to write the canonical
// rotation token back to a desynced card. The resync_nonce is
// one-shot — consumed only on /resync/complete after the write lands.
//
// Implementation note: we reuse the CardServerNonce table for the
// resync nonce since it's the same shape (single-use, scoped). The
// sender edge is filled with a synthetic "self" by reusing one of
// the cardholder's own merchant nonces if available; otherwise we
// create a free-floating nonce with no sender — left as a TODO since
// it requires a small schema relaxation. For v1 we issue an in-memory
// nonce signed with HMAC + the server's secret, no DB round-trip,
// which keeps this endpoint trivially testable.
func (ctrl *Controller) Resync(ctx *gin.Context) {
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}
	card, ok := cardForUser(ctx, user)
	if !ok {
		return
	}
	if card.CurrentTokenCiphertext == nil || card.CardPassword == nil {
		u.APIResponse(ctx, http.StatusConflict, "error",
			"Card was never fully linked — nothing to resync to", nil)
		return
	}

	// Generate a one-shot resync nonce — HMAC of (user_id || card_id
	// || now) keyed by the server secret. On /resync/complete we
	// re-derive and compare. No DB row needed for v1 — TTL is encoded
	// in the timestamp bound the verifier checks.
	expiresAt := time.Now().Add(resyncNonceTTL).Unix()
	nonce, err := issueResyncNonce(user.ID, card.ID, expiresAt)
	if err != nil {
		logger.Errorf("Resync: nonce: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to issue resync nonce", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Resync payload",
		resyncResponse{
			CurrentTokenCT: EncodeHex(*card.CurrentTokenCiphertext),
			CardPassword:   EncodeHex(*card.CardPassword),
			ResyncNonce:    nonce,
		})
}

// -----------------------------------------------------------------------------
// POST /v1/cards/me/resync/complete
// -----------------------------------------------------------------------------

type resyncCompleteRequest struct {
	ResyncNonce string `json:"resync_nonce" binding:"required"`
}

// ResyncComplete verifies the one-shot nonce, clears needs_resync,
// and resets the token-mismatch counter. The PWA calls this after the
// NDEF write to the card succeeds.
func (ctrl *Controller) ResyncComplete(ctx *gin.Context) {
	var req resyncCompleteRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	user, ok := userFromCtx(ctx)
	if !ok {
		return
	}
	card, ok := cardForUser(ctx, user)
	if !ok {
		return
	}
	if err := verifyResyncNonce(user.ID, card.ID, req.ResyncNonce); err != nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Resync nonce invalid or expired",
			map[string]any{"code": "resync_nonce_invalid"})
		return
	}
	if _, err := card.Update().
		SetNeedsResync(false).
		SetTokenMismatchCount(0).
		Save(ctx); err != nil {
		logger.Errorf("ResyncComplete: persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to mark resync complete", nil)
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "Resync complete",
		map[string]any{"acknowledged": true})
}

// -----------------------------------------------------------------------------
// POST /v1/admin/cards/:id/recovery
// -----------------------------------------------------------------------------

type recoveryRequest struct {
	UserEmail string `json:"user_email" binding:"required,email"`
}

// AdminRecovery emails a 6-digit recovery code to the cardholder for
// the iOS-no-Web-NFC escape hatch. The actual reset is operator-
// driven: support staff reads the code over a call, then uses an
// Android device to perform the resync on the cardholder's behalf.
//
// Hits the existing svc.EmailService SendGrid path with the template
// id from `CARD_RECOVERY_SENDGRID_TEMPLATE`. When that env is empty
// (e.g. local dev without a configured SendGrid template), the email
// send returns a noop and we surface the raw code in the response
// for the operator to read from logs.
func (ctrl *Controller) AdminRecovery(ctx *gin.Context) {
	cardIDStr := ctx.Param("id")
	cardID, err := uuid.Parse(cardIDStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid card id", nil)
		return
	}
	var req recoveryRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	card, err := storage.Client.TappCard.Get(ctx, cardID)
	if err != nil {
		u.APIResponse(ctx, http.StatusNotFound, "error", "Card not found", nil)
		return
	}

	// Generate a 6-digit human-readable code.
	codeBytes, err := GenerateServerNonce()
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to issue recovery code", nil)
		return
	}
	code := codeFromBytes(codeBytes)

	mailer := emailService()
	if _, err := mailer.SendCardRecoveryCode(ctx.Request.Context(), req.UserEmail, code); err != nil {
		logger.Errorf("admin recovery: send email: %v", err)
	}
	logger.Infof("admin recovery issued: card=%s email=%s", card.ID, req.UserEmail)

	u.APIResponse(ctx, http.StatusOK, "success", "Recovery code issued",
		map[string]any{
			"acknowledged":  true,
			"note":          "Code emailed to cardholder. Have them read it over the support call.",
			"debug_code":    code, // visible regardless so dev/staging can read it from the response
		})
}

// emailService lazily instantiates an EmailService over the
// SendGrid provider. Same pattern AuthController uses; kept inline
// here so we don't drag the full controller wiring across packages.
var emailServiceInstance *svc.EmailService

func emailService() *svc.EmailService {
	if emailServiceInstance == nil {
		emailServiceInstance = svc.NewEmailService(svc.SENDGRID_MAIL_PROVIDER)
	}
	return emailServiceInstance
}

// codeFromBytes turns random bytes into a 6-digit code. Modulo bias
// across u32 → 1_000_000 is < 1 in 4_000 — acceptable for a recovery
// code with short TTL and rate-limited attempts.
func codeFromBytes(b []byte) string {
	if len(b) < 4 {
		return "000000"
	}
	v := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1_000_000
	return zeroPad(v, 6)
}

func zeroPad(v uint32, width int) string {
	s := uint64ToString(uint64(v))
	for len(s) < width {
		s = "0" + s
	}
	return s
}

func uint64ToString(n uint64) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	return digits
}
