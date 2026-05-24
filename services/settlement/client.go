package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// DefaultBaseURL is the public settlement aggregator root, version
// included. Override via SETTLEMENT_API_URL in env to point at a
// self-hosted or staging instance.
// IMPORTANT: include the API version suffix (e.g. "/v1") — endpoint
// builders below append only the path under that version.
const DefaultBaseURL = "https://api.paycrest.io/v1"

// Client wraps the settlement aggregator's HTTP API. Two endpoints are
// used in the Route A off-ramp flow:
//
//	GET /v1/pubkey                          (cached, see PubkeyCache)
//	GET /v1/orders/:chainId/:orderId        (status polling)
//
// We don't authenticate either endpoint — off-ramp orders are picked up
// from the on-chain Gateway event. The senderApiKeyID we ship is metadata,
// not auth; it lives inside the encrypted messageHash recipient blob.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client

	pubkey *pubkeyCache
}

// New constructs a Client with a 15s HTTP timeout and a pubkey cache TTL.
// baseURL may be "" for DefaultBaseURL. ttl <= 0 defaults to one hour.
func New(baseURL string, ttl time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	c := &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	c.pubkey = newPubkeyCache(ttl, c.fetchPubkey)
	return c
}

// FetchPublicKey returns the cached aggregator PEM, refetching on TTL miss.
func (c *Client) FetchPublicKey(ctx context.Context) (string, error) {
	return c.pubkey.Get(ctx)
}

// fetchPublicKey is the un-cached fetch used by the cache.
func (c *Client) fetchPubkey(ctx context.Context) (string, error) {
	var resp pubkeyResponse
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/pubkey", nil, &resp); err != nil {
		return "", fmt.Errorf("settlement: fetch pubkey: %w", err)
	}
	if resp.Status != "success" || resp.Data == "" {
		return "", fmt.Errorf("settlement: pubkey response not success: %q (%s)", resp.Status, resp.Message)
	}
	return resp.Data, nil
}

// RateQuote is the live LP rate the aggregator will accept for an off-ramp
// order of the given (network, token, amount, currency) tuple. Pass the
// returned Rate directly to Gateway.createOrder — any value above this
// causes the order to time-out and auto-refund (no LP will fulfill above
// their posted ceiling). Refresh per-order; rates move on every tick.
type RateQuote struct {
	Rate                 decimal.Decimal
	ProviderIDs          []string
	OrderType            string
	RefundTimeoutMinutes int
}

// FetchRate quotes the current LP sell-side rate. `network` is the
// aggregator's chain identifier ("base", "polygon", …), NOT the EVM
// chain id. `token`
// is the symbol ("USDC"). `amount` is the order amount in human units.
// `currency` is the destination fiat ISO-ish code ("NGN", "GHS", …).
//
// The /v2 endpoint lives at the same origin as /v1 — we strip the /v1
// suffix off our base before composing the URL.
func (c *Client) FetchRate(ctx context.Context, network, token string, amount decimal.Decimal, currency string) (*RateQuote, error) {
	origin := strings.TrimSuffix(c.BaseURL, "/v1")
	endpoint := fmt.Sprintf("%s/v2/rates/%s/%s/%s/%s?side=sell",
		origin,
		url.PathEscape(network),
		url.PathEscape(token),
		amount.String(),
		url.PathEscape(currency),
	)
	var resp struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Sell struct {
				Rate                 string   `json:"rate"`
				ProviderIDs          []string `json:"providerIds"`
				OrderType            string   `json:"orderType"`
				RefundTimeoutMinutes int      `json:"refundTimeoutMinutes"`
			} `json:"sell"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("settlement: fetch rate: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("settlement: rate response not success: %q (%s)", resp.Status, resp.Message)
	}
	if resp.Data.Sell.Rate == "" {
		return nil, fmt.Errorf("settlement: rate response missing data.sell.rate")
	}
	rate, err := decimal.NewFromString(resp.Data.Sell.Rate)
	if err != nil {
		return nil, fmt.Errorf("settlement: rate parse %q: %w", resp.Data.Sell.Rate, err)
	}
	return &RateQuote{
		Rate:                 rate,
		ProviderIDs:          resp.Data.Sell.ProviderIDs,
		OrderType:            resp.Data.Sell.OrderType,
		RefundTimeoutMinutes: resp.Data.Sell.RefundTimeoutMinutes,
	}, nil
}

// FetchOrderStatus polls the aggregator for the current lifecycle state of
// an on-chain order identified by (chainId, orderId). orderId is the
// bytes32 hex returned from the Gateway's createOrder call (0x-prefixed).
func (c *Client) FetchOrderStatus(ctx context.Context, chainID int64, orderID string) (*OrderInfo, error) {
	endpoint := c.BaseURL + "/orders/" + strconv.FormatInt(chainID, 10) + "/" + url.PathEscape(orderID)
	var resp orderResponse
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("settlement: fetch order status: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("settlement: order response not success: %q (%s)", resp.Status, resp.Message)
	}
	return &resp.Data, nil
}

// do is the shared HTTP call helper. Matches services/lifi/client.go's
// shape so future readers don't have to learn two conventions.
func (c *Client) do(ctx context.Context, method, endpoint string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
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
