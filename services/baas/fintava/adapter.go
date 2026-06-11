package fintava

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/usezoracle/rails-sui/services/baas"
)

// Adapter normalises the Fintava API onto baas.Provider.
//
// Mapping notes:
//   - The pooled float = the MERCHANT wallet; Transfer debits it via
//     /bank/credit/merchant with our PaymentReference as Fintava's
//     CustomerReference.
//   - Fintava requires the beneficiary accountName on transfers; when
//     the request doesn't carry one we resolve it with a name enquiry
//     first (also a correctness win: mis-sends fail before money
//     moves).
//   - LP deposit accounts = STATIC_FUND customers. Identity is
//     verified by Fintava inside customer creation, so
//     Initiate/ValidateIdentity are accepted no-ops.
//   - Unknown transfer statuses normalise to PENDING, never success.
type Adapter struct {
	c *Client
}

// NewAdapter wraps a configured client.
func NewAdapter(c *Client) *Adapter { return &Adapter{c: c} }

// Client exposes the underlying client for rail-specific calls
// (customer creation fields exceed the neutral request type).
func (a *Adapter) Client() *Client { return a.c }

// Name identifies the rail in logs/audit.
func (a *Adapter) Name() string { return "fintava" }

// ListBanks returns beneficiary banks.
func (a *Adapter) ListBanks(ctx context.Context) ([]baas.Bank, error) {
	banks, err := a.c.ListBanks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]baas.Bank, 0, len(banks))
	for _, b := range banks {
		out = append(out, baas.Bank{Name: b.DisplayName(), BankCode: b.BankCode(), Active: true})
	}
	return out, nil
}

// ListAccounts returns the merchant wallet as the single main account
// (subAccounts=true returns empty — LP balances live in OUR ledger,
// same posture as the Korapay adapter).
func (a *Adapter) ListAccounts(ctx context.Context, subAccounts bool) ([]baas.Account, error) {
	if subAccounts {
		return nil, nil
	}
	bal, err := a.c.MerchantBalance(ctx)
	if err != nil {
		return nil, err
	}
	cur := bal.Currency
	if cur == "" {
		cur = "NGN"
	}
	return []baas.Account{{
		ID:            "merchant",
		AccountNumber: bal.AccountNumber,
		AccountName:   bal.AccountName,
		Balance:       bal.Available(),
		LedgerBalance: bal.LedgerBalance.Decimal,
		Type:          "merchant_wallet",
		Currency:      cur,
		Status:        "active",
	}}, nil
}

// GetAccount reads one customer wallet balance by wallet id.
func (a *Adapter) GetAccount(ctx context.Context, accountID string) (*baas.Account, error) {
	bal, err := a.c.WalletBalance(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return &baas.Account{ID: accountID, Balance: bal, Currency: "NGN", Status: "active"}, nil
}

// NameEnquiry resolves a beneficiary.
func (a *Adapter) NameEnquiry(ctx context.Context, bankCode, accountNumber string) (*baas.NameEnquiry, error) {
	r, err := a.c.NameEnquiry(ctx, accountNumber, bankCode)
	if err != nil {
		return nil, err
	}
	return &baas.NameEnquiry{
		AccountNumber: orDefault(r.AccountNumber, accountNumber),
		AccountName:   r.AccountName,
		BankCode:      orDefault(r.SortCode, bankCode),
	}, nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// Transfer pays a beneficiary from the merchant wallet.
func (a *Adapter) Transfer(ctx context.Context, req baas.TransferRequest) (*baas.Transfer, error) {
	if req.PaymentReference == "" {
		return nil, fmt.Errorf("fintava: PaymentReference is required")
	}
	// Fintava mandates the beneficiary name — resolve it first. Also a
	// correctness win: a mis-keyed account fails before money moves.
	ne, err := a.c.NameEnquiry(ctx, req.BeneficiaryAccount, req.BeneficiaryBankCode)
	if err != nil || ne.AccountName == "" {
		return nil, fmt.Errorf("fintava: could not resolve beneficiary %s/%s: %v", req.BeneficiaryBankCode, req.BeneficiaryAccount, err)
	}
	accountName := ne.AccountName
	res, err := a.c.MerchantTransfer(ctx, req.PaymentReference, req.Amount,
		req.BeneficiaryAccount, accountName, req.BeneficiaryBankCode, req.Narration)
	if err != nil {
		return nil, err
	}
	return &baas.Transfer{
		Reference:        orDefault(res.AnyReference(), req.PaymentReference),
		PaymentReference: req.PaymentReference,
		Amount:           req.Amount,
		Fees:             res.Charges.Decimal,
		Status:           normalizeStatus(res.Status),
		RawStatus:        res.Status,
		Message:          res.Message,
		CreditAccount:    req.BeneficiaryAccount,
	}, nil
}

// TransferStatus looks a transfer up by reference.
func (a *Adapter) TransferStatus(ctx context.Context, providerRef string) (*baas.Transfer, error) {
	res, err := a.c.TransactionByReference(ctx, providerRef)
	if err != nil {
		// An unknown reference is indeterminate, not failed.
		if strings.Contains(err.Error(), "http 404") {
			return &baas.Transfer{Reference: providerRef, PaymentReference: providerRef, Status: baas.TransferPending, RawStatus: "not_found"}, nil
		}
		return nil, err
	}
	return &baas.Transfer{
		Reference:        orDefault(res.AnyReference(), providerRef),
		PaymentReference: orDefault(res.CustomerReference, providerRef),
		Amount:           res.Amount.Decimal,
		Fees:             res.Charges.Decimal,
		Status:           normalizeStatus(res.Status),
		RawStatus:        res.Status,
		Message:          res.Message,
	}, nil
}

// normalizeStatus maps Fintava's vocabulary onto the neutral enum.
// Unknown → pending (poll again), never success.
func normalizeStatus(s string) baas.TransferStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SUCCESS", "SUCCESSFUL", "PAID", "COMPLETED":
		return baas.TransferSuccess
	case "FAILED", "REVERSED", "DECLINED", "CANCELLED":
		return baas.TransferFailed
	default:
		return baas.TransferPending
	}
}

