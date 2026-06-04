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
	h := verificationEmailHTML("Ada", "123456")
	assert.Contains(t, h, "123456", "renders the code")
	assert.Contains(t, h, "Ada", "renders the name")
	assert.True(t, strings.HasPrefix(h, "<!doctype html>"), "is HTML")

	r := passwordResetEmailHTML("Bee", "654321")
	assert.Contains(t, r, "654321")
	assert.Contains(t, r, "Reset your password")

	// HTML-escaping guards against injection via name.
	x := verificationEmailHTML("<script>", "000")
	assert.NotContains(t, x, "<script>")
	assert.Contains(t, x, "&lt;script&gt;")
}
