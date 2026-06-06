package services

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultMailProvider(t *testing.T) {
	old := notificationConf.EmailProvider
	defer func() { notificationConf.EmailProvider = old }()

	cases := map[string]MailProvider{
		"resend":   RESEND_MAIL_PROVIDER,
		"RESEND":   RESEND_MAIL_PROVIDER,
		"mailgun":  MAILGUN_MAIL_PROVIDER,
		"sendgrid": SENDGRID_MAIL_PROVIDER,
		"":         SENDGRID_MAIL_PROVIDER, // default
		"unknown":  SENDGRID_MAIL_PROVIDER, // default
	}
	for in, want := range cases {
		notificationConf.EmailProvider = in
		assert.Equal(t, want, DefaultMailProvider(), "EMAIL_PROVIDER=%q", in)
	}
}

func TestResendEmailHTML(t *testing.T) {
	h := verificationEmailHTML("Ada", "481920", "ada@example.com")
	assert.Contains(t, h, "Ada", "renders the name")
	assert.Contains(t, h, "Verify your email", "has the verification title")
	assert.Contains(t, h, "481920", "renders the 6-digit code")
	assert.NotContains(t, h, "/verify-email?token=", "no longer a link")
	assert.True(t, strings.HasPrefix(h, "<!doctype html>"), "is HTML")

	r := passwordResetEmailHTML("Bee", "738104", "bee@example.com")
	assert.Contains(t, r, "Reset your password")
	assert.Contains(t, r, "738104", "renders the reset code")

	// HTML-escaping guards against injection via name.
	x := verificationEmailHTML("<script>", "000000", "x@example.com")
	assert.NotContains(t, x, "<script>")
	assert.Contains(t, x, "&lt;script&gt;")
}
