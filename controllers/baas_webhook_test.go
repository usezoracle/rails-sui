package controllers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func hmacHex(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func postBaaSWebhook(body []byte, sig string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	var ctrl Controller
	r := gin.New()
	r.POST("/safehaven/webhook", ctrl.BaaSWebhook)
	req := httptest.NewRequest(http.MethodPost, "/safehaven/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Safehaven-Signature", sig)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestBaaSWebhook_NoSecret_AcceptsValidJSON(t *testing.T) {
	old := baasConf.WebhookSecret
	baasConf.WebhookSecret = ""
	defer func() { baasConf.WebhookSecret = old }()

	for _, ref := range []string{"routeA-abc", "routeB-xyz", "routeC-123", "other"} {
		w := postBaaSWebhook([]byte(`{"paymentReference":"`+ref+`","status":"Completed"}`), "")
		assert.Equal(t, http.StatusOK, w.Code, ref)
		assert.Contains(t, w.Body.String(), "received")
	}
}

func TestBaaSWebhook_MalformedJSON(t *testing.T) {
	old := baasConf.WebhookSecret
	baasConf.WebhookSecret = ""
	defer func() { baasConf.WebhookSecret = old }()

	w := postBaaSWebhook([]byte(`{not json`), "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBaaSWebhook_SignatureGate(t *testing.T) {
	old := baasConf.WebhookSecret
	baasConf.WebhookSecret = "shhh-secret"
	defer func() { baasConf.WebhookSecret = old }()

	body := []byte(`{"paymentReference":"routeA-xyz","status":"Completed"}`)

	assert.Equal(t, http.StatusOK, postBaaSWebhook(body, hmacHex(body, "shhh-secret")).Code, "valid signature")
	assert.Equal(t, http.StatusUnauthorized, postBaaSWebhook(body, "deadbeef").Code, "wrong signature")
	assert.Equal(t, http.StatusUnauthorized, postBaaSWebhook(body, "").Code, "missing signature")
	assert.Equal(t, http.StatusUnauthorized, postBaaSWebhook(body, hmacHex(body, "wrong-key")).Code, "valid HMAC but wrong key")
}

// TestBaaSWebhook_FailsClosedInProd: no secret + non-local env → 503
// (an unsigned webhook must never be accepted in production).
func TestBaaSWebhook_FailsClosedInProd(t *testing.T) {
	oldSecret, oldEnv := baasConf.WebhookSecret, serverConf.Environment
	baasConf.WebhookSecret = ""
	serverConf.Environment = "production"
	defer func() { baasConf.WebhookSecret = oldSecret; serverConf.Environment = oldEnv }()

	w := postBaaSWebhook([]byte(`{"paymentReference":"routeA-x","status":"Completed"}`), "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestVerifyBaaSSignature(t *testing.T) {
	body := []byte("payload-bytes")
	assert.True(t, verifyBaaSSignature(body, hmacHex(body, "k"), "k"))
	assert.False(t, verifyBaaSSignature(body, "bad", "k"))
	assert.False(t, verifyBaaSSignature(body, "", "k"))
}
