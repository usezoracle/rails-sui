// Package korapay is the Korapay (korahq.com) adapter for the baas
// rail: NGN payouts from the merchant float, beneficiary resolution,
// balance reads, BVN/NIN identity lookups, and permanent virtual bank
// accounts for deposits.
//
// Korapay specifics the adapter normalises away (see adapter.go):
//
//   - Single pooled merchant balance per currency — no per-account
//     debiting; TransferRequest.DebitAccountNumber is ignored.
//   - The merchant-supplied `reference` IS the provider ref: payouts,
//     status lookups, and webhooks all key on it.
//   - Duplicate reference → explicit error, not idempotent replay; the
//     adapter converts that into a status fetch so retries stay safe.
//   - Identity verification is a one-shot lookup (no OTP challenge).
//   - Webhook signature: HMAC-SHA256 (hex) over the raw `data` object
//     bytes — NOT the whole body — keyed with the API secret key.
//
// Docs: https://developers.korapay.com/docs/payout-via-api,
// /docs/virtual-bank-accounts-ngn, /docs/webhooks, /docs/balance-api.
package korapay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// DefaultBaseURL is Korapay's API root (test/live selected by key).
const DefaultBaseURL = "https://api.korapay.com/merchant"

// Client is a thin HTTP wrapper over the Korapay merchant API.
type Client struct {
	BaseURL    string
	SecretKey  string
	HTTPClient *http.Client

	// PayoutCustomerEmail is attached to every disbursement (Korapay
	// requires a customer email per payout; receipts go there). Use an
	// ops inbox.
	PayoutCustomerEmail string
	// VBABankCode is the partner bank issuing virtual accounts
	// ("035" Wema in live, "000" in sandbox).
	VBABankCode string
}

// New constructs a Client. secretKey is required; baseURL "" → prod.
func New(secretKey, baseURL, payoutEmail, vbaBankCode string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if vbaBankCode == "" {
		vbaBankCode = "035" // Wema
	}
	return &Client{
		BaseURL:             strings.TrimRight(baseURL, "/"),
		SecretKey:           secretKey,
		HTTPClient:          &http.Client{Timeout: 30 * time.Second},
		PayoutCustomerEmail: payoutEmail,
		VBABankCode:         vbaBankCode,
	}
}

// envelope is Korapay's uniform response wrapper.
type envelope struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// apiError carries Korapay's message so callers can branch on it
// (e.g. duplicate-reference detection).
type apiError struct {
	HTTPStatus int
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("korapay: http %d: %s", e.HTTPStatus, e.Message)
}

