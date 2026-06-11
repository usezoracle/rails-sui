package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// ConfigController manages the DB-backed operational config — currencies &
// rates, tokens, networks, providers/LPs — plus a read-only view of the static
// env params. All writes are audited.
type ConfigController struct{}

// NewConfigController constructs the controller.
func NewConfigController() *ConfigController { return &ConfigController{} }

// --- Currencies & rates -----------------------------------------------------

// GetCurrencies lists fiat currencies with their market rate + enabled flag.
//
//	GET /v1/admin/config/currencies
func (c *ConfigController) GetCurrencies(ctx *gin.Context) {
	rows, err := storage.Client.FiatCurrency.Query().All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load currencies", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id": r.ID.String(), "code": r.Code, "name": r.Name, "symbol": r.Symbol,
			"market_rate": r.MarketRate.String(), "is_enabled": r.IsEnabled,
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", out)
}

type currencyPatch struct {
	MarketRate *string `json:"market_rate"`
	IsEnabled  *bool   `json:"is_enabled"`
}

// UpdateCurrency sets a currency's market rate and/or enabled flag.
//
//	PATCH /v1/admin/config/currencies/:id
func (c *ConfigController) UpdateCurrency(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be a uuid", nil)
		return
	}
	var body currencyPatch
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "invalid body", nil)
		return
	}
	upd := storage.Client.FiatCurrency.UpdateOneID(id)
	detail := map[string]any{}
	if body.MarketRate != nil {
		rate, err := decimal.NewFromString(*body.MarketRate)
		if err != nil {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "market_rate must be numeric", nil)
			return
		}
		upd.SetMarketRate(rate)
		detail["market_rate"] = rate.String()
	}
	if body.IsEnabled != nil {
		upd.SetIsEnabled(*body.IsEnabled)
		detail["is_enabled"] = *body.IsEnabled
	}
	if len(detail) == 0 {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "nothing to update", nil)
		return
	}
	if _, err := upd.Save(ctx); err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "currency not found", nil)
			return
		}
		logger.Errorf("admin UpdateCurrency %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update", nil)
		return
	}
	writeAudit(ctx, "config.currency.update", id.String(), detail)
	u.APIResponse(ctx, http.StatusOK, "success", "currency updated", detail)
}

// --- Tokens -----------------------------------------------------------------

// GetTokens lists supported tokens.
//
//	GET /v1/admin/config/tokens
func (c *ConfigController) GetTokens(ctx *gin.Context) {
	rows, err := storage.Client.Token.Query().All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load tokens", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id": r.ID, "symbol": r.Symbol, "contract_address": r.ContractAddress,
			"decimals": r.Decimals, "is_enabled": r.IsEnabled,
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", out)
}

type tokenPatch struct {
	IsEnabled *bool `json:"is_enabled"`
}

// UpdateToken toggles a token's enabled flag.
//
//	PATCH /v1/admin/config/tokens/:id
func (c *ConfigController) UpdateToken(ctx *gin.Context) {
	id, err := strconv.Atoi(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "id must be an integer", nil)
		return
	}
	var body tokenPatch
	if err := ctx.ShouldBindJSON(&body); err != nil || body.IsEnabled == nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "is_enabled required", nil)
		return
	}
	if _, err := storage.Client.Token.UpdateOneID(id).SetIsEnabled(*body.IsEnabled).Save(ctx); err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "token not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update", nil)
		return
	}
	detail := map[string]any{"is_enabled": *body.IsEnabled}
	writeAudit(ctx, "config.token.update", strconv.Itoa(id), detail)
	u.APIResponse(ctx, http.StatusOK, "success", "token updated", detail)
}

// --- Networks ---------------------------------------------------------------

// GetNetworks lists configured networks (read-only).
//
//	GET /v1/admin/config/networks
func (c *ConfigController) GetNetworks(ctx *gin.Context) {
	rows, err := storage.Client.Network.Query().All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load networks", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id": r.ID, "identifier": r.Identifier, "rpc_endpoint": r.RPCEndpoint,
			"is_testnet": r.IsTestnet, "fee": r.Fee.String(),
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", out)
}

