package korapay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/services/baas"
)

// Adapter makes the Korapay Client satisfy baas.Provider. This is the
// only file that knows both vocabularies; everything downstream
// depends on baas alone. Normalisations:
//
//   - One pooled NGN balance → a single synthetic Account (ID "NGN");
//     DebitAccountNumber on transfers is ignored.
//   - PaymentReference doubles as the provider ref (Korapay keys
//     everything on the merchant reference).
//   - A duplicate-reference rejection is converted into a status fetch,
//     so retrying a transfer is idempotent like the interface demands.
//   - Initiate/Validate identity collapse onto Korapay's one-shot
//     lookup (no OTP challenge exists).
//   - Webhook signatures cover the raw `data` object bytes only,
//     keyed with the API secret key.
type Adapter struct {
	client *Client
}

// NewAdapter wraps a Korapay Client as a baas.Provider.
func NewAdapter(client *Client) *Adapter { return &Adapter{client: client} }

var _ baas.Provider = (*Adapter)(nil)

func (a *Adapter) Name() string { return "korapay" }

func (a *Adapter) ListBanks(ctx context.Context) ([]baas.Bank, error) {
	banks, err := a.client.ListBanks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]baas.Bank, 0, len(banks))
	for _, b := range banks {
		out = append(out, baas.Bank{Name: b.Name, BankCode: b.Code, Active: true})
	}
	return out, nil
}

// ListAccounts maps the pooled per-currency balances onto synthetic
// accounts. Korapay has no balance-holding sub-accounts (VBAs pool
// into the merchant balance), so subAccounts=true returns empty —
// per-LP balances live in OUR ledger, not on this rail.
func (a *Adapter) ListAccounts(ctx context.Context, subAccounts bool) ([]baas.Account, error) {
	if subAccounts {
		return []baas.Account{}, nil
	}
	balances, err := a.client.Balances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]baas.Account, 0, len(balances))
	for ccy, b := range balances {
		out = append(out, baas.Account{
			ID:            ccy,
			AccountName:   "Korapay pooled balance (" + ccy + ")",
			Balance:       b.AvailableBalance,
			LedgerBalance: b.AvailableBalance.Add(b.PendingBalance),
			Type:          "pooled",
			Currency:      ccy,
			Status:        "active",
		})
	}
	return out, nil
}

func (a *Adapter) GetAccount(ctx context.Context, accountID string) (*baas.Account, error) {
	if accountID == "" {
		accountID = "NGN"
	}
	accounts, err := a.ListAccounts(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if strings.EqualFold(accounts[i].ID, accountID) {
			return &accounts[i], nil
		}
	}
	return nil, fmt.Errorf("korapay: no balance for currency %q", accountID)
}

// NameEnquiry resolves the beneficiary. Korapay doesn't bind transfers
// to an enquiry session, so Reference is empty and TransferRequest may
// leave NameEnquiryReference blank.
func (a *Adapter) NameEnquiry(ctx context.Context, bankCode, accountNumber string) (*baas.NameEnquiry, error) {
	r, err := a.client.Resolve(ctx, bankCode, accountNumber)
	if err != nil {
		return nil, err
	}
	return &baas.NameEnquiry{
		AccountNumber: r.AccountNumber,
		AccountName:   r.AccountName,
		BankCode:      r.BankCode,
	}, nil
}

func (a *Adapter) Transfer(ctx context.Context, req baas.TransferRequest) (*baas.Transfer, error) {
	p, err := a.client.Disburse(ctx, req.PaymentReference, req.Amount,
		req.BeneficiaryBankCode, req.BeneficiaryAccount, req.Narration)
	if err != nil {
		// Retry safety: the interface demands idempotency on
		// PaymentReference. Korapay rejects duplicates instead of
		// replaying, so a rejected retry means the original submit
		// went through — fetch and return it.
		if IsDuplicateReference(err) {
			return a.TransferStatus(ctx, req.PaymentReference)
		}
		return nil, err
	}
	return toTransfer(p), nil
}

func (a *Adapter) TransferStatus(ctx context.Context, providerRef string) (*baas.Transfer, error) {
	p, err := a.client.PayoutStatus(ctx, providerRef)
	if err != nil {
		return nil, err
	}
	return toTransfer(p), nil
}

// InitiateIdentity runs Korapay's one-shot BVN/NIN lookup. There is no
// OTP challenge to the identity owner on this rail — the result is
// immediately terminal, and ValidateIdentity is a no-op echo.
func (a *Adapter) InitiateIdentity(ctx context.Context, req baas.IdentityInit) (*baas.IdentityResult, error) {
	idType := strings.ToLower(strings.TrimSpace(req.Type))
	// Map the interface's BVN/NIN vocabulary (BVNUSSD, vNIN variants
	// have no Korapay equivalent).
	switch idType {
	case "bvn", "nin":
	default:
		return nil, fmt.Errorf("korapay: identity type %q not supported (bvn|nin)", req.Type)
	}
	if _, err := a.client.VerifyIdentity(ctx, idType, req.Number); err != nil {
		return nil, err
	}
	// The identity number doubles as the identity id downstream
	// (CreateSubAccount needs the BVN itself for the VBA's kyc field).
	return &baas.IdentityResult{ID: req.Number, Status: "verified"}, nil
}

