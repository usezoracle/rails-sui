package config

import (
	"github.com/shopspring/decimal"
	"github.com/spf13/viper"
)

// SafehavenConfiguration holds credentials for the Safe Haven MFB BaaS rail —
// the NGN fiat payout used by Route C (managed liquidity) and Route A's
// treasury mode. See services/baas/safehaven.
type SafehavenConfiguration struct {
	// ClientID is the OAuth Client ID from the Safe Haven dashboard.
	ClientID string
	// PrivateKeyPEM is the RSA private key whose public cert is registered on
	// the dashboard. Store as a secret; "\n" escapes are tolerated.
	PrivateKeyPEM string
	// BaseURL is the API root (prod by default).
	BaseURL string
	// Audience overrides the JWT aud claim; empty → BaseURL.
	Audience string
	// Issuer overrides the JWT iss claim; empty → ClientID.
	Issuer string
	// DebitAccountNumber is our funded Safe Haven account that payouts debit.
	DebitAccountNumber string
	// WebhookSecret verifies inbound Safe Haven transfer/credit callbacks. When
	// empty, signature verification is skipped (dev only).
	WebhookSecret string
	// MaxTransferNGN caps a single admin payout to guard against fat-finger /
	// stolen-token catastrophe. Zero means unlimited (not recommended in prod).
	MaxTransferNGN decimal.Decimal
}

// SafehavenConfig reads Safe Haven settings from env.
func SafehavenConfig() *SafehavenConfiguration {
	viper.SetDefault("SAFEHAVEN_BASE_URL", "https://api.safehavenmfb.com")
	viper.SetDefault("SAFEHAVEN_MAX_TRANSFER_NGN", "1000000")

	maxTransfer, err := decimal.NewFromString(viper.GetString("SAFEHAVEN_MAX_TRANSFER_NGN"))
	if err != nil {
		maxTransfer = decimal.NewFromInt(1_000_000)
	}

	return &SafehavenConfiguration{
		ClientID:           viper.GetString("SAFEHAVEN_CLIENT_ID"),
		PrivateKeyPEM:      viper.GetString("SAFEHAVEN_PRIVATE_KEY"),
		BaseURL:            viper.GetString("SAFEHAVEN_BASE_URL"),
		Audience:           viper.GetString("SAFEHAVEN_AUDIENCE"),
		Issuer:             viper.GetString("SAFEHAVEN_ISSUER"),
		DebitAccountNumber: viper.GetString("SAFEHAVEN_DEBIT_ACCOUNT_NUMBER"),
		WebhookSecret:      viper.GetString("SAFEHAVEN_WEBHOOK_SECRET"),
		MaxTransferNGN:     maxTransfer,
	}
}