// --- Providers / LPs --------------------------------------------------------

// GetProviders lists LPs with their activation, KYB, and the BaaS provider mapping.
//
//	GET /v1/admin/config/providers
func (c *ConfigController) GetProviders(ctx *gin.Context) {
	rows, err := storage.Client.ProviderProfile.Query().All(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load providers", nil)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id": r.ID, "trading_name": r.TradingName, "is_active": r.IsActive,
			"is_available": r.IsAvailable, "is_kyb_verified": r.IsKybVerified,
			"safehaven_account_number": r.SafehavenAccountNumber,
		})
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", out)
}

type providerPatch struct {
	IsActive               *bool   `json:"is_active"`
	IsKYBVerified          *bool   `json:"is_kyb_verified"`
	SafehavenAccountNumber *string `json:"safehaven_account_number"`
}

// UpdateProvider sets a provider's activation / KYB / the BaaS provider deposit account.
//
//	PATCH /v1/admin/config/providers/:id
func (c *ConfigController) UpdateProvider(ctx *gin.Context) {
	id := ctx.Param("id")
	var body providerPatch
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "invalid body", nil)
		return
	}
	upd := storage.Client.ProviderProfile.UpdateOneID(id)
	detail := map[string]any{}
	if body.IsActive != nil {
		upd.SetIsActive(*body.IsActive)
		detail["is_active"] = *body.IsActive
	}
	if body.IsKYBVerified != nil {
		upd.SetIsKybVerified(*body.IsKYBVerified)
		detail["is_kyb_verified"] = *body.IsKYBVerified
	}
	if body.SafehavenAccountNumber != nil {
		upd.SetSafehavenAccountNumber(*body.SafehavenAccountNumber)
		detail["safehaven_account_number"] = *body.SafehavenAccountNumber
	}
	if len(detail) == 0 {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "nothing to update", nil)
		return
	}
	if _, err := upd.Save(ctx); err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "provider not found", nil)
			return
		}
		logger.Errorf("admin UpdateProvider %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to update", nil)
		return
	}
	writeAudit(ctx, "config.provider.update", id, detail)
	u.APIResponse(ctx, http.StatusOK, "success", "provider updated", detail)
}

// --- Env params (read-only) -------------------------------------------------

// GetParams returns the static env-backed params. These need a redeploy to
// change (they're not DB-backed); shown so operators can see current values.
//
//	GET /v1/admin/config/params
func (c *ConfigController) GetParams(ctx *gin.Context) {
	conf := config.OrderConfig()
	u.APIResponse(ctx, http.StatusOK, "success", "ok (read-only — env-backed, redeploy to change)", gin.H{
		"base_sender_fee_bps":                  conf.BaseSenderFeeBPS,
		"percent_deviation_from_external_rate": conf.PercentDeviationFromExternalRate.String(),
		"percent_deviation_from_market_rate":   conf.PercentDeviationFromMarketRate.String(),
		"order_fulfillment_validity":           conf.OrderFulfillmentValidity.String(),
		"receive_address_validity":             conf.ReceiveAddressValidity.String(),
		"order_request_validity":               conf.OrderRequestValidity.String(),
		"refund_cancellation_count":            conf.RefundCancellationCount,
		"base_chain_id":                        conf.BaseChainID,
	})
}

// -----------------------------------------------------------------------------
// Settlement route switch (Route A bridge / Route B LP network / Route C float)
// -----------------------------------------------------------------------------

// GetSettleMode returns the live settlement route for card-tap orders.
//
//	GET /v1/admin/config/settle-mode
func (c *ConfigController) GetSettleMode(ctx *gin.Context) {
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"mode": services.CurrentSettleMode(ctx),
		"modes": []gin.H{
			{"value": services.SettleModeBridge, "label": "Route A — Bridge (Paycrest LP pays merchant)", "available": true},
			{"value": services.SettleModeFloat, "label": "Route C — Float (instant payout, Paycrest reloads)", "available": true},
			{"value": services.SettleModeLPNetwork, "label": "Route B — Own LPs (no bridge: LP float pays, LP receives the USDC)", "available": true},
		},
	})
}

type settleModeReq struct {
	Mode string `json:"mode" binding:"required"`
}

