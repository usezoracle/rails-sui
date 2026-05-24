// Package settlement is the Rails-side client for the off-ramp
// settlement aggregator (HTTP API + on-chain Gateway). Two pieces:
//
//   - HTTP: GET /v1/pubkey (RSA pubkey used to encrypt the recipient
//     blob), GET /v1/orders/:chainId/:orderId (lifecycle polling).
//   - Crypto: build the messageHash by JSON-encoding the recipient,
//     RSA-PKCS1v15-encrypting under the aggregator pubkey, and base64
//     encoding. The string we pass to the contract is *not* a hash
//     despite its name.
//
// The on-chain Gateway interaction lives in services/evm. The default
// upstream is api.paycrest.io but the URL is env-driven and any
// API-compatible aggregator can be plugged in via SETTLEMENT_API_URL.
package settlement

// OrderStatus mirrors the aggregator's lifecycle enum, surfaced via
// GET /v1/orders/:chainId/:orderId in the `status` field.
type OrderStatus string

const (
	StatusPending    OrderStatus = "pending"
	StatusFulfilling OrderStatus = "fulfilling"
	StatusFulfilled  OrderStatus = "fulfilled"
	StatusValidated  OrderStatus = "validated"
	StatusSettling   OrderStatus = "settling"
	StatusSettled    OrderStatus = "settled"
	StatusRefunding  OrderStatus = "refunding"
	StatusRefunded   OrderStatus = "refunded"
	StatusExpired    OrderStatus = "expired"
)

// IsTerminal reports whether the order can no longer change state
// from the aggregator's perspective. Callers should stop polling on terminal.
func (s OrderStatus) IsTerminal() bool {
	switch s {
	case StatusSettled, StatusRefunded, StatusExpired:
		return true
	}
	return false
}

// envelope is the response shape every aggregator endpoint uses.
type envelope[T any] struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// PubkeyResponse — `GET /v1/pubkey`. Data is a PEM-encoded RSA public key.
type pubkeyResponse = envelope[string]

// OrderResponse — `GET /v1/orders/:chainId/:orderId`. The shape includes
// far more than this; we project to the fields we read for state transitions.
type orderResponse = envelope[OrderInfo]

// OrderInfo is the projected subset of the aggregator's order document we
// actually consume. Add fields here as we surface them.
type OrderInfo struct {
	OrderID    string      `json:"orderId"`
	ChainID    int64       `json:"chainId"`
	Status     OrderStatus `json:"status"`
	Token      string      `json:"token"`
	Amount     string      `json:"amount"`
	Rate       string      `json:"rate"`
	TxHash     string      `json:"txHash"`
	UpdatedAt  string      `json:"updatedAt"`
}
