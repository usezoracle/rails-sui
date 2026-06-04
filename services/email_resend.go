package services

import (
	"context"
	"fmt"
	"html"
	"time"

	fastshot "github.com/opus-domini/fast-shot"

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

// --- minimal inline HTML (Resend has no server-side templates) ---------------

func verificationEmailHTML(firstName, token string) string {
	return baseEmailHTML(
		"Verify your email",
		fmt.Sprintf("Hi %s, use the code below to verify your email address.", html.EscapeString(firstName)),
		token,
	)
}

func passwordResetEmailHTML(firstName, token string) string {
	return baseEmailHTML(
		"Reset your password",
		fmt.Sprintf("Hi %s, use the code below to reset your password. If you didn't request this, ignore this email.", html.EscapeString(firstName)),
		token,
	)
}

func codeEmailHTML(title, intro, code string) string {
	return baseEmailHTML(title, html.EscapeString(intro), code)
}

// baseEmailHTML renders a simple, client-safe HTML email with a prominent code.
func baseEmailHTML(title, intro, code string) string {
	return fmt.Sprintf(`<!doctype html><html><body style="font-family:Arial,Helvetica,sans-serif;background:#f6f6f6;margin:0;padding:24px">
<div style="max-width:480px;margin:0 auto;background:#fff;border-radius:12px;padding:32px">
<h2 style="margin:0 0 12px;color:#111">%s</h2>
<p style="color:#444;font-size:14px;line-height:1.5">%s</p>
<div style="margin:24px 0;padding:16px;background:#f0f0f0;border-radius:8px;text-align:center;font-size:28px;letter-spacing:4px;font-weight:bold;color:#111">%s</div>
<p style="color:#999;font-size:12px">If you didn't request this, you can safely ignore this email.</p>
</div></body></html>`, html.EscapeString(title), intro, html.EscapeString(code))
}
