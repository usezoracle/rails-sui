// settle_mode.go — the runtime switch for how card-tap orders settle.
// Operator-controlled from the admin dashboard; read at order creation.
//
//	bridge     → Route A: bridge USDC, Paycrest LP pays the merchant
//	float      → Route C: instant payout from the platform float,
//	             Paycrest reloads the float asynchronously
//	lp_network → Route B: our own LP network (selection rejected until
//	             the matching engine is wired to Route-A orders)
//
// Stored in Redis so it flips instantly across instances without a
// deploy; falls back to the CARD_TAP_MODE env when Redis is empty or
// down (fail-safe to whatever ops configured at boot).
package services

import (
	"context"
	"strings"
	"time"

	"github.com/usezoracle/rails-sui/config"
	db "github.com/usezoracle/rails-sui/storage"
)

const settleModeKey = "rails:settle_mode"

// Settle modes. Exposed to the admin API verbatim.
const (
	SettleModeBridge    = "bridge"
	SettleModeFloat     = "float"
	SettleModeLPNetwork = "lp_network"
)

// ValidSettleMode reports whether m is a recognised mode and whether
// it is currently implemented (Route B selection is recognised but
// not yet wired).
func ValidSettleMode(m string) (recognised, implemented bool) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case SettleModeBridge, SettleModeFloat:
		return true, true
	case SettleModeLPNetwork:
		return true, false
	default:
		return false, false
	}
}

// CurrentSettleMode returns the operator-selected mode, falling back
// to the CARD_TAP_MODE env mapping when unset/unavailable.
func CurrentSettleMode(ctx context.Context) string {
	if rdb := db.RedisClient; rdb != nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if v, err := rdb.Get(cctx, settleModeKey).Result(); err == nil && v != "" {
			return v
		}
	}
	if strings.EqualFold(config.OrderConfig().CardTapMode, "treasury") {
		return SettleModeFloat
	}
	return SettleModeBridge
}

// SetSettleMode persists the operator's choice. Caller validates with
// ValidSettleMode and audits.
func SetSettleMode(ctx context.Context, mode string) error {
	if rdb := db.RedisClient; rdb != nil {
		return rdb.Set(ctx, settleModeKey, strings.ToLower(strings.TrimSpace(mode)), 0).Err()
	}
	return nil
}
