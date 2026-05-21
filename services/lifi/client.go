package lifi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Sui's LiFi chain id (chainType = MVM). Verified against
// https://li.quest/v1/chains?chainTypes=MVM
const SuiChainID int64 = 9270000000000000

// BSC chain id (target for Route A bridging).
const BSCChainID int64 = 56

// DefaultBaseURL is LiFi's public API root.
const DefaultBaseURL = "https://li.quest/v1"

// DefaultSlippage is the slippage tolerance used when QuoteRequest.Slippage
// is zero. Matches LiFi's documented default.
const DefaultSlippage = 0.003

// Client is a thin HTTP wrapper for the LiFi REST API.
//
// Free-tier usage requires no API key but is rate-limited. Production volume
// needs a key obtained from https://li.quest — set APIKey to enable it; the
// client sends it as the "x-lifi-api-key" header.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// New constructs a Client with sensible defaults: 30s HTTP timeout, public
// LiFi base URL. APIKey may be empty (free tier).
func New(apiKey string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetQuote fetches a bridge quote from LiFi. For Sui-source quotes, the
// QuoteResponse.TransactionRequest.Data is base64-encoded Sui transaction
// bytes the caller signs + submits via sui_executeTransactionBlock.
func (c *Client) GetQuote(ctx context.Context, req QuoteRequest) (*QuoteResponse, error) {
	if req.Slippage == 0 {
		req.Slippage = DefaultSlippage
	}

	params := url.Values{}
	params.Set("fromChain", req.FromChain)
	params.Set("toChain", req.ToChain)
	params.Set("fromToken", req.FromToken)
	params.Set("toToken", req.ToToken)
	params.Set("fromAmount", req.FromAmount)
	params.Set("fromAddress", req.FromAddress)
	if req.ToAddress != "" {
		params.Set("toAddress", req.ToAddress)
	}
	params.Set("slippage", strconv.FormatFloat(req.Slippage, 'f', -1, 64))
	if req.IntegratorID != "" {
		params.Set("integrator", req.IntegratorID)
	}

	endpoint := c.BaseURL + "/quote?" + params.Encode()
	var resp QuoteResponse
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("lifi: get quote: %w", err)
	}
	return &resp, nil
}

// GetStatus polls bridge progress via /status. Callers should poll every
// ~30s until Status == "DONE" or "FAILED".
func (c *Client) GetStatus(ctx context.Context, req StatusRequest) (*StatusResponse, error) {
	params := url.Values{}
	params.Set("txHash", req.TxHash)
	if req.Bridge != "" {
		params.Set("bridge", req.Bridge)
	}
	if req.FromChain != "" {
		params.Set("fromChain", req.FromChain)
	}
	if req.ToChain != "" {
		params.Set("toChain", req.ToChain)
	}
	endpoint := c.BaseURL + "/status?" + params.Encode()
	var resp StatusResponse
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("lifi: get status: %w", err)
	}
	return &resp, nil
}

// do is the shared HTTP call helper. Adds the API key header when set,
// JSON-decodes responses into out, and surfaces non-2xx bodies as errors.
func (c *Client) do(ctx context.Context, method, endpoint string, body io.Reader, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("x-lifi-api-key", c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(respBody))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, string(respBody))
	}
	return nil
}