// InitiateIdentity is an accepted no-op: Fintava verifies BVN/NIN
// inside customer creation and rejects bad identities there.
func (a *Adapter) InitiateIdentity(_ context.Context, req baas.IdentityInit) (*baas.IdentityResult, error) {
	return &baas.IdentityResult{ID: req.Number, Status: "deferred_to_customer_creation"}, nil
}

// ValidateIdentity is an accepted no-op (see InitiateIdentity).
func (a *Adapter) ValidateIdentity(_ context.Context, identityID, _, _ string) (*baas.IdentityResult, error) {
	return &baas.IdentityResult{ID: identityID, Status: "deferred_to_customer_creation"}, nil
}

// CreateSubAccount opens the LP's permanent STATIC_FUND wallet.
// Requires the extended KYC fields (name, DOB, address, NIN) the
// neutral request now carries.
func (a *Adapter) CreateSubAccount(ctx context.Context, req baas.CreateSubAccountRequest) (*baas.Account, error) {
	if req.FirstName == "" || req.LastName == "" || req.DateOfBirth == "" || req.Address == "" || req.NIN == "" {
		return nil, fmt.Errorf("fintava: customer creation needs first/last name, dateOfBirth, address and nin")
	}
	cu, err := a.c.CreateCustomer(ctx, CreateCustomerRequest{
		FirstName:   req.FirstName,
		LastName:    req.LastName,
		PhoneNumber: req.PhoneNumber,
		Email:       req.EmailAddress,
		Address:     req.Address,
		DateOfBirth: req.DateOfBirth,
		BVN:         req.IdentityNumber,
		NIN:         req.NIN,
	})
	if err != nil {
		return nil, err
	}
	return &baas.Account{
		ID:            cu.CustomerID(),
		AccountNumber: cu.DepositAccountNumber(),
		AccountName:   strings.TrimSpace(req.FirstName + " " + req.LastName),
		Type:          "static_fund_customer",
		Currency:      "NGN",
		Status:        "active",
	}, nil
}

// VerifyWebhook checks x-fintava-signature: HMAC-SHA512 over the RAW
// body, hex-encoded, constant-time compare. Fails closed without a
// secret.
func (a *Adapter) VerifyWebhook(body []byte, signature string) bool {
	if a.c.WebhookSecret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha512.New, []byte(a.c.WebhookSecret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(want))
}

// WebhookConfigured reports whether a secret is set.
func (a *Adapter) WebhookConfigured() bool { return a.c.WebhookSecret != "" }

// webhookPayload is the tolerant decode of Fintava events:
// {"type": "...", "data": {...}} (field names vary per event).
type webhookPayload struct {
	Type  string `json:"type"`
	Event string `json:"event"`
	Data  struct {
		Amount            flexDecimal `json:"amount"`
		Reference         string      `json:"reference"`
		TransactionRef    string      `json:"transactionReference"`
		CustomerReference string      `json:"customerReference"`
		MerchantReference string      `json:"merchantReference"`
		Status            string      `json:"status"`
		PaymentStatus     string      `json:"paymentStatus"`
		AccountNumber     string      `json:"accountNumber"`
		VirtualAcctNo     string      `json:"virtualAcctNo"`
		TargetAcctNo      string      `json:"target_customer_accno"`
	} `json:"data"`
}

// ParseWebhook maps Fintava events onto the neutral shape:
//
//	account_funded / customer_wallet_credited /
//	VIRTUAL_WALLET_PAYMENT            → Type "deposit" (credit events)
//	customer_bank_transfer /
//	debit_transfer_reversal           → transfer finality by reference
func (a *Adapter) ParseWebhook(body []byte) (*baas.WebhookEvent, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("fintava: parse webhook: %w", err)
	}
	evType := strings.ToLower(orDefault(p.Type, p.Event))
	rawStatus := orDefault(p.Data.Status, p.Data.PaymentStatus)
	ev := &baas.WebhookEvent{
		Type:             evType,
		PaymentReference: firstNonEmpty(p.Data.CustomerReference, p.Data.MerchantReference, p.Data.Reference, p.Data.TransactionRef),
		ProviderRef:      firstNonEmpty(p.Data.Reference, p.Data.TransactionRef, p.Data.CustomerReference),
		Status:           normalizeStatus(rawStatus),
		RawStatus:        rawStatus,
		Amount:           p.Data.Amount.String(),
		AccountNumber:    firstNonEmpty(p.Data.AccountNumber, p.Data.VirtualAcctNo, p.Data.TargetAcctNo),
	}
	switch evType {
	case "account_funded", "customer_wallet_credited", "virtual_wallet_payment":
		ev.Type = "deposit"
	case "debit_transfer_reversal":
		ev.Status = baas.TransferFailed
	}
	return ev, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