func (a *Adapter) ValidateIdentity(_ context.Context, identityID, _, _ string) (*baas.IdentityResult, error) {
	// One-shot rail: verification completed in InitiateIdentity.
	return &baas.IdentityResult{ID: identityID, Status: "verified"}, nil
}

// CreateSubAccount opens a permanent virtual bank account. NOTE the
// semantics differ from a true sub-account rail: deposits to this
// account pool into the merchant balance — per-owner balances are the
// caller's ledger to keep (attribute charge.success webhooks by
// ExternalReference).
func (a *Adapter) CreateSubAccount(ctx context.Context, req baas.CreateSubAccountRequest) (*baas.Account, error) {
	if strings.ToLower(req.IdentityType) != "bvn" || req.IdentityNumber == "" {
		return nil, fmt.Errorf("korapay: virtual accounts require a BVN (got identity type %q)", req.IdentityType)
	}
	// The interface carries no display name; Korapay requires one.
	// Use the email local part — cosmetic only (shows on transfers).
	name := req.EmailAddress
	if at := strings.IndexByte(name, '@'); at > 0 {
		name = name[:at]
	}
	if name == "" {
		name = req.ExternalReference
	}
	va, err := a.client.CreateVirtualAccount(ctx, req.ExternalReference, name, req.EmailAddress, req.IdentityNumber)
	if err != nil {
		return nil, err
	}
	return &baas.Account{
		ID:            va.AccountReference,
		AccountNumber: va.AccountNumber,
		AccountName:   va.AccountName,
		Balance:       decimal.Zero, // pooled rail: no per-VBA balance
		LedgerBalance: decimal.Zero,
		Type:          "virtual",
		Currency:      va.Currency,
		Status:        va.AccountStatus,
	}, nil
}

// WebhookConfigured: Korapay signs with the API secret key itself, so
// webhooks are verifiable whenever the client is configured.
func (a *Adapter) WebhookConfigured() bool { return a.client.SecretKey != "" }

// VerifyWebhook checks x-korapay-signature: HMAC-SHA256 (hex) over the
// RAW BYTES of the `data` object — not the whole body — keyed with the
// API secret key. We extract the data slice from the original bytes
// (json.RawMessage) precisely to avoid re-serialization mismatches.
func (a *Adapter) VerifyWebhook(body []byte, signature string) bool {
	if a.client.SecretKey == "" {
		return false
	}
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || len(probe.Data) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.client.SecretKey))
	mac.Write(probe.Data)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature)))
}

// ParseWebhook decodes transfer.* (payouts) and charge.* (pay-ins,
// incl. VBA credits) events onto the neutral shape.
func (a *Adapter) ParseWebhook(body []byte) (*baas.WebhookEvent, error) {
	var p struct {
		Event string `json:"event"`
		Data  struct {
			Reference        string      `json:"reference"`
			PaymentReference string      `json:"payment_reference"`
			Status           string      `json:"status"`
			Amount           json.Number `json:"amount"`
			Currency         string      `json:"currency"`
			VBADetails       struct {
				VirtualBankAccount struct {
					AccountNumber    string `json:"account_number"`
					AccountReference string `json:"account_reference"`
				} `json:"virtual_bank_account"`
			} `json:"virtual_bank_account_details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("korapay: parse webhook: %w", err)
	}
	paymentRef := p.Data.PaymentReference
	if paymentRef == "" {
		paymentRef = p.Data.Reference
	}
	return &baas.WebhookEvent{
		Type:             p.Event,
		PaymentReference: paymentRef,
		ProviderRef:      p.Data.Reference,
		Status:           normalizeStatus(p.Data.Status),
		RawStatus:        p.Data.Status,
		Amount:           p.Data.Amount.String(),
		AccountNumber:    p.Data.VBADetails.VirtualBankAccount.AccountNumber,
	}, nil
}

func toTransfer(p *Payout) *baas.Transfer {
	return &baas.Transfer{
		Reference:        p.Reference, // merchant ref IS the provider ref on this rail
		PaymentReference: p.Reference,
		Amount:           p.Amount,
		Fees:             p.Fee,
		Status:           normalizeStatus(p.Status),
		RawStatus:        p.Status,
		Message:          p.Message,
	}
}

// normalizeStatus maps Korapay's payout vocabulary onto the neutral
// enum. Unknown/empty → pending: never assume terminal without
// evidence.
func normalizeStatus(s string) baas.TransferStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "successful":
		return baas.TransferSuccess
	case "failed", "reversed", "expired":
		return baas.TransferFailed
	default: // pending, processing, ...
		return baas.TransferPending
	}
}
