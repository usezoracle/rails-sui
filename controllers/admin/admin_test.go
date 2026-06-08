package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// buildAdminRouter wires the admin console routes alongside stubs for the
// pre-existing admin routes, on one engine — so gin's radix tree is exercised
// exactly as in routers/index.go. A path conflict panics here.
func buildAdminRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	noop := func(c *gin.Context) { c.Status(http.StatusOK) }

	// pre-existing admin route shapes (stubbed)
	r.GET("/v1/admin/route-a/orders/:id/events", noop)
	r.POST("/v1/admin/route-a/orders/:id/force-state", noop)
	r.POST("/v1/admin/cards/:id/recovery", noop)

	// card-ops routes share the /v1/admin/cards/:id prefix — register them on
	// the same engine so a path conflict would panic this build.
	cardOps := NewCardOpsController()
	r.GET("/v1/admin/cards/:id", cardOps.GetCard)
	r.POST("/v1/admin/cards/:id/unlock", cardOps.Unlock)
	r.POST("/v1/admin/cards/:id/status", cardOps.SetStatus)

	// the console under test
	g := r.Group("/v1/admin/")
	tx := NewTransactionsController()
	g.GET("transactions", tx.GetTransactions)
	g.GET("transactions/:id", tx.GetTransactionTimeline)
	fund := NewFundingController()
	g.GET("funding/balances", fund.GetBalances)
	g.POST("funding/transfer", fund.Transfer)
	cfg := NewConfigController()
	g.GET("config/currencies", cfg.GetCurrencies)
	g.PATCH("config/currencies/:id", cfg.UpdateCurrency)
	g.GET("config/tokens", cfg.GetTokens)
	g.PATCH("config/tokens/:id", cfg.UpdateToken)
	g.GET("config/networks", cfg.GetNetworks)
	g.GET("config/providers", cfg.GetProviders)
	g.PATCH("config/providers/:id", cfg.UpdateProvider)
	g.GET("config/params", cfg.GetParams)
	refund := NewRefundController()
	g.POST("orders/:id/refund", refund.RefundOrder)

	webhook := NewWebhooksController()
	g.GET("webhooks", webhook.GetWebhookAttempts)
	g.POST("webhooks/:id/retry", webhook.RetryWebhook)
	dep := NewDepositAddressController()
	g.GET("deposit-addresses/:address", dep.GetAddress)
	g.POST("deposit-addresses/:address/extend", dep.ExtendAddress)
	return r
}

// TestAdminRoutesRegister fails if gin detects a path conflict (would otherwise
// panic the server at startup).
func TestAdminRoutesRegister(t *testing.T) {
	assert.NotPanics(t, func() { buildAdminRouter() })
}

func do(r *gin.Engine, method, path string, body []byte) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGetParams returns the env-backed params (no DB needed).
func TestGetParams(t *testing.T) {
	w := do(buildAdminRouter(), http.MethodGet, "/v1/admin/config/params", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "base_sender_fee_bps")
}

// TestFundingTransfer_Gates: bad amount → 400; valid body but the BaaS provider not
// configured → 503 (the gate runs before any money moves).
func TestFundingTransfer_Gates(t *testing.T) {
	r := buildAdminRouter()

	bad := do(r, http.MethodPost, "/v1/admin/funding/transfer",
		[]byte(`{"beneficiary_bank_code":"090286","beneficiary_account":"1","amount":"-5","reference":"x"}`))
	assert.Equal(t, http.StatusBadRequest, bad.Code)

	// the BaaS provider is unconfigured in the test binary (Default() == nil).
	unconfigured := do(r, http.MethodPost, "/v1/admin/funding/transfer",
		[]byte(`{"beneficiary_bank_code":"090286","beneficiary_account":"1234567890","amount":"100","reference":"x"}`))
	assert.Equal(t, http.StatusServiceUnavailable, unconfigured.Code)
}

// TestFundingTransfer_AmountCap: an amount above the configured max is rejected
// before any money path (default cap 1,000,000 NGN).
func TestFundingTransfer_AmountCap(t *testing.T) {
	w := do(buildAdminRouter(), http.MethodPost, "/v1/admin/funding/transfer",
		[]byte(`{"beneficiary_bank_code":"090286","beneficiary_account":"1234567890","amount":"2000000","reference":"x"}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "exceeds max")
}

// TestRefund_BadUUID is rejected before any DB access.
func TestRefund_BadUUID(t *testing.T) {
	w := do(buildAdminRouter(), http.MethodPost, "/v1/admin/orders/not-a-uuid/refund",
		[]byte(`{"justification":"test","confirm":false}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestCardOps_BadUUID: card-ops handlers parse the id before any DB access.
func TestCardOps_BadUUID(t *testing.T) {
	r := buildAdminRouter()
	for _, p := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/v1/admin/cards/not-a-uuid", nil},
		{http.MethodPost, "/v1/admin/cards/not-a-uuid/unlock", nil},
		{http.MethodPost, "/v1/admin/cards/not-a-uuid/status", []byte(`{"status":"revoked"}`)},
	} {
		w := do(r, p.method, p.path, p.body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s %s", p.method, p.path)
	}
}

// TestWebhookRetry_BadID: the attempt id is an int; non-numeric → 400 pre-DB.
func TestWebhookRetry_BadID(t *testing.T) {
	w := do(buildAdminRouter(), http.MethodPost, "/v1/admin/webhooks/not-an-int/retry", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
