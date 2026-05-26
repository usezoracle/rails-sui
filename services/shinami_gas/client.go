// Package shinami_gas is a JSON-RPC client for Shinami's Sui Gas
// Station service (https://api.us1.shinami.com/sui/gas/v1).
//
// Why this exists: our self-rolled sponsored-tx path in
// services/order/sui.go has been broken in production — every
// aggregator-sponsored Move call hits the block-vision SDK's BCS
// quirks and gets rejected by Sui RPC before submission. Shinami's
// Gas Station does the sponsorship part for us — we send a
// TransactionKind (no gas), they attach their fund's gas + sign as
// sponsor, we sign as sender, we submit both signatures.
//
// Endpoints used:
//
//   - gas_sponsorTransactionBlock   — main path; returns sponsored
//                                     txBytes + sponsor signature
//   - gas_getFund                   — balance check for the alert cron
//   - gas_getSponsoredTransactionBlockStatus — optional, mostly for
//                                     reconciliation; we don't poll it
//                                     in the happy path because we
//                                     immediately submit + observe.
//
// IMPORTANT: per Shinami docs, the access key must NEVER ship to a
// frontend (no CORS, plus leak = drained fund). All calls go from the
// Rails backend.

package shinami_gas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// DefaultBaseURL is the US East mainnet endpoint. Override via
// SHINAMI_GAS_BASE_URL if you ever need a different region or a
// self-hosted staging mock.
const DefaultBaseURL = "https://api.us1.shinami.com/sui/gas/v1"

// Client is a thin wrapper around the Gas Station JSON-RPC endpoint.
// Concurrency-safe — the only mutable state is the request-id counter.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	nextID uint64
}

// New constructs a Client. apiKey is the Shinami Gas access key
// (`X-Api-Key` header value) — required; an empty key returns an
// error on every call so misconfiguration surfaces immediately.
// baseURL may be "" to use DefaultBaseURL.
func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SponsorTransactionBlockResult mirrors Shinami's gas_sponsorTransactionBlock
// response. Field names match their JSON exactly so a future SDK
// upgrade can drop in.
type SponsorTransactionBlockResult struct {
	// Base64-encoded, BCS-serialized TransactionData with Shinami's
	// gas data attached. The sender (us) must sign these bytes.
	TxBytes string `json:"txBytes"`
	// Base58 transaction digest. Useful for the optional status poll.
	TxDigest string `json:"txDigest"`
	// Base64 sponsor signature — pass alongside the sender signature
	// to sui_executeTransactionBlock.
	Signature string `json:"signature"`
	// Unix epoch seconds when the assigned gas object expires.
	// Shinami sets this to ~1h from creation.
	ExpireAtTime int64 `json:"expireAtTime"`
}

// SponsorTransactionBlock asks Shinami to attach a gas coin from our
// fund and sign as sponsor.
//
//	txKindB64  — base64-encoded BCS TransactionKind (built with
//	             onlyTransactionKind: true; NO gas data)
//	sender     — Sui address of the user sender
//	gasBudget  — 0 to use Shinami's auto-budget (recommended); else
//	             explicit MIST. Auto-budget runs a free dryRun first
//	             so invalid txs return an error before sponsorship.
//	gasPrice   — 0 to use current RGP (recommended). Override only
//	             for congestion-priority cases.
//
// Returns the sponsored TransactionData bytes + sponsor signature.
func (c *Client) SponsorTransactionBlock(
	ctx context.Context,
	txKindB64 string,
	sender string,
	gasBudget uint64,
	gasPrice uint64,
) (*SponsorTransactionBlockResult, error) {
	// Match Shinami's own TS SDK shape exactly: trim trailing
	// undefined values rather than padding with nulls. Their
	// `trimTrailingParams([txKind, sender, gasBudget, gasPrice])`
	// drops the right side until it hits a defined value. Sending
	// nulls broke their parser in testing.
	params := []any{txKindB64, sender, nil, nil}
	if gasPrice > 0 {
		params[3] = fmt.Sprintf("%d", gasPrice)
	}
	if gasBudget > 0 {
		params[2] = fmt.Sprintf("%d", gasBudget)
	}
	// Trim trailing nils.
	for len(params) > 2 && params[len(params)-1] == nil {
		params = params[:len(params)-1]
	}

	var result SponsorTransactionBlockResult
	if err := c.call(ctx, "gas_sponsorTransactionBlock", params, &result); err != nil {
		return nil, fmt.Errorf("shinami_gas.SponsorTransactionBlock: %w", err)
	}
	return &result, nil
}

