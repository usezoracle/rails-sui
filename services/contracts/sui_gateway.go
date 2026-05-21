// Package contracts holds Go-side bindings to our on-chain Move package
// `rails-gateway`. Unlike EVM (where abigen produces bindings from an ABI),
// Sui Move packages don't have a code-gen tool — bindings are hand-written.
//
// What lives here:
//   - Stable identifiers (package address, module + function names) used when
//     building PTBs that call into the Gateway.
//   - Go structs mirroring the on-chain `Order<T>` and `Gateway` object layout
//     for decoding `sui_getObject` responses.
//   - Event type tag strings used to filter `sui_subscribeEvent` / `sui_queryEvents`.
//
// The Move package source lives in /Users/mac/rails/contracts/gateway/.
package contracts

import (
	"github.com/shopspring/decimal"
)

// ----------------------------------------------------------------------------
// Stable identifiers used when constructing PTBs and event filters.
//
// PackageID is set per environment (testnet / mainnet) via config — these
// constants are the module and function names that never change between
// environments, while the package address does.
// ----------------------------------------------------------------------------

// Module names within the Gateway package.
const (
	ModuleConfig = "config"
	ModuleOrder  = "order"
	ModuleEvents = "events"
)

// Entry functions on rails::order.
const (
	FnCreateOrder = "create_order"
	FnSettleOrder = "settle_order"
	FnRefundOrder = "refund_order"
)

// Entry functions on rails::config (admin operations).
const (
	FnAddSupportedCoin    = "add_supported_coin"
	FnRemoveSupportedCoin = "remove_supported_coin"
	FnSetProtocolFee      = "set_protocol_fee"
	FnSetTreasury         = "set_treasury"
	FnPause               = "pause"
	FnUnpause             = "unpause"
	FnMintAggregatorCap   = "mint_aggregator_cap"
)

// Event names inside rails::events. Used to build event-type tag strings of
// the form "{PackageID}::events::{EventName}" for sui_subscribeEvent filters.
const (
	EventOrderCreated          = "OrderCreated"
	EventOrderSettled          = "OrderSettled"
	EventOrderRefunded         = "OrderRefunded"
	EventSenderFeeTransferred  = "SenderFeeTransferred"
)

// Order status enum values from the Move package (rails::order).
const (
	OrderStatusPending  uint8 = 0
	OrderStatusSettled  uint8 = 1
	OrderStatusRefunded uint8 = 2
)

// EventTypeTag composes the full Move type tag for an event, given the
// deployed package ID. Used as the value passed to sui_subscribeEvent /
// sui_queryEvents filters by MoveEventType.
//
// Example: EventTypeTag("0xabc...", EventOrderCreated)
//   → "0xabc...::events::OrderCreated"
func EventTypeTag(packageID, eventName string) string {
	return packageID + "::" + ModuleEvents + "::" + eventName
}

// FunctionRef composes the fully-qualified function reference for a PTB
// MoveCall, e.g. "0xabc...::order::create_order".
func FunctionRef(packageID, module, fn string) string {
	return packageID + "::" + module + "::" + fn
}

// ----------------------------------------------------------------------------
// Object decoders — mirror the Move struct layouts so we can decode
// `sui_getObject` responses into Go values.
// ----------------------------------------------------------------------------

// Gateway is the Go view of the on-chain rails::config::Gateway shared object.
// Field names match the JSON field names returned by sui_getObject with
// "showContent: true".
type Gateway struct {
	ID              string   `json:"id"`
	SupportedCoins  []string `json:"supported_coins"`
	ProtocolFeeBps  uint64   `json:"protocol_fee_bps"`
	MaxBps          uint64   `json:"max_bps"`
	Treasury        string   `json:"treasury"`
	Paused          bool     `json:"paused"`
}

// Order is the Go view of the on-chain rails::order::Order<T> shared object.
// The phantom type parameter T (coin type) is captured separately in CoinType.
type Order struct {
	ID                 string          `json:"id"`
	CoinType           string          `json:"coin_type"` // synthesized from the object's type tag, not a Move field
	Sender             string          `json:"sender"`
	Amount             uint64          `json:"amount"`
	Rate               uint64          `json:"rate"`
	InstitutionCode    []byte          `json:"institution_code"`
	MessageHash        string          `json:"message_hash"`
	SenderFee          uint64          `json:"sender_fee"`
	SenderFeeRecipient string          `json:"sender_fee_recipient"`
	ProtocolFee        uint64          `json:"protocol_fee"`
	RefundAddress      string          `json:"refund_address"`
	EscrowValue        uint64          `json:"escrow_value"` // from Balance<T>.value
	SettledLpAmount    uint64          `json:"settled_lp_amount"`
	Status             uint8           `json:"status"`
	CreatedAtMs        uint64          `json:"created_at_ms"`
}

// LpDistributable returns the amount available for distribution to LPs
// (amount minus protocol fee and sender fee), in the coin's smallest unit.
func (o *Order) LpDistributable() uint64 {
	return o.Amount - o.ProtocolFee - o.SenderFee
}

// AmountDecimal returns Amount as a decimal scaled by `decimals` (typically
// 6 for USDC on Sui).
func (o *Order) AmountDecimal(decimals int32) decimal.Decimal {
	return decimal.NewFromInt(int64(o.Amount)).Shift(-decimals)
}
