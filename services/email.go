package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	mailgunv3 "github.com/mailgun/mailgun-go/v3"
	fastshot "github.com/opus-domini/fast-shot"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/types"
	"github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

var (
	notificationConf = config.NotificationConfig()

	mailgunClient       mailgunv3.Mailgun
	_DefaultFromAddress = notificationConf.EmailFromAddress
)

type MailProvider string

const (
	MAILGUN_MAIL_PROVIDER  MailProvider = "MAILGUN"
	SENDGRID_MAIL_PROVIDER MailProvider = "SENDGRID"
	RESEND_MAIL_PROVIDER   MailProvider = "RESEND"
)

// DefaultMailProvider resolves the configured EMAIL_PROVIDER into a MailProvider,
// defaulting to SendGrid for backward compatibility.
func DefaultMailProvider() MailProvider {
	switch strings.ToLower(notificationConf.EmailProvider) {
	case "resend":
		return RESEND_MAIL_PROVIDER
	case "mailgun":
		return MAILGUN_MAIL_PROVIDER
	default:
		return SENDGRID_MAIL_PROVIDER
	}
}

// EmailService provides functionality to sending emails via a mailer provider
type EmailService struct {
	MailProvider MailProvider
}

// NewEmailService creates a new instance of EmailService with a given MailProvider.
func NewEmailService(mailProvider MailProvider) *EmailService {
	return &EmailService{MailProvider: mailProvider}
}

// NewMailgun initialize mailgunv3.Mailgun and can be used to initialize a mocked Mailgun interface.
func NewMailgun(m mailgunv3.Mailgun) {
	if m != nil {
		mailgunClient = m
		return
	}

	mailgunClient = mailgunv3.NewMailgun(notificationConf.EmailDomain, notificationConf.EmailAPIKey)
}

// SendEmail performs the action for sending an email.
func (m *EmailService) SendEmail(ctx context.Context, payload types.SendEmailPayload) (types.SendEmailResponse, error) {
	switch m.MailProvider {
	case MAILGUN_MAIL_PROVIDER:
		return sendEmailViaMailgun(ctx, payload)
	case SENDGRID_MAIL_PROVIDER:
		return sendEmailViaSendGrid(ctx, payload)
	case RESEND_MAIL_PROVIDER:
		return sendEmailViaResend(ctx, payload)
	default:
		return types.SendEmailResponse{}, fmt.Errorf("unsupported mail provider")
	}
}

// SendVerificationEmail performs the actions for sending a verification token to the user email.
func (m *EmailService) SendVerificationEmail(ctx context.Context, token, email, firstName string) (types.SendEmailResponse, error) {
	if m.MailProvider == RESEND_MAIL_PROVIDER {
		return sendEmailViaResend(ctx, types.SendEmailPayload{
			FromAddress: _DefaultFromAddress,
			ToAddress:   email,
			Subject:     "Verify your email",
			HTMLBody:    verificationEmailHTML(firstName, token, email),
		})
	}
	payload := types.SendEmailPayload{
		FromAddress: _DefaultFromAddress,
		ToAddress:   email,
		DynamicData: map[string]interface{}{
			"first_name": firstName,
			"token":      token,
		},
	}
	return SendTemplateEmail(payload, "d-f26d853bbb884c0c856f0bbda894032c")

}

// SendPasswordResetEmail performs the actions for sending a password reset token to the user email.
func (m *EmailService) SendPasswordResetEmail(ctx context.Context, token, email, firstName string) (types.SendEmailResponse, error) {
	if m.MailProvider == RESEND_MAIL_PROVIDER {
		return sendEmailViaResend(ctx, types.SendEmailPayload{
			FromAddress: _DefaultFromAddress,
			ToAddress:   email,
			Subject:     "Reset your password",
			HTMLBody:    passwordResetEmailHTML(firstName, token, email),
		})
	}
	payload := types.SendEmailPayload{
		FromAddress: _DefaultFromAddress,
		ToAddress:   email,
		DynamicData: map[string]interface{}{
			"first_name": firstName,
			"token":      token,
		},
	}
	return SendTemplateEmail(payload, "d-8b689801cd9947748775ccd1c4cc932e")
}

// SendCardRecoveryCode emails the 6-digit code the support agent will
// read back to the cardholder during the iOS-resync escape-hatch
// flow (see docs/tapp-card-spec.md "Torn writes & resync — iOS gap").
//
// The template ID is read from the CARD_RECOVERY_SENDGRID_TEMPLATE
// env var so ops can rotate the SendGrid template without redeploying.
// Empty template ID skips the send and returns a noop response — the
// caller logs the code so dev environments without SendGrid can still
// exercise the flow.
func (m *EmailService) SendCardRecoveryCode(ctx context.Context, email, code string) (types.SendEmailResponse, error) {
	if m.MailProvider == RESEND_MAIL_PROVIDER {
		return sendEmailViaResend(ctx, types.SendEmailPayload{
			FromAddress: _DefaultFromAddress,
			ToAddress:   email,
			Subject:     "Your card recovery code",
			HTMLBody:    codeEmailHTML("Card recovery code", "Use this code to recover your card", code),
		})
	}
	templateID := notificationConf.CardRecoveryTemplate
	if templateID == "" {
		return types.SendEmailResponse{Response: "skipped — CARD_RECOVERY_SENDGRID_TEMPLATE not set"}, nil
	}
	payload := types.SendEmailPayload{
		FromAddress: _DefaultFromAddress,
		ToAddress:   email,
		DynamicData: map[string]interface{}{
			"recovery_code": code,
		},
	}
	return SendTemplateEmail(payload, templateID)
}

