// rail_switch.go — runtime selection of the NGN payment rails. The
// admin dashboard (Payment Rails card) is the single authority; the
// choices persist in Redis and survive restarts. No envs involved.
//
// Two independent knobs:
//   - float rail: which configured rail Routes B & C pay from (and,
//     for Route C, where Paycrest reloads land — the rail's own
//     deposit account).
//   - default BaaS provider: what baas.Default() is (funding console,
//     legacy flows). Switchable live among korapay|fintava; SafeHaven
//     is boot-only (its adapter needs key material main.go assembles).
package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/baas/fintava"
	"github.com/usezoracle/rails-sui/services/baas/korapay"
	db "github.com/usezoracle/rails-sui/storage"
)

const (
	floatRailKey        = "rails:float_rail"
	baasProviderKey     = "rails:baas_provider"
	fintavaFloatBankKey = "rails:fintava_float_institution"
)

func redisGet(key string) string {
	rdb := db.RedisClient
	if rdb == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return ""
	}
	return v
}

func redisSet(ctx context.Context, key, val string) error {
	if rdb := db.RedisClient; rdb != nil {
		return rdb.Set(ctx, key, val, 0).Err()
	}
	return nil
}

// CurrentFloatRail returns the operator-selected float rail:
// "korapay" | "fintava" | "default". The admin dashboard is the only
// authority; before the first switch, the in-code default applies.
func CurrentFloatRail() string {
	if v := redisGet(floatRailKey); v != "" {
		return v
	}
	return "korapay"
}

// SetFloatRail persists the operator's float-rail choice.
func SetFloatRail(ctx context.Context, rail string) error {
	return redisSet(ctx, floatRailKey, strings.ToLower(strings.TrimSpace(rail)))
}

// BaaSProviderOverride returns the operator-selected default provider
// ("" before the first switch). Read at boot by main.go.
func BaaSProviderOverride() string { return redisGet(baasProviderKey) }

// SetBaaSProvider persists the override; the caller applies
// baas.SetDefault for the live process.
func SetBaaSProvider(ctx context.Context, provider string) error {
	return redisSet(ctx, baasProviderKey, strings.ToLower(strings.TrimSpace(provider)))
}

// FintavaFloatInstitution is the Paycrest institution code of the
// bank behind the Fintava merchant wallet's NUBAN — needed to aim the
// Route C reload at it. Operator-set (admin config).
func FintavaFloatInstitution() string { return redisGet(fintavaFloatBankKey) }

// SetFintavaFloatInstitution stores the institution code.
func SetFintavaFloatInstitution(ctx context.Context, code string) error {
	return redisSet(ctx, fintavaFloatBankKey, strings.TrimSpace(code))
}

// RailConfigured reports whether the named rail has credentials.
func RailConfigured(name string) bool {
	bc := config.BaaSConfig()
	switch strings.ToLower(name) {
	case "korapay":
		return bc.KorapaySecretKey != ""
	case "fintava":
		return bc.FintavaAPIKey != ""
	case "safehaven":
		return bc.ClientID != ""
	case "default":
		return baas.Default() != nil
	default:
		return false
	}
}

// BuildRail constructs a fresh adapter for a live-switchable rail.
func BuildRail(name string) (baas.Provider, error) {
	bc := config.BaaSConfig()
	switch strings.ToLower(name) {
	case "korapay":
		if bc.KorapaySecretKey == "" {
			return nil, fmt.Errorf("korapay not configured (KORAPAY_SECRET_KEY)")
		}
		return korapay.NewAdapter(korapay.New(
			bc.KorapaySecretKey, bc.KorapayPublicKey, bc.KorapayBaseURL,
			bc.KorapayPayoutEmail, bc.KorapayVBABankCode,
		)), nil
	case "fintava":
		if bc.FintavaAPIKey == "" {
			return nil, fmt.Errorf("fintava not configured (FINTAVA_API_KEY)")
		}
		return fintava.NewAdapter(fintava.New(
			bc.FintavaAPIKey, bc.FintavaWebhookSecret, bc.FintavaBaseURL,
		)), nil
	default:
		return nil, fmt.Errorf("rail %q is not live-switchable (safehaven is boot-only via BAAS_PROVIDER)", name)
	}
}
