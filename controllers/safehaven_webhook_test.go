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

func postSafeHavenWebhook(body []byte, sig string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	var ctrl Controller
	r := gin.New()
	r.POST("/safehaven/webhook", ctrl.SafeHavenWebhook)
	req := httptest.NewRequest(http.MethodPost, "/safehaven/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Safehaven-Signature", sig)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSafeHavenWebhook_NoSecret_AcceptsValidJSON(t *testing.T) {
	old := safehavenConf.WebhookSecret
	safehavenConf.WebhookSecret = ""
	defer func() { safehavenConf.WebhookSecret = old }()

	for _, ref := range []string{"routeA-abc", "routeB-xyz", "routeC-123", "other"} {
		w := postSafeHavenWebhook([]byte(`{"paymentReference":"`+ref+`","status":"Completed"}`), "")
		assert.Equal(t, http.StatusOK, w.Code, ref)
		assert.Contains(t, w.Body.String(), "received")
	}
}

func TestSafeHavenWebhook_MalformedJSON(t *testing.T) {
	old := safehavenConf.WebhookSecret
	safehavenConf.WebhookSecret = ""
	defer func() { safehavenConf.WebhookSecret = old }()

	w := postSafeHavenWebhook([]byte(`{not json`), "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSafeHavenWebhook_SignatureGate(t *testing.T) {
	old := safehavenConf.WebhookSecret
	safehavenConf.WebhookSecret = "shhh-secret"
	defer func() { safehavenConf.WebhookSecret = old }()

	body := []byte(`{"paymentReference":"routeA-xyz","status":"Completed"}`)

	assert.Equal(t, http.StatusOK, postSafeHavenWebhook(body, hmacHex(body, "shhh-secret")).Code, "valid signature")
	assert.Equal(t, http.StatusUnauthorized, postSafeHavenWebhook(body, "deadbeef").Code, "wrong signature")
	assert.Equal(t, http.StatusUnauthorized, postSafeHavenWebhook(body, "").Code, "missing signature")
	assert.Equal(t, http.StatusUnauthorized, postSafeHavenWebhook(body, hmacHex(body, "wrong-key")).Code, "valid HMAC but wrong key")
}

func TestVerifySafeHavenSignature(t *testing.T) {
	body := []byte("payload-bytes")
	assert.True(t, verifySafeHavenSignature(body, hmacHex(body, "k"), "k"))
	assert.False(t, verifySafeHavenSignature(body, "bad", "k"))
	assert.False(t, verifySafeHavenSignature(body, "", "k"))
}
