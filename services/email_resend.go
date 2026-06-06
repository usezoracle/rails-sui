package services

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	fastshot "github.com/opus-domini/fast-shot"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/types"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// resendAPIBase is Resend's fixed API host. Unlike SendGrid/Mailgun there is no
// per-account domain in the URL; the sending domain lives in the From address
// (which must be verified in the Resend dashboard).
const resendAPIBase = "https://api.resend.com"

// sendEmailViaResend sends a single email via Resend (POST /emails). Resend has
// no SendGrid-style dynamic templates, so callers supply the HTML body.
func sendEmailViaResend(ctx context.Context, content types.SendEmailPayload) (types.SendEmailResponse, error) {
	_ = ctx
	from := content.FromAddress
	if from == "" {
		from = _DefaultFromAddress
	}
	body := map[string]interface{}{
		"from":    from,
		"to":      []string{content.ToAddress},
		"subject": content.Subject,
		"html":    content.HTMLBody,
	}

	res, err := fastshot.NewClient(resendAPIBase).
		Config().SetTimeout(30*time.Second).
		Auth().BearerToken(notificationConf.EmailAPIKey).
		Header().Add("Content-Type", "application/json").
		Build().POST("/emails").
		Body().AsJSON(body).
		Send()
	if err != nil {
		logger.Errorf("resend: send error: %v", err)
		return types.SendEmailResponse{}, fmt.Errorf("resend: send: %w", err)
	}
	if res.RawResponse.StatusCode >= 400 {
		return types.SendEmailResponse{}, fmt.Errorf("resend: http %d", res.RawResponse.StatusCode)
	}

	return types.SendEmailResponse{
		Id:       res.RawResponse.Header.Get("X-Entity-Id"),
		Response: "sent",
	}, nil
}

// --- inline HTML (Resend has no server-side templates) -----------------------

// frontendURL builds a link to the web app's <path> carrying the (opaque) token
// and email as query params. The frontend page reads them and POSTs to the
// matching API endpoint (e.g. /v1/auth/confirm-account).
func frontendURL(path, token, email string) string {
	base := strings.TrimRight(config.ServerConfig().CheckoutBaseURL, "/")
	return fmt.Sprintf("%s%s?token=%s&email=%s", base, path, url.QueryEscape(token), url.QueryEscape(email))
}

// verificationEmailHTML — a CTA-button email. The token is opaque (not a
// typeable code), so the user clicks a link rather than copying a code.
func verificationEmailHTML(firstName, token, email string) string {
	return buttonEmailHTML(
		"Verify your email",
		fmt.Sprintf("Hi %s, confirm your email address to finish setting up your account.", html.EscapeString(firstName)),
		frontendURL("/verify-email", token, email),
		"Verify email",
	)
}

func passwordResetEmailHTML(firstName, token, email string) string {
	return buttonEmailHTML(
		"Reset your password",
		fmt.Sprintf("Hi %s, click the button below to set a new password. If you didn't request this, you can ignore this email.", html.EscapeString(firstName)),
		frontendURL("/reset-password", token, email),
		"Reset password",
	)
}

// buttonEmailHTML renders a clean, client-safe email with a prominent CTA button
// plus a paste-able link fallback.
func buttonEmailHTML(title, intro, ctaURL, ctaLabel string) string {
	safeURL := html.EscapeString(ctaURL)
	return fmt.Sprintf(`<!doctype html><html><body style="font-family:Arial,Helvetica,sans-serif;background:#f6f6f6;margin:0;padding:24px">
<div style="max-width:480px;margin:0 auto;background:#fff;border-radius:12px;padding:32px">
<h2 style="margin:0 0 12px;color:#111">%s</h2>
<p style="color:#444;font-size:14px;line-height:1.6">%s</p>
<div style="text-align:center;margin:28px 0">
<a href="%s" style="display:inline-block;background:#2775CA;color:#ffffff;text-decoration:none;font-size:15px;font-weight:600;padding:12px 28px;border-radius:8px">%s</a>
</div>
<p style="color:#888;font-size:12px;line-height:1.5">Or paste this link into your browser:<br><a href="%s" style="color:#2775CA;word-break:break-all">%s</a></p>
<p style="color:#999;font-size:12px">If you didn't request this, you can safely ignore this email.</p>
</div></body></html>`, html.EscapeString(title), intro, safeURL, html.EscapeString(ctaLabel), safeURL, safeURL)
}

// codeEmailHTML renders a short, typeable code (e.g. the card-recovery code that
// a support agent reads back). Kept distinct from the link-based emails above.
func codeEmailHTML(title, intro, code string) string {
	return fmt.Sprintf(`<!doctype html><html><body style="font-family:Arial,Helvetica,sans-serif;background:#f6f6f6;margin:0;padding:24px">
<div style="max-width:480px;margin:0 auto;background:#fff;border-radius:12px;padding:32px">
<h2 style="margin:0 0 12px;color:#111">%s</h2>
<p style="color:#444;font-size:14px;line-height:1.5">%s</p>
<div style="margin:24px 0;padding:16px;background:#f0f0f0;border-radius:8px;text-align:center;font-size:28px;letter-spacing:6px;font-weight:bold;color:#111">%s</div>
<p style="color:#999;font-size:12px">If you didn't request this, you can safely ignore this email.</p>
</div></body></html>`, html.EscapeString(title), html.EscapeString(intro), html.EscapeString(code))
}
