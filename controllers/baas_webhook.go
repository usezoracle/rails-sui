package controllers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	orderpkg "github.com/usezoracle/rails-sui/services/order"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/utils/logger"
)

var baasConf = config.BaaSConfig()

// baasWebhookPayload is the BaaS provider's transfer/credit notification. Exact
// shape is to be confirmed against a live webhook — unknown keys are ignored, so
// adding fields later is non-breaking.
type baasWebhookPayload struct {
	Type             string `json:"type"`
	PaymentReference string `json:"paymentReference"`
	SessionID        string `json:"sessionId"`
	Status           string `json:"status"`
	Amount           string `json:"amount"`
	AccountNumber    string `json:"accountNumber"`
}

// BaaSWebhook ingests the BaaS provider transfer-status / credit callbacks. It
// verifies the signature (when a secret is configured), then routes by the
// paymentReference prefix to the owning settlement flow. It always ACKs 200 on a
// well-formed, authentic event so the BaaS provider stops retrying.
//
// The per-route handlers are intentionally stubs: they light up as each route is
// wired (Route A treasury dispatch, Route B/C settlement). The reference prefix
// is set by mfb.PaymentReference(prefix, orderID).
func (ctrl *Controller) BaaSWebhook(ctx *gin.Context) {
	raw, err := io.ReadAll(ctx.Request.Body)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "unreadable body"})
		return
	}

	// Signature verification (HMAC-SHA256 over the raw body). FAIL CLOSED: an
	// unsigned webhook can move order state, so outside local dev we reject when
	// no secret is configured rather than trusting the caller.
	secret := baasConf.WebhookSecret
	if secret == "" {
		if serverConf.Environment != "local" && serverConf.Environment != "" {
			logger.Errorf("[mfb] webhook REJECTED: SAFEHAVEN_WEBHOOK_SECRET not set in env=%s", serverConf.Environment)
			ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook signature not configured"})
			return
		}
		logger.Warnf("[mfb] webhook signature SKIPPED (no secret; local dev only)")
	} else {
		sig := ctx.GetHeader("X-Safehaven-Signature")
		if !verifyBaaSSignature(raw, sig, secret) {
			logger.Errorf("[mfb] webhook signature mismatch")
			ctx.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}
	}

	var p baasWebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		logger.Errorf("[mfb] webhook parse: %v", err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	// Idempotent dispatch by reference prefix.
	switch {
	case strings.HasPrefix(p.PaymentReference, "routeA-"):
		logger.Infof("[mfb] routeA transfer update ref=%s status=%s", p.PaymentReference, p.Status)
		// TODO(route-a): mark RouteAOrder settled/failed by paymentReference.
	case strings.HasPrefix(p.PaymentReference, "routeB-"), strings.HasPrefix(p.PaymentReference, "routeC-"):
		logger.Infof("[mfb] routeB/C transfer update ref=%s status=%s", p.PaymentReference, p.Status)
		applyFiatPayoutWebhook(ctx, p.PaymentReference, p.Status)
	default:
		logger.Infof("[mfb] webhook ref=%s type=%s status=%s (no handler)", p.PaymentReference, p.Type, p.Status)
	}

	ctx.JSON(http.StatusOK, gin.H{"status": "received"})
}

// applyFiatPayoutWebhook converges a Route B/C lock order's fiat payout status
// from an inbound transfer callback. Idempotent: it keys on the order id parsed
// from the reference ("routeB-<uuid>") and the matching fiat_payout_reference,
// and only writes a terminal status (pending callbacks are no-ops). A malformed
// reference or unknown status is logged and ignored so we always ACK 200.
func applyFiatPayoutWebhook(ctx *gin.Context, reference, rawStatus string) {
	idStr := reference
	idStr = strings.TrimPrefix(idStr, "routeB-")
	idStr = strings.TrimPrefix(idStr, "routeC-")
	id, err := uuid.Parse(idStr)
	if err != nil {
		logger.Warnf("[mfb] payout webhook: unparseable order id in ref=%s", reference)
		return
	}

	var status lockpaymentorder.FiatPayoutStatus
	switch strings.ToLower(strings.TrimSpace(rawStatus)) {
	case "success", "successful", "completed", "00", "approved":
		status = lockpaymentorder.FiatPayoutStatusSuccess
	case "failed", "rejected", "cancelled", "canceled", "reversed", "declined", "error":
		status = lockpaymentorder.FiatPayoutStatusFailed
	default:
		logger.Infof("[mfb] payout webhook: non-terminal status=%s ref=%s (ignored)", rawStatus, reference)
		return
	}

	upd := storage.Client.LockPaymentOrder.Update().
		Where(
			lockpaymentorder.IDEQ(id),
			lockpaymentorder.FiatPayoutReferenceEQ(reference),
		).
		SetFiatPayoutStatus(status).
		ClearFiatPayoutSessionID()
	if status == lockpaymentorder.FiatPayoutStatusSuccess {
		upd = upd.ClearFiatPayoutError()
	}
	n, err := upd.Save(ctx)
	if err != nil {
		logger.Errorf("[mfb] payout webhook: update %s: %v", id, err)
		return
	}
	if n == 0 {
		logger.Warnf("[mfb] payout webhook: no matching order for ref=%s", reference)
		return
	}

	// Fiat confirmed → release the LP's USDC (fulfil + settle). Async so the
	// webhook ACKs promptly; idempotent on the order's processing state.
	if status == lockpaymentorder.FiatPayoutStatusSuccess {
		go func() {
			if err := orderpkg.NewExecuteOrderService().SettleAfterPayout(context.Background(), id); err != nil {
				logger.Errorf("[mfb] payout webhook: settle %s: %v", id, err)
			}
		}()
	}
}

// verifyBaaSSignature checks an HMAC-SHA256 hex signature over the raw body.
func verifyBaaSSignature(body []byte, sigHex, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(sigHex)))
}
