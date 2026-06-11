// settle_mode.go — the runtime switch for how card-tap orders settle.
// Operator-controlled from the admin dashboard; read at order creation.
//
//	bridge     → Route A: bridge USDC, Paycrest LP pays the merchant
//	float      → Route C: instant payout from the platform float,
//	             Paycrest reloads the float asynchronously
//	lp_network → Route B: our own LP network (selection rejected until
//	             the matching engine is wired to Route-A orders)
//
// Stored in Redis — the admin dashboard is the single authority; the
// in-code default (bridge) only applies before the first switch.
package services

import (
	"context"
	"strings"
	"time"

	db "github.com/usezoracle/rails-sui/storage"
)

const settleModeKey = "rails:settle_mode"

// Settle modes. Exposed to the admin API verbatim.
const (
	SettleModeBridge    = "bridge"
	SettleModeFloat     = "float"
	SettleModeLPNetwork = "lp_network"
)

// ValidSettleMode reports whether m is a recognised, implemented mode.
func ValidSettleMode(m string) (recognised, implemented bool) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case SettleModeBridge, SettleModeFloat, SettleModeLPNetwork:
		return true, true
	default:
		return false, false
	}
}

// CurrentSettleMode returns the operator-selected mode; bridge until
// the dashboard says otherwise.
func CurrentSettleMode(ctx context.Context) string {
	if rdb := db.RedisClient; rdb != nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if v, err := rdb.Get(cctx, settleModeKey).Result(); err == nil && v != "" {
			return v
		}
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
