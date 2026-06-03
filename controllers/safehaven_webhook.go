package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/utils/logger"
)

var safehavenConf = config.SafehavenConfig()

// safeHavenWebhookPayload is Safe Haven's transfer/credit notification. Exact
// shape is to be confirmed against a live webhook — unknown keys are ignored, so
// adding fields later is non-breaking.
type safeHavenWebhookPayload struct {
	Type             string `json:"type"`
	PaymentReference string `json:"paymentReference"`
	SessionID        string `json:"sessionId"`
	Status           string `json:"status"`
	Amount           string `json:"amount"`
	AccountNumber    string `json:"accountNumber"`
}

// SafeHavenWebhook ingests Safe Haven transfer-status / credit callbacks. It
// verifies the signature (when a secret is configured), then routes by the
// paymentReference prefix to the owning settlement flow. It always ACKs 200 on a
// well-formed, authentic event so Safe Haven stops retrying.
//
// The per-route handlers are intentionally stubs: they light up as each route is
// wired (Route A treasury dispatch, Route B/C settlement). The reference prefix
// is set by safehaven.PaymentReference(prefix, orderID).
func (ctrl *Controller) SafeHavenWebhook(ctx *gin.Context) {
	raw, err := io.ReadAll(ctx.Request.Body)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "unreadable body"})
		return
	}

	// Signature verification. Header name + scheme are best-effort (HMAC-SHA256
	// over the raw body) until confirmed with Safe Haven; skipped when no secret
	// is configured so local dev still works.
	if secret := safehavenConf.WebhookSecret; secret != "" {
		sig := ctx.GetHeader("X-Safehaven-Signature")
		if !verifySafeHavenSignature(raw, sig, secret) {
			logger.Errorf("[safehaven] webhook signature mismatch")
			ctx.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}
	}

	var p safeHavenWebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		logger.Errorf("[safehaven] webhook parse: %v", err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	// Idempotent dispatch by reference prefix.
	switch {
	case strings.HasPrefix(p.PaymentReference, "routeA-"):
		logger.Infof("[safehaven] routeA transfer update ref=%s status=%s", p.PaymentReference, p.Status)
		// TODO(route-a): mark RouteAOrder settled/failed by paymentReference.
	case strings.HasPrefix(p.PaymentReference, "routeB-"), strings.HasPrefix(p.PaymentReference, "routeC-"):
		logger.Infof("[safehaven] routeB/C transfer update ref=%s status=%s", p.PaymentReference, p.Status)
		// TODO(route-b/c): mark LockPaymentOrder / payout record by paymentReference.
	default:
		logger.Infof("[safehaven] webhook ref=%s type=%s status=%s (no handler)", p.PaymentReference, p.Type, p.Status)
	}

	ctx.JSON(http.StatusOK, gin.H{"status": "received"})
}

// verifySafeHavenSignature checks an HMAC-SHA256 hex signature over the raw body.
func verifySafeHavenSignature(body []byte, sigHex, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(sigHex)))
}