// sendEmailViaMailgun performs the actions for sending an email.
func sendEmailViaMailgun(ctx context.Context, content types.SendEmailPayload) (types.SendEmailResponse, error) {
	// initialize
	NewMailgun(mailgunClient)

	message := mailgunClient.NewMessage(
		content.FromAddress,
		content.Subject,
		content.Body,
		content.ToAddress,
	)

	response, id, err := mailgunClient.Send(ctx, message)

	return types.SendEmailResponse{Id: id, Response: response}, err
}

// sendEmailViaSendGrid performs the actions for sending an email.
func sendEmailViaSendGrid(ctx context.Context, content types.SendEmailPayload) (types.SendEmailResponse, error) {
	_ = ctx
	from := mail.NewEmail("Rails", "<no-reply@usezoracle.com>")
	to := mail.NewEmail("", content.ToAddress)
	body := mail.NewContent("text/plain", content.Body)
	htmlBody := mail.NewContent("text/html", content.HTMLBody)

	m := mail.NewV3Mail()
	m.Subject = content.Subject
	m.SetFrom(from)
	m.AddContent(body)
	m.AddContent(htmlBody)

	p := mail.NewPersonalization()
	p.AddTos(to)
	m.AddPersonalizations(p)

	request := sendgrid.GetRequest(notificationConf.EmailAPIKey, "/v3/mail/send", fmt.Sprintf("https://%s", notificationConf.EmailDomain))
	request.Method = "POST"
	request.Body = mail.GetRequestBody(m)
	response, err := sendgrid.API(request)
	if err != nil || response.StatusCode >= 400 {
		return types.SendEmailResponse{}, err
	}

	return types.SendEmailResponse{Id: response.Headers["X-Message-Id"][0]}, nil
}

// SendTemplateEmail sends an email using SendGrid's dynamic template.
func SendTemplateEmail(content types.SendEmailPayload, templateId string) (types.SendEmailResponse, error) {
	reqBody := map[string]interface{}{
		"from": map[string]string{
			"email": content.FromAddress,
		},
		"personalizations": []map[string]interface{}{
			{
				"to": []map[string]string{
					{
						"email": content.ToAddress,
						"name":  "Rails",
					},
				},
				"dynamic_template_data": content.DynamicData,
			},
		},
		"template_id": templateId,
	}

	res, err := fastshot.NewClient(fmt.Sprintf("https://%s", notificationConf.EmailDomain)).
		Config().SetTimeout(30*time.Second).
		Auth().BearerToken(notificationConf.EmailAPIKey).
		Header().Add("Content-Type", "application/json").
		Build().POST("/v3/mail/send").
		Body().AsJSON(reqBody).
		Send()
	if err != nil {
		logger.Errorf("error sending request: %v", err)
		return types.SendEmailResponse{}, fmt.Errorf("error sending request: %w", err)
	}

	data, err := utils.ParseJSONResponse(res.RawResponse)
	if err != nil {
		logger.Errorf("error parsing response: %v %v", err, data)
		return types.SendEmailResponse{}, fmt.Errorf("error parsing response: %w", err)
	}

	return types.SendEmailResponse{
		Response: res.RawResponse.Header.Get("X-Message-Id"),
		Id:       res.RawResponse.Header.Get("X-Message-Id"),
	}, nil
}

// SendTemplateEmailWithJsonAttachment sends an email using SendGrid's dynamic template with a JSON attachment.
func SendTemplateEmailWithJsonAttachment(content types.SendEmailPayload, templateId string) error {
	reqBody := map[string]interface{}{
		"from": map[string]string{
			"email": content.FromAddress,
		},
		"personalizations": []map[string]interface{}{
			{
				"to": []map[string]string{
					{
						"email": content.ToAddress,
						"name":  "Rails",
					},
				},
				"dynamic_template_data": content.DynamicData,
			},
		},
		"template_id": templateId,
		"attachments": []map[string]interface{}{
			{
				"content": content.Body,
				"type":    "text/json", "disposition": "attachment",
				"filename": "payload.json",
			},
		},
	}

	res, err := fastshot.NewClient(fmt.Sprintf("https://%s", notificationConf.EmailDomain)).
		Config().SetTimeout(30*time.Second).
		Auth().BearerToken(notificationConf.EmailAPIKey).
		Header().Add("Content-Type", "application/json").
		Build().POST("/v3/mail/send").
		Body().AsJSON(reqBody).
		Send()
	if err != nil {
		logger.Errorf("error sending request: %v", err)
		return fmt.Errorf("error sending request: %w", err)
	}

	data, err := utils.ParseJSONResponse(res.RawResponse)
	if err != nil {
		logger.Errorf("error parsing response: %v %v", err, data)
		return fmt.Errorf("error parsing response: %w", err)
	}

	return nil
}
