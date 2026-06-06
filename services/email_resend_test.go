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
	h := verificationEmailHTML("Ada", "tok123", "ada@example.com")
	assert.Contains(t, h, "Ada", "renders the name")
	assert.Contains(t, h, "Verify email", "has a CTA button")
	assert.Contains(t, h, "/verify-email?token=tok123", "links to the frontend verify page with the token")
	assert.True(t, strings.HasPrefix(h, "<!doctype html>"), "is HTML")

	r := passwordResetEmailHTML("Bee", "rst456", "bee@example.com")
	assert.Contains(t, r, "Reset your password")
	assert.Contains(t, r, "/reset-password?token=rst456")

	// HTML-escaping guards against injection via name.
	x := verificationEmailHTML("<script>", "000", "x@example.com")
	assert.NotContains(t, x, "<script>")
	assert.Contains(t, x, "&lt;script&gt;")
}