// FundInfo is the gas_getFund response.
type FundInfo struct {
	Network        string `json:"network"`
	Name           string `json:"name"`
	Balance        int64  `json:"balance"`        // MIST
	InFlight       int64  `json:"inFlight"`       // MIST — withheld for active sponsorships
	DepositAddress string `json:"depositAddress"` // nil/"" if no deposit address yet
}

// GetFund returns the balance + in-flight reservation of the fund
// linked to the access key. Cheap; suitable for a balance-alert cron.
func (c *Client) GetFund(ctx context.Context) (*FundInfo, error) {
	var result FundInfo
	if err := c.call(ctx, "gas_getFund", []any{}, &result); err != nil {
		return nil, fmt.Errorf("shinami_gas.GetFund: %w", err)
	}
	return &result, nil
}

// SponsoredStatus is the gas_getSponsoredTransactionBlockStatus result.
type SponsoredStatus string

const (
	SponsoredStatusInFlight SponsoredStatus = "IN_FLIGHT"
	SponsoredStatusInvalid  SponsoredStatus = "INVALID"
	SponsoredStatusComplete SponsoredStatus = "COMPLETE"
)

// GetSponsoredStatus checks whether the sponsored tx has been
// submitted, expired, or completed. Mostly for reconciliation; the
// happy path doesn't need it because we submit immediately after
// receiving the sponsorship response.
func (c *Client) GetSponsoredStatus(ctx context.Context, txDigest string) (SponsoredStatus, error) {
	var result SponsoredStatus
	if err := c.call(ctx, "gas_getSponsoredTransactionBlockStatus", []any{txDigest}, &result); err != nil {
		return "", fmt.Errorf("shinami_gas.GetSponsoredStatus: %w", err)
	}
	return result, nil
}

// jsonrpcEnvelope is the JSON-RPC 2.0 wire format. result is
// decoded into the typed result via json.RawMessage indirection so
// callers don't have to repeat the boilerplate.
type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	if c.APIKey == "" {
		return fmt.Errorf("SHINAMI_GAS_API_KEY not set (Rails .env)")
	}
	id := atomic.AddUint64(&c.nextID, 1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.APIKey)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	// Shinami returns 200 even for JSON-RPC errors. A non-200 means
	// transport-level failure (auth, rate-limit pre-JSONRPC, network).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(respBody))
	}

	var env jsonrpcEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (body: %s)", err, string(respBody))
	}
	if env.Error != nil {
		// Per Shinami docs: code -32010 is rate limit; code -32602 is
		// any param validation failure (gas-object reference, malformed
		// txBytes, wrong sender format, etc.). Dump the params on
		// -32602 so we can inspect what we actually sent. Redact the
		// API key from the dump.
		if env.Error.Code == -32602 {
			paramsDump, _ := json.Marshal(map[string]any{
				"method": method,
				"params": params,
			})
			fmt.Fprintf(os.Stderr,
				"[shinami_gas] -32602 Invalid params — dumping request: %s\n",
				string(paramsDump))
		}
		return fmt.Errorf("rpc error %d: %s", env.Error.Code, env.Error.Message)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("decode result: %w (raw: %s)", err, string(env.Result))
		}
	}
	return nil
}