// IsDuplicateReference reports whether an error is Korapay's
// duplicate-payment-reference rejection — the retry-safety signal.
func IsDuplicateReference(err error) bool {
	ae, ok := err.(*apiError)
	return ok && strings.Contains(strings.ToLower(ae.Message), "duplicate")
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("korapay: marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.SecretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("korapay: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("korapay: read response: %w", err)
	}

	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return &apiError{HTTPStatus: resp.StatusCode, Message: truncate(raw, 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !env.Status {
		return &apiError{HTTPStatus: resp.StatusCode, Message: env.Message}
	}
	if out != nil && len(env.Data) > 0 {
		if jerr := json.Unmarshal(env.Data, out); jerr != nil {
			return fmt.Errorf("korapay: decode data: %w", jerr)
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Banks + resolution
// ---------------------------------------------------------------------------

// Bank is one beneficiary bank. Code is the classic CBN code Korapay's
// payout API accepts; NIBSSBankCode is the NIP code.
type Bank struct {
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	Code          string `json:"code"`
	NIBSSBankCode string `json:"nibss_bank_code"`
	Country       string `json:"country"`
}

// ListBanks returns NGN beneficiary banks.
func (c *Client) ListBanks(ctx context.Context) ([]Bank, error) {
	var out []Bank
	if err := c.do(ctx, http.MethodGet, "/api/v1/misc/banks?countryCode=NG", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Resolved is a beneficiary name-enquiry result.
type Resolved struct {
	BankName      string `json:"bank_name"`
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
}

// Resolve looks up the account name for (bankCode, accountNumber).
func (c *Client) Resolve(ctx context.Context, bankCode, accountNumber string) (*Resolved, error) {
	var out Resolved
	err := c.do(ctx, http.MethodPost, "/api/v1/misc/banks/resolve", map[string]string{
		"bank":     bankCode,
		"account":  accountNumber,
		"currency": "NGN",
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Balance
// ---------------------------------------------------------------------------

// Balance is one currency's pooled merchant balance.
type Balance struct {
	AvailableBalance decimal.Decimal `json:"available_balance"`
	PendingBalance   decimal.Decimal `json:"pending_balance"`
}

// Balances returns the per-currency pooled balances.
func (c *Client) Balances(ctx context.Context) (map[string]Balance, error) {
	var out map[string]Balance
	if err := c.do(ctx, http.MethodGet, "/api/v1/balances", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Payouts
// ---------------------------------------------------------------------------

// Payout is a disbursement result (from disburse or the verification
// endpoint). Reference is the merchant-supplied idempotency key —
// Korapay has no separate vendor ref.
type Payout struct {
	Reference string          `json:"reference"`
	Status    string          `json:"status"` // pending|processing|success|failed
	Amount    decimal.Decimal `json:"amount"`
	Fee       decimal.Decimal `json:"fee"`
	Currency  string          `json:"currency"`
	Narration string          `json:"narration"`
	Message   string          `json:"message"`
	TraceID   string          `json:"trace_id"`
}

// Disburse submits a single NGN payout from the pooled balance.
// reference must be unique (≥5 chars); duplicates are rejected — use
// IsDuplicateReference + PayoutStatus for retry safety.
func (c *Client) Disburse(ctx context.Context, reference string, amount decimal.Decimal, bankCode, accountNumber, narration string) (*Payout, error) {
	body := map[string]any{
		"reference": reference,
		"destination": map[string]any{
			"type":      "bank_account",
			"amount":    amount.StringFixed(2),
			"currency":  "NGN",
			"narration": narration,
			"bank_account": map[string]string{
				"bank":    bankCode,
				"account": accountNumber,
			},
			"customer": map[string]string{
				"email": c.PayoutCustomerEmail,
			},
		},
	}
	var out Payout
	if err := c.do(ctx, http.MethodPost, "/api/v1/transactions/disburse", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PayoutStatus fetches a payout by the merchant reference.
func (c *Client) PayoutStatus(ctx context.Context, reference string) (*Payout, error) {
	var out Payout
	if err := c.do(ctx, http.MethodGet, "/api/v1/transactions/"+reference, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Identity (one-shot BVN/NIN lookup — no OTP challenge)
// ---------------------------------------------------------------------------

// Identity is the subset of Korapay's identity response we surface.
type Identity struct {
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	DateOfBirth string `json:"date_of_birth"`
	PhoneNumber string `json:"phone_number"`
}

// VerifyIdentity runs a one-shot BVN or NIN lookup. idType is "bvn" or
// "nin". verification_consent is asserted by the caller's UX.
func (c *Client) VerifyIdentity(ctx context.Context, idType, number string) (*Identity, error) {
	idType = strings.ToLower(idType)
	if idType != "bvn" && idType != "nin" {
		return nil, fmt.Errorf("korapay: unsupported identity type %q (bvn|nin)", idType)
	}
	var out Identity
	err := c.do(ctx, http.MethodPost, "/api/v1/identities/ng/"+idType, map[string]any{
		"id":                   number,
		"verification_consent": true,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Virtual bank accounts (permanent, NGN, BVN-mandatory)
// ---------------------------------------------------------------------------

// VirtualAccount is a permanent NGN deposit account. Credits pool into
// the merchant balance; attribution is via AccountReference on the
// charge.success webhook.
type VirtualAccount struct {
	AccountNumber    string `json:"account_number"`
	AccountName      string `json:"account_name"`
	BankCode         string `json:"bank_code"`
	BankName         string `json:"bank_name"`
	AccountReference string `json:"account_reference"`
	UniqueID         string `json:"unique_id"`
	AccountStatus    string `json:"account_status"`
	Currency         string `json:"currency"`
}

// CreateVirtualAccount opens a permanent VBA. accountReference is the
// caller's unique id (e.g. LP profile id); bvn is mandatory.
func (c *Client) CreateVirtualAccount(ctx context.Context, accountReference, accountName, customerEmail, bvn string) (*VirtualAccount, error) {
	body := map[string]any{
		"account_name":      accountName,
		"account_reference": accountReference,
		"permanent":         true,
		"bank_code":         c.VBABankCode,
		"customer": map[string]string{
			"name":  accountName,
			"email": customerEmail,
		},
		"kyc": map[string]string{"bvn": bvn},
	}
	var out VirtualAccount
	if err := c.do(ctx, http.MethodPost, "/api/v1/virtual-bank-account", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetVirtualAccount fetches a VBA by its account reference.
func (c *Client) GetVirtualAccount(ctx context.Context, accountReference string) (*VirtualAccount, error) {
	var out VirtualAccount
	if err := c.do(ctx, http.MethodGet, "/api/v1/virtual-bank-account/"+accountReference, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
