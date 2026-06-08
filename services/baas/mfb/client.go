package mfb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the authenticated the BaaS provider REST client. It wraps an
// Authenticator and attaches a fresh bearer token to every request.
//
// Implemented so far: GetBanks (read-only, safe to call against live). The
// money-movement methods — NameEnquiry, Transfer, TransferStatus — are the
// next step and are deliberately left out until their exact request shapes
// are confirmed and a live test is authorised. See docs/route-c-mfb.md.
type Client struct {
	auth       *Authenticator
	httpClient *http.Client
}

// NewClient builds a Client from an Authenticator. The HTTP client is shared
// with the authenticator's when none is supplied.
func NewClient(auth *Authenticator) *Client {
	hc := auth.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{auth: auth, httpClient: hc}
}

// GetBanks lists supported beneficiary banks (GET /transfers/banks). Read-only;
// a convenient call to prove auth works end to end against the live gateway.
func (c *Client) GetBanks(ctx context.Context) ([]Bank, error) {
	var banks []Bank
	if err := c.doJSON(ctx, http.MethodGet, "/transfers/banks", nil, &banks); err != nil {
		return nil, err
	}
	return banks, nil
}

// ListAccounts returns our accounts. isSubAccount=false → main float account(s)
// (Route C debit source); isSubAccount=true → LP deposit sub-accounts (Route B).
// Read-only.
func (c *Client) ListAccounts(ctx context.Context, isSubAccount bool) ([]Account, error) {
	path := fmt.Sprintf("/accounts?page=0&limit=100&isSubAccount=%t", isSubAccount)
	// The list endpoint may wrap accounts under a "accounts" key alongside
	// pagination; decode leniently and fall back to a bare array.
	var wrapped struct {
		Accounts []Account `json:"accounts"`
	}
	raw, err := c.doRaw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Accounts) > 0 {
		return wrapped.Accounts, nil
	}
	var arr []Account
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("mfb: decode accounts: %w (data: %s)", err, string(raw))
	}
	return arr, nil
}

// GetAccount fetches one account (incl. balance) by the BaaS provider account id.
// Read-only.
func (c *Client) GetAccount(ctx context.Context, id string) (*Account, error) {
	var acct Account
	if err := c.doJSON(ctx, http.MethodGet, "/accounts/"+id, nil, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

// NameEnquiry resolves a beneficiary's account name and returns the sessionId a
// Transfer must reference. Read-only (no money moves).
func (c *Client) NameEnquiry(ctx context.Context, bankCode, accountNumber string) (*NameEnquiry, error) {
	body := map[string]string{"bankCode": bankCode, "accountNumber": accountNumber}
	var ne NameEnquiry
	if err := c.doJSON(ctx, http.MethodPost, "/transfers/name-enquiry", body, &ne); err != nil {
		return nil, err
	}
	return &ne, nil
}

// Transfer moves fiat from req.DebitAccountNumber (main float for Route C, an LP
// sub-account for Route B) to the beneficiary. MONEY-MOVEMENT: callers must pass
// a deterministic PaymentReference for idempotency and should only invoke this
// behind an explicit settlement decision.
func (c *Client) Transfer(ctx context.Context, req TransferRequest) (*Transfer, error) {
	var tr Transfer
	if err := c.doJSON(ctx, http.MethodPost, "/transfers", req, &tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

// TransferStatus queries the outcome of a transfer (POST /transfers/tqs) by the
// sessionId returned from Transfer. Read-only; use to reconcile alongside the
// webhook.
func (c *Client) TransferStatus(ctx context.Context, sessionID string) (*Transfer, error) {
	body := map[string]string{"sessionId": sessionID}
	var tr Transfer
	if err := c.doJSON(ctx, http.MethodPost, "/transfers/tqs", body, &tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

// InitiateIdentity starts BVN/NIN verification for an LP (Route B onboarding).
// SIDE EFFECT: charges a verification fee to DebitAccountNumber and sends an OTP
// to the holder's phone. Gate behind explicit onboarding intent.
func (c *Client) InitiateIdentity(ctx context.Context, req IdentityInit) (*IdentityResult, error) {
	var res IdentityResult
	if err := c.doJSON(ctx, http.MethodPost, "/identity/v2", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ValidateIdentity completes verification with the OTP. type must match the
// initiate call (BVN → BVNUSSD per the BaaS provider's flow).
func (c *Client) ValidateIdentity(ctx context.Context, identityID, idType, otp string) (*IdentityResult, error) {
	body := map[string]string{"identityId": identityID, "type": idType, "otp": otp}
	var res IdentityResult
	if err := c.doJSON(ctx, http.MethodPost, "/identity/v2/validate", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// CreateSubAccount provisions an LP deposit sub-account after identity
// verification (Route B). SIDE EFFECT: opens a real bank account. Requires a
// validated IdentityID + OTP from InitiateIdentity/ValidateIdentity.
func (c *Client) CreateSubAccount(ctx context.Context, req CreateSubAccountRequest) (*Account, error) {
	var acct Account
	if err := c.doJSON(ctx, http.MethodPost, "/accounts/v2/subaccount", req, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

// doJSON performs an authenticated request and unmarshals the envelope's data
// field into out (when non-nil).
func (c *Client) doJSON(ctx context.Context, method, path string, payload, out any) error {
	data, err := c.doRaw(ctx, method, path, payload)
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("mfb: decode %s %s data: %w (data: %s)", method, path, err, string(data))
		}
	}
	return nil
}

// doRaw performs an authenticated request against the the BaaS provider REST API and
// returns the envelope's raw data field. It treats a non-2xx HTTP status, or a
// non-success responseCode, as an error.
func (c *Client) doRaw(ctx context.Context, method, path string, payload any) (json.RawMessage, error) {
	token, err := c.auth.Token(ctx)
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("mfb: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.auth.BaseURL()+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	// the BaaS provider scopes authenticated REST calls by the ibs_client_id returned
	// at token exchange; without this header the gateway's guard rejects the
	// request with 403 "Forbidden resource" even though the bearer is valid.
	if ibs := c.auth.IBSClientID(); ibs != "" {
		req.Header.Set("ClientID", ibs)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mfb: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mfb: %s %s http %d: %s", method, path, resp.StatusCode, string(body))
	}

	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("mfb: decode %s %s: %w (body: %s)", method, path, err, string(body))
	}
	// the BaaS provider signals business-level failure via the envelope, not HTTP.
	if env.ResponseCode != "" && env.ResponseCode != "00" {
		return nil, fmt.Errorf("mfb: %s %s rejected: code=%s msg=%q", method, path, env.ResponseCode, env.Message)
	}
	return env.Data, nil
}
