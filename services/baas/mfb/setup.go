package mfb

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNotConfigured indicates the BaaS provider credentials are absent. Callers should
// treat the fiat rail as unavailable (and fail the specific route) rather than
// crash the whole process — Route A `mode=lp` and other flows don't need it.
var ErrNotConfigured = errors.New("mfb: not configured (set SAFEHAVEN_CLIENT_ID + SAFEHAVEN_PRIVATE_KEY)")

// NewClientFromCredentials builds a Client from raw credentials. Returns
// ErrNotConfigured when clientID or privateKeyPEM is empty; any other error
// means the credentials are present but invalid (e.g. unparseable key) and
// should fail fast at startup. Kept config-free so the package stays hermetic
// and unit-testable — the composition root (main.go) reads app config and calls
// this.
func NewClientFromCredentials(clientID, privateKeyPEM, baseURL, audience, issuer string) (*Client, error) {
	if clientID == "" || privateKeyPEM == "" {
		return nil, ErrNotConfigured
	}
	auth, err := NewAuthenticator(Config{
		ClientID:      clientID,
		PrivateKeyPEM: privateKeyPEM,
		BaseURL:       baseURL,
		Audience:      audience,
		Issuer:        issuer,
	})
	if err != nil {
		return nil, err
	}
	return NewClient(auth), nil
}

var (
	defaultMu     sync.RWMutex
	defaultClient *Client
)

// SetDefault registers the process-wide Client. Called once from main after
// building the client from config.
func SetDefault(c *Client) {
	defaultMu.Lock()
	defaultClient = c
	defaultMu.Unlock()
}

// Default returns the process-wide Client, or nil if the rail was never
// configured. Consumers must nil-check and fail their own route gracefully.
func Default() *Client {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultClient
}

// PaymentReference builds a deterministic, idempotent transfer reference from a
// route prefix and an order id, so a retried payout reuses the same reference
// and the BaaS provider rejects the duplicate instead of double-paying.
//
//	PaymentReference("routeA", orderID) -> "routeA-<orderID>"
func PaymentReference(prefix, orderID string) string {
	return fmt.Sprintf("%s-%s", prefix, orderID)
}
