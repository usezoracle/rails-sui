package utils

import (
	"strings"

	"github.com/google/uuid"
	"github.com/usezoracle/rails-sui/config"
)

// BuildCheckoutURL returns the public Zoracle checkout URL the payer
// device should be redirected to (via NFC NDEF, QR, or share-sheet link)
// for a given PaymentOrder.
//
// Tapp Merchant embeds this URL in an NDEF Type 4 Tag on Android HCE
// or renders it as a QR on iOS. Either way, the payer's phone opens it
// in their browser, where zkLogin signs a PTB calling
// `rails::order::create_order`.
//
// Source of truth: the CHECKOUT_BASE_URL env var
// (default: https://checkout.zoracle.com). The trailing slash is
// normalized so callers don't have to be careful.
func BuildCheckoutURL(orderID uuid.UUID) string {
	base := strings.TrimRight(config.ServerConfig().CheckoutBaseURL, "/")
	return base + "/order/" + orderID.String()
}
