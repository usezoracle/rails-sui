package safehaven

import (
	"encoding/json"

	"github.com/shopspring/decimal"
)

// tokenResponse is the /oauth2/token payload (snake_case, OAuth2-style).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	IBSClientID  string `json:"ibs_client_id"`
	IBSUserID    string `json:"ibs_user_id"`
}

// apiResponse is Safe Haven's standard envelope for the REST (non-OAuth)
// endpoints. Data is left raw so each typed call decodes its own shape.
//
// Note: Safe Haven returns HTTP 200 even for business-level rejections; the
// real outcome is in StatusCode/ResponseCode. Callers must check those, not
// just the HTTP status. ("200"/"00" denote success on the gateway.)
type apiResponse struct {
	StatusCode   int             `json:"statusCode"`
	ResponseCode string          `json:"responseCode"`
	Message      string          `json:"message"`
	Data         json.RawMessage `json:"data"`
}

// Bank is one entry from GET /transfers/banks. BankCode is the CBN/NIP code
// (e.g. "090836") passed to name-enquiry and transfer as the beneficiary bank.
type Bank struct {
	Name     string `json:"name"`
	BankCode string `json:"bankCode"`
	Active   bool   `json:"active"`
}

// Account is a Safe Haven account — our main float account (isSubAccount=false)
// or an LP deposit sub-account (isSubAccount=true). AccountBalance is spendable;
// debit it via Transfer.DebitAccountNumber.
type Account struct {
	ID             string          `json:"_id"`
	AccountNumber  string          `json:"accountNumber"`
	AccountName    string          `json:"accountName"`
	AccountBalance decimal.Decimal `json:"accountBalance"`
	LedgerBalance  decimal.Decimal `json:"ledgerBalance"`
	AccountType    string          `json:"accountType"`
	Currency       string          `json:"currencyCode"`
	Status         string          `json:"status"`
}

// NameEnquiry resolves a beneficiary account name and returns the sessionId that
// a subsequent Transfer must reference (Safe Haven binds the transfer to the
// enquiry to prevent mis-sends). Returned by POST /transfers/name-enquiry.
type NameEnquiry struct {
	SessionID     string `json:"sessionId"`
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
	BankCode      string `json:"bankCode"`
}

// TransferRequest is the body for POST /transfers. PaymentReference MUST be
// derived deterministically from our order id so a retried transfer is
// idempotent on Safe Haven's side and never double-pays.
type TransferRequest struct {
	NameEnquiryReference string          `json:"nameEnquiryReference"`
	DebitAccountNumber   string          `json:"debitAccountNumber"`
	BeneficiaryBankCode  string          `json:"beneficiaryBankCode"`
	BeneficiaryAccount   string          `json:"beneficiaryAccountNumber"`
	Amount               decimal.Decimal `json:"amount"`
	Narration            string          `json:"narration"`
	PaymentReference     string          `json:"paymentReference"`
	SaveBeneficiary      bool            `json:"saveBeneficiary"`
}

// Transfer is the result of POST /transfers (and POST /transfers/tqs). Status is
// Safe Haven's transaction status; treat anything other than a terminal success
// as pending and reconcile via webhook + TransferStatus polling.
type Transfer struct {
	SessionID        string          `json:"sessionId"`
	PaymentReference string          `json:"paymentReference"`
	Amount           decimal.Decimal `json:"amount"`
	Fees             decimal.Decimal `json:"fees"`
	Status           string          `json:"status"`
	ResponseMessage  string          `json:"responseMessage"`
	CreditAccount    string          `json:"creditAccountNumber"`
}

// IdentityInit starts BVN/NIN verification for LP sub-account onboarding (Route
// B). The response _id plus the OTP sent to the holder's phone are required to
// create the sub-account. Body for POST /identity/v2.
type IdentityInit struct {
	Type               string `json:"type"`               // BVN | NIN | BVNUSSD | vNIN
	Number             string `json:"number"`             // the BVN/NIN
	DebitAccountNumber string `json:"debitAccountNumber"` // our main account that pays the verification fee
	Async              bool   `json:"async"`
}

// IdentityResult carries the verification _id returned by initiate/validate.
type IdentityResult struct {
	ID     string `json:"_id"`
	Status string `json:"status"`
}

// CreateSubAccountRequest creates an LP deposit sub-account (Route B). Requires a
// prior IdentityInit/validate: pass its IdentityID plus the OTP. Body for
// POST /accounts/v2/subaccount.
type CreateSubAccountRequest struct {
	PhoneNumber       string `json:"phoneNumber"`
	EmailAddress      string `json:"emailAddress"`
	ExternalReference string `json:"externalReference"`
	IdentityType      string `json:"identityType"`
	IdentityNumber    string `json:"identityNumber"`
	IdentityID        string `json:"identityId"`
	OTP               string `json:"otp"`
	CallbackURL       string `json:"callbackUrl,omitempty"`
}
