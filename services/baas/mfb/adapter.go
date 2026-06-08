package mfb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/usezoracle/rails-sui/services/baas"
)

// Adapter makes the the BaaS provider Client satisfy baas.Provider, translating Safe
// Haven's vendor types onto the provider-neutral domain and normalising its
// status vocabulary. This is the only file that knows both worlds; everything
// downstream depends on baas alone.
type Adapter struct {
	client        *Client
	webhookSecret string
}

// NewAdapter wraps a the BaaS provider Client as a baas.Provider. webhookSecret is the
// HMAC secret for inbound callbacks ("" disables verification).
func NewAdapter(client *Client, webhookSecret string) *Adapter {
	return &Adapter{client: client, webhookSecret: webhookSecret}
}

// compile-time assertion that the adapter satisfies the interface.
var _ baas.Provider = (*Adapter)(nil)

func (a *Adapter) Name() string { return "safehaven" }

func (a *Adapter) ListBanks(ctx context.Context) ([]baas.Bank, error) {
	banks, err := a.client.GetBanks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]baas.Bank, 0, len(banks))
	for _, b := range banks {
		out = append(out, baas.Bank{Name: b.Name, BankCode: b.BankCode, Active: b.Active})
	}
	return out, nil
}

func (a *Adapter) ListAccounts(ctx context.Context, subAccounts bool) ([]baas.Account, error) {
	accts, err := a.client.ListAccounts(ctx, subAccounts)
	if err != nil {
		return nil, err
	}
	out := make([]baas.Account, 0, len(accts))
	for _, ac := range accts {
		out = append(out, toAccount(ac))
	}
	return out, nil
}

func (a *Adapter) GetAccount(ctx context.Context, accountID string) (*baas.Account, error) {
	ac, err := a.client.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := toAccount(*ac)
	return &out, nil
}

func (a *Adapter) NameEnquiry(ctx context.Context, bankCode, accountNumber string) (*baas.NameEnquiry, error) {
	ne, err := a.client.NameEnquiry(ctx, bankCode, accountNumber)
	if err != nil {
		return nil, err
	}
	return &baas.NameEnquiry{
		Reference:     ne.SessionID,
		AccountNumber: ne.AccountNumber,
		AccountName:   ne.AccountName,
		BankCode:      ne.BankCode,
	}, nil
}

func (a *Adapter) Transfer(ctx context.Context, req baas.TransferRequest) (*baas.Transfer, error) {
	tr, err := a.client.Transfer(ctx, TransferRequest{
		NameEnquiryReference: req.NameEnquiryReference,
		DebitAccountNumber:   req.DebitAccountNumber,
		BeneficiaryBankCode:  req.BeneficiaryBankCode,
		BeneficiaryAccount:   req.BeneficiaryAccount,
		Amount:               req.Amount,
		Narration:            req.Narration,
		PaymentReference:     req.PaymentReference,
		SaveBeneficiary:      req.SaveBeneficiary,
	})
	if err != nil {
		return nil, err
	}
	return toTransfer(tr), nil
}

func (a *Adapter) TransferStatus(ctx context.Context, providerRef string) (*baas.Transfer, error) {
	tr, err := a.client.TransferStatus(ctx, providerRef)
	if err != nil {
		return nil, err
	}
	return toTransfer(tr), nil
}

func (a *Adapter) InitiateIdentity(ctx context.Context, req baas.IdentityInit) (*baas.IdentityResult, error) {
	res, err := a.client.InitiateIdentity(ctx, IdentityInit{
		Type:               req.Type,
		Number:             req.Number,
		DebitAccountNumber: req.DebitAccountNumber,
		Async:              req.Async,
	})
	if err != nil {
		return nil, err
	}
	return &baas.IdentityResult{ID: res.ID, Status: res.Status}, nil
}

func (a *Adapter) ValidateIdentity(ctx context.Context, identityID, idType, otp string) (*baas.IdentityResult, error) {
	res, err := a.client.ValidateIdentity(ctx, identityID, idType, otp)
	if err != nil {
		return nil, err
	}
	return &baas.IdentityResult{ID: res.ID, Status: res.Status}, nil
}

func (a *Adapter) CreateSubAccount(ctx context.Context, req baas.CreateSubAccountRequest) (*baas.Account, error) {
	ac, err := a.client.CreateSubAccount(ctx, CreateSubAccountRequest{
		PhoneNumber:       req.PhoneNumber,
		EmailAddress:      req.EmailAddress,
		ExternalReference: req.ExternalReference,
		IdentityType:      req.IdentityType,
		IdentityNumber:    req.IdentityNumber,
		IdentityID:        req.IdentityID,
		OTP:               req.OTP,
		CallbackURL:       req.CallbackURL,
	})
	if err != nil {
		return nil, err
	}
	out := toAccount(*ac)
	return &out, nil
}

// WebhookConfigured reports whether a verification secret is set.
func (a *Adapter) WebhookConfigured() bool { return a.webhookSecret != "" }

// VerifyWebhook checks an HMAC-SHA256 hex signature over the raw body. Returns
// false when no secret is configured.
func (a *Adapter) VerifyWebhook(body []byte, signature string) bool {
	if a.webhookSecret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.webhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature)))
}

// ParseWebhook decodes the BaaS provider's transfer/credit callback into a neutral
// event. Unknown keys are ignored so added fields stay non-breaking.
func (a *Adapter) ParseWebhook(body []byte) (*baas.WebhookEvent, error) {
	var p struct {
		Type             string `json:"type"`
		PaymentReference string `json:"paymentReference"`
		SessionID        string `json:"sessionId"`
		Status           string `json:"status"`
		Amount           string `json:"amount"`
		AccountNumber    string `json:"accountNumber"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &baas.WebhookEvent{
		Type:             p.Type,
		PaymentReference: p.PaymentReference,
		ProviderRef:      p.SessionID,
		Status:           normalizeStatus(p.Status),
		RawStatus:        p.Status,
		Amount:           p.Amount,
		AccountNumber:    p.AccountNumber,
	}, nil
}

// toAccount maps a the BaaS provider account onto the neutral type.
func toAccount(ac Account) baas.Account {
	return baas.Account{
		ID:            ac.ID,
		AccountNumber: ac.AccountNumber,
		AccountName:   ac.AccountName,
		Balance:       ac.AccountBalance,
		LedgerBalance: ac.LedgerBalance,
		Type:          ac.AccountType,
		Currency:      ac.Currency,
		Status:        ac.Status,
	}
}

// toTransfer maps a the BaaS provider transfer onto the neutral type, normalising status.
func toTransfer(tr *Transfer) *baas.Transfer {
	return &baas.Transfer{
		Reference:        tr.SessionID,
		PaymentReference: tr.PaymentReference,
		Amount:           tr.Amount,
		Fees:             tr.Fees,
		Status:           normalizeStatus(tr.Status),
		RawStatus:        tr.Status,
		Message:          tr.ResponseMessage,
		CreditAccount:    tr.CreditAccount,
	}
}

// normalizeStatus maps the BaaS provider's status vocabulary onto baas.TransferStatus.
// Unknown/empty values are treated as pending so a transfer is never assumed
// terminal without evidence.
func normalizeStatus(s string) baas.TransferStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "successful", "completed", "00", "approved":
		return baas.TransferSuccess
	case "failed", "rejected", "cancelled", "canceled", "reversed", "declined", "error":
		return baas.TransferFailed
	default:
		return baas.TransferPending
	}
}
