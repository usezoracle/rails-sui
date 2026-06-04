package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// NotificationConfiguration defines the email service configurations
type NotificationConfiguration struct {
	// EmailProvider selects the mailer: "resend" | "sendgrid" | "mailgun".
	EmailProvider    string
	EmailDomain      string
	EmailAPIKey      string
	EmailFromAddress string
	// SendGrid dynamic-template ID for the Tap Card admin recovery
	// email. Empty → recovery endpoint logs the code instead of
	// emailing it (PoC / local dev).
	CardRecoveryTemplate string
}

// NotificationConfig sets the email configurations
func NotificationConfig() (config *NotificationConfiguration) {
	viper.SetDefault("EMAIL_PROVIDER", "sendgrid")
	viper.SetDefault("EMAIL_DOMAIN", "sandbox9c66b379b78d43d2b1533bf2a09a5325.mailgun.org")
	viper.SetDefault("EMAIL_FROM_ADDRESS", "Rails <no-reply@usezoracle.com>")
	viper.SetDefault("CARD_RECOVERY_SENDGRID_TEMPLATE", "")

	return &NotificationConfiguration{
		EmailProvider:        viper.GetString("EMAIL_PROVIDER"),
		EmailDomain:          viper.GetString("EMAIL_DOMAIN"),
		EmailAPIKey:          viper.GetString("EMAIL_API_KEY"),
		EmailFromAddress:     viper.GetString("EMAIL_FROM_ADDRESS"),
		CardRecoveryTemplate: viper.GetString("CARD_RECOVERY_SENDGRID_TEMPLATE"),
	}
}

func init() {
	if err := SetupConfig(); err != nil {
		panic(fmt.Sprintf("config SetupConfig() error: %s", err))
	}
}
