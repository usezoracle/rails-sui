package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// DepositAddressController lets operators inspect a Sui receive address and
// override its expiry — the common un-stick when a deposit lands just after
// valid_until passes (address flips to expired before the watcher forwards).
//
// Note: this only moves DB state (expiry window + status). It does NOT sweep or
// forward funds; a deposited-but-not-forwarded address needs the manual-forward
// sweep tool (decrypts the seed and signs a transfer), which is a separate,
// dry-run-gated money operation.
type DepositAddressController struct{}

// NewDepositAddressController constructs the controller.
func NewDepositAddressController() *DepositAddressController {
	return &DepositAddressController{}
}

func (c *DepositAddressController) load(ctx *gin.Context) (*ent.SuiReceiveAddress, bool) {
	addr := strings.TrimSpace(ctx.Param("address"))
	row, err := storage.Client.SuiReceiveAddress.Query().
		Where(suireceiveaddress.AddressEQ(addr)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "deposit address not found", nil)
			return nil, false
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load address", nil)
		return nil, false
	}
	return row, true
}

func depositAddrView(r *ent.SuiReceiveAddress) gin.H {
	return gin.H{
		"address":           r.Address,
		"status":            r.Status.String(),
		"coin_type":         r.CoinType,
		"expected_amount":   r.ExpectedAmount,
		"valid_until":       r.ValidUntil.Format(tsLayout),
		"deposit_tx_digest": r.DepositTxDigest,
		"forward_tx_digest": r.ForwardTxDigest,
	}
}

// GetAddress returns one Sui receive address's state.
//
//	GET /v1/admin/deposit-addresses/:address
func (c *DepositAddressController) GetAddress(ctx *gin.Context) {
	row, ok := c.load(ctx)
	if !ok {
		return
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", depositAddrView(row))
}

type extendReq struct {
	// ExtendMinutes pushes valid_until to now + this many minutes (default 60).
	ExtendMinutes int `json:"extend_minutes"`
	// Reactivate flips an expired address back to unused so the watcher
	// re-indexes it. Only valid from the expired state.
	Reactivate bool `json:"reactivate"`
}

// ExtendAddress pushes a receive address's expiry out and optionally reactivates
// an expired one. DB-only — no funds move.
//
//	POST /v1/admin/deposit-addresses/:address/extend
func (c *DepositAddressController) ExtendAddress(ctx *gin.Context) {
	row, ok := c.load(ctx)
	if !ok {
		return
	}
	var body extendReq
	_ = ctx.ShouldBindJSON(&body)
	mins := body.ExtendMinutes
	if mins <= 0 {
		mins = 60
	}
	newValidUntil := time.Now().Add(time.Duration(mins) * time.Minute)

	upd := storage.Client.SuiReceiveAddress.UpdateOne(row).SetValidUntil(newValidUntil)
	detail := map[string]any{
		"previous_status":      row.Status.String(),
		"previous_valid_until": row.ValidUntil.Format(tsLayout),
		"new_valid_until":      newValidUntil.Format(tsLayout),
	}
	if body.Reactivate {
		if row.Status != suireceiveaddress.StatusExpired {
			u.APIResponse(ctx, http.StatusConflict, "error", "reactivate only valid from expired (current: "+row.Status.String()+")", nil)
			return
		}
		upd = upd.SetStatus(suireceiveaddress.StatusUnused)
		detail["reactivated"] = true
	}
	if _, err := upd.Save(ctx); err != nil {
		logger.Errorf("admin deposit-address extend %s: %v", row.Address, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update address", nil)
		return
	}
	writeAudit(ctx, "deposit_address.extend", row.Address, detail)
	fresh, _ := c.load(ctx)
	u.APIResponse(ctx, http.StatusOK, "success", "address updated", depositAddrView(fresh))
}
