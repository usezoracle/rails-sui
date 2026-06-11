// Package baas defines a provider-agnostic Banking-as-a-Service rail: the NGN
// fiat in/out used by Route B (decentralized LP) and Route C (managed float).
//
// The concrete provider (today the BaaS provider MFB) lives behind the Provider
// interface and provider-neutral domain types declared here. All app code —
// the auto-pay loop, the admin funding console, the reconcile cron, the LP
// balance endpoint — depends ONLY on this package, never on a vendor SDK. To
// switch providers, implement Provider in a new adapter and select it in the
// composition root (main.go); no consumer changes.
//
// Status normalisation is part of the contract: each adapter maps its vendor's
// status vocabulary onto TransferStatus so consumers branch on a stable enum,
// not on vendor strings.
package baas

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/shopspring/decimal"
)

// ErrNotConfigured signals no BaaS provider is registered. Consumers must
// nil-check Default() and fail their own route gracefully (the rail is optional
// for flows that don't move fiat, e.g. Route A mode=lp).
var ErrNotConfigured = errors.New("baas: no provider configured")

// TransferStatus is the provider-neutral classification of a fiat transfer.
// Adapters normalise their vendor status onto exactly one of these.
type TransferStatus string

const (
	// TransferPending — submitted, not yet terminal; reconcile via webhook + poll.
	TransferPending TransferStatus = "pending"
	// TransferSuccess — money has left the debit account and credited the beneficiary.
	TransferSuccess TransferStatus = "success"
	// TransferFailed — terminal failure; the debit did not stick (or was reversed).
	TransferFailed TransferStatus = "failed"
)

// Account is a funded account on the rail — the platform's main float
// (subAccounts=false) or an LP deposit sub-account (subAccounts=true). Balance
// is spendable; debit it via Transfer.DebitAccountNumber.
type Account struct {
	ID            string
	AccountNumber string
	AccountName   string
	Balance       decimal.Decimal
	LedgerBalance decimal.Decimal
	Type          string
	Currency      string
	Status        string
}

// Bank is a supported beneficiary bank. BankCode is the CBN/NIP code passed to
// NameEnquiry and Transfer.
type Bank struct {
	Name     string
	BankCode string
	Active   bool
}

// NameEnquiry resolves a beneficiary account name and returns the Reference a
// subsequent Transfer must echo (the rail binds the transfer to the enquiry to
// prevent mis-sends). Read-only; no money moves.
type NameEnquiry struct {
	Reference     string
	AccountNumber string
	AccountName   string
	BankCode      string
}

// TransferRequest moves fiat from DebitAccountNumber to the beneficiary.
// PaymentReference MUST be derived deterministically from the order id (see
// PaymentReference) so a retried transfer is idempotent on the rail and never
// double-pays.
type TransferRequest struct {
	NameEnquiryReference string
	DebitAccountNumber   string
	BeneficiaryBankCode  string
	BeneficiaryAccount   string
	Amount               decimal.Decimal
	Narration            string
	PaymentReference     string
	SaveBeneficiary      bool
}

// Transfer is the result of Transfer / TransferStatus. Status is normalised;
// RawStatus preserves the vendor string for logging/audit.
type Transfer struct {
	Reference        string // vendor session/txn ref; pass to TransferStatus
	PaymentReference string
	Amount           decimal.Decimal
	Fees             decimal.Decimal
	Status           TransferStatus
	RawStatus        string
	Message          string
	CreditAccount    string
}

// IdentityInit starts BVN/NIN verification for LP sub-account onboarding.
// SIDE EFFECT: may charge a verification fee and send an OTP.
type IdentityInit struct {
	Type               string // BVN | NIN | BVNUSSD | vNIN
	Number             string
	DebitAccountNumber string
	Async              bool
}

// IdentityResult carries the verification id used to create the sub-account.
type IdentityResult struct {
	ID     string
	Status string
}

// CreateSubAccountRequest opens an LP deposit sub-account after identity
// verification. SIDE EFFECT: opens a real bank account.
type CreateSubAccountRequest struct {
	PhoneNumber       string
	EmailAddress      string
	ExternalReference string
	IdentityType      string
	IdentityNumber    string
	IdentityID        string
	OTP               string
	CallbackURL       string

	// Extended KYC — required by rails that open full customer wallets
	// (Fintava); ignored by rails that only need a BVN (Korapay).
	FirstName   string
	LastName    string
	DateOfBirth string // YYYY-MM-DD
	Address     string
	NIN         string
}

// WebhookEvent is the provider-neutral parse of an inbound transfer/credit
// callback. Status is normalised; PaymentReference routes it to the owning flow.
type WebhookEvent struct {
	Type             string
	PaymentReference string
	ProviderRef      string
	Status           TransferStatus
	RawStatus        string
	Amount           string
	AccountNumber    string
}

// Provider is the BaaS rail contract. An adapter wraps one vendor SDK and
// normalises its types onto the structs above.
type Provider interface {
	// Name identifies the implementation (e.g. "safehaven"). For logs/audit.
	Name() string

	// --- Account reads (no money movement) ---
	ListBanks(ctx context.Context) ([]Bank, error)
	ListAccounts(ctx context.Context, subAccounts bool) ([]Account, error)
	GetAccount(ctx context.Context, accountID string) (*Account, error)

	// --- Money movement ---
	NameEnquiry(ctx context.Context, bankCode, accountNumber string) (*NameEnquiry, error)
	Transfer(ctx context.Context, req TransferRequest) (*Transfer, error)
	TransferStatus(ctx context.Context, providerRef string) (*Transfer, error)

	// --- Sub-account provisioning (LP onboarding) ---
	InitiateIdentity(ctx context.Context, req IdentityInit) (*IdentityResult, error)
	ValidateIdentity(ctx context.Context, identityID, idType, otp string) (*IdentityResult, error)
	CreateSubAccount(ctx context.Context, req CreateSubAccountRequest) (*Account, error)

	// --- Inbound webhook ---
	// VerifyWebhook reports whether the raw body + signature are authentic.
	// Returns false when no secret is configured (the caller decides policy).
	VerifyWebhook(body []byte, signature string) bool
	// WebhookConfigured reports whether a verification secret is set, so the
	// caller can fail closed in production.
	WebhookConfigured() bool
	// ParseWebhook decodes the vendor payload into a neutral event.
	ParseWebhook(body []byte) (*WebhookEvent, error)
}

var (
	mu       sync.RWMutex
	provider Provider
)

// SetDefault registers the process-wide BaaS provider. Called once from the
// composition root after the adapter is built from config.
func SetDefault(p Provider) {
	mu.Lock()
	provider = p
	mu.Unlock()
}

// Default returns the process-wide provider, or nil if none was configured.
func Default() Provider {
	mu.RLock()
	defer mu.RUnlock()
	return provider
}

// PaymentReference builds a deterministic, idempotent transfer reference from a
// route prefix and an order id, so a retried payout reuses the same reference
// and the rail rejects the duplicate instead of double-paying.
//
//	PaymentReference("routeB", orderID) -> "routeB-<orderID>"
func PaymentReference(prefix, orderID string) string {
	return fmt.Sprintf("%s-%s", prefix, orderID)
}