// SetSettleMode flips the live settlement route. Takes effect on the
// next order — no deploy, no restart. Audited.
//
//	PUT /v1/admin/config/settle-mode
func (c *ConfigController) SetSettleMode(ctx *gin.Context) {
	var req settleModeReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "mode is required", nil)
		return
	}
	recognised, implemented := services.ValidSettleMode(req.Mode)
	if !recognised {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"mode must be bridge | float | lp_network", nil)
		return
	}
	if !implemented {
		u.APIResponse(ctx, http.StatusConflict, "error",
			"that settlement mode is not available",
			gin.H{"code": "mode_not_implemented"})
		return
	}
	prev := services.CurrentSettleMode(ctx)
	if err := services.SetSettleMode(ctx, req.Mode); err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to persist mode", nil)
		return
	}
	writeAudit(ctx, "config.settle_mode", req.Mode, map[string]any{"from": prev, "to": req.Mode})
	u.APIResponse(ctx, http.StatusOK, "success", "settlement route updated", gin.H{"mode": req.Mode})
}

// -----------------------------------------------------------------------------
// Payment rails switch — default BaaS provider + Route B/C float rail
// -----------------------------------------------------------------------------

// GetRails returns the live rail configuration for the admin console.
//
//	GET /v1/admin/config/rails
func (c *ConfigController) GetRails(ctx *gin.Context) {
	current := "none"
	if p := baas.Default(); p != nil {
		current = p.Name()
	}
	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"baas_provider": current,
		"float_rail":    services.CurrentFloatRail(),
		"fintava_float_institution": services.FintavaFloatInstitution(),
		"rails": []gin.H{
			{"value": "fintava", "configured": services.RailConfigured("fintava"), "switchable": true},
			{"value": "korapay", "configured": services.RailConfigured("korapay"), "switchable": true},
			{"value": "safehaven", "configured": services.RailConfigured("safehaven"), "switchable": false, "note": "boot-only (BAAS_PROVIDER env)"},
		},
	})
}

type railsPatch struct {
	BaaSProvider            *string `json:"baas_provider"`
	FloatRail               *string `json:"float_rail"`
	FintavaFloatInstitution *string `json:"fintava_float_institution"`
}

// SetRails switches the default BaaS provider and/or the Route B/C
// float rail at runtime. Provider switches apply to the live process
// immediately AND persist (Redis) across restarts. Audited.
//
//	PUT /v1/admin/config/rails
func (c *ConfigController) SetRails(ctx *gin.Context) {
	var req railsPatch
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "invalid body", nil)
		return
	}
	applied := gin.H{}

	if req.FloatRail != nil {
		rail := strings.ToLower(strings.TrimSpace(*req.FloatRail))
		if rail != "default" && !services.RailConfigured(rail) {
			u.APIResponse(ctx, http.StatusBadRequest, "error",
				"float_rail must be a configured rail (korapay|fintava) or 'default'", nil)
			return
		}
		if err := services.SetFloatRail(ctx, rail); err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to persist float rail", nil)
			return
		}
		applied["float_rail"] = rail
	}

	if req.BaaSProvider != nil {
		name := strings.ToLower(strings.TrimSpace(*req.BaaSProvider))
		adapter, err := services.BuildRail(name)
		if err != nil {
			u.APIResponse(ctx, http.StatusBadRequest, "error", err.Error(), nil)
			return
		}
		baas.SetDefault(adapter) // live, this process
		if err := services.SetBaaSProvider(ctx, name); err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "switched live but failed to persist", nil)
			return
		}
		applied["baas_provider"] = name
	}

	if req.FintavaFloatInstitution != nil {
		code := strings.TrimSpace(*req.FintavaFloatInstitution)
		if err := services.SetFintavaFloatInstitution(ctx, code); err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to persist institution", nil)
			return
		}
		applied["fintava_float_institution"] = code
	}

	if len(applied) == 0 {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "nothing to change", nil)
		return
	}
	writeAudit(ctx, "config.rails", "payment-rails", applied)
	u.APIResponse(ctx, http.StatusOK, "success", "rails updated", applied)
}
