// Package mfb integrates the BaaS provider MFB's Banking-as-a-Service API —
// the NGN fiat rail behind Route C (our managed liquidity) and Route A's
// treasury payout mode. The merchant-payout primitive both routes share is
// the BaaS provider's name-enquiry → transfer → status flow.
//
// This file owns authentication. the BaaS provider uses OAuth2 with a signed JWT
// client assertion (RFC 7523), not a static API key:
//
//  1. We hold an RSA private key; the matching public cert is uploaded to the
//     the BaaS provider dashboard for our app.
//  2. On each token fetch we mint a short-lived RS256 JWT (the "client
//     assertion") and POST it to /oauth2/token with grant_type=client_credentials.
//  3. The response carries access_token (~1h), refresh_token, and the BaaS provider's
//     internal ibs_client_id / ibs_user_id. Subsequent calls send
//     Authorization: Bearer <access_token>.
//
// Authenticator caches the token and refreshes it (refresh_token grant first,
// falling back to a fresh assertion) just before expiry. It is concurrency
// safe — Route C settlement and Route A dispatch can both call Token freely.
package mfb

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// ProdBaseURL and SandboxBaseURL are the BaaS provider's API roots. Pick via
	// Config.BaseURL; the JWT "aud" must match the environment you POST to —
	// mixing sandbox and prod is the most common auth failure.
	ProdBaseURL    = "https://api.safehavenmfb.com"
	SandboxBaseURL = "https://api.sandbox.safehavenmfb.com"

	tokenPath           = "/oauth2/token"
	clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	// assertionTTL keeps the signed client assertion short-lived to limit
	// exposure if intercepted (the BaaS provider's guidance is ~5 minutes).
	assertionTTL = 5 * time.Minute
	// refreshSkew refetches the access token this far ahead of its stated
	// expiry so an in-flight request never races a server-side expiry.
	refreshSkew = 60 * time.Second
)

// Config holds the credentials and environment for a the BaaS provider app.
type Config struct {
	// ClientID is the OAuth Client ID from the the BaaS provider dashboard. Becomes
	// the JWT "sub" and the client_id form field.
	ClientID string
	// PrivateKeyPEM is the RSA private key (PKCS#1 or PKCS#8 PEM) whose public
	// cert is registered on the dashboard. Escaped "\n" sequences (common when
	// the key is stored in a single-line env var) are normalised on load.
	PrivateKeyPEM string
	// BaseURL is the API root; defaults to ProdBaseURL.
	BaseURL string
	// Audience is the JWT "aud" claim; defaults to BaseURL.
	Audience string
	// Issuer is the JWT "iss" claim; defaults to ClientID.
	Issuer string
	// HTTPClient is optional; a 20s-timeout client is used when nil.
	HTTPClient *http.Client
}

// Authenticator mints and caches the BaaS provider access tokens.
type Authenticator struct {
	cfg        Config
	privateKey *rsa.PrivateKey
	httpClient *http.Client

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	ibsClientID  string
	ibsUserID    string
}

// NewAuthenticator validates config, parses the private key, and returns a
// ready Authenticator. It performs no network I/O — the first Token call
// fetches the access token lazily.
func NewAuthenticator(cfg Config) (*Authenticator, error) {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("mfb: ClientID is required")
	}
	if strings.TrimSpace(cfg.PrivateKeyPEM) == "" {
		return nil, fmt.Errorf("mfb: PrivateKeyPEM is required")
	}
	// Env vars frequently store the PEM with literal "\n"; restore newlines.
	pem := strings.ReplaceAll(cfg.PrivateKeyPEM, `\n`, "\n")
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pem))
	if err != nil {
		return nil, fmt.Errorf("mfb: parse private key: %w", err)
	}

	if cfg.BaseURL == "" {
		cfg.BaseURL = ProdBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Audience == "" {
		cfg.Audience = cfg.BaseURL
	}
	if cfg.Issuer == "" {
		cfg.Issuer = cfg.ClientID
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}

	return &Authenticator{cfg: cfg, privateKey: key, httpClient: hc}, nil
}

// BaseURL returns the configured API root (no trailing slash).
func (a *Authenticator) BaseURL() string { return a.cfg.BaseURL }

// IBSClientID returns the BaaS provider's internal client id from the last token
// exchange (empty until the first successful Token call). Some endpoints
// require it in the request body.
func (a *Authenticator) IBSClientID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ibsClientID
}

// Token returns a valid bearer access token, fetching or refreshing as needed.
// Safe for concurrent use.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.expiresAt.Add(-refreshSkew)) {
		return a.accessToken, nil
	}

	// Prefer the cheap refresh-token grant; fall back to a fresh assertion if
	// we have no refresh token or the refresh is rejected (e.g. expired).
	if a.refreshToken != "" {
		if err := a.exchange(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {a.cfg.ClientID},
			"refresh_token": {a.refreshToken},
		}); err == nil {
			return a.accessToken, nil
		}
		a.refreshToken = "" // poisoned; force a full re-auth below
	}

	assertion, err := a.signAssertion()
	if err != nil {
		return "", err
	}
	if err := a.exchange(ctx, url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {a.cfg.ClientID},
		"client_assertion_type": {clientAssertionType},
		"client_assertion":      {assertion},
	}); err != nil {
		return "", err
	}
	return a.accessToken, nil
}

// signAssertion builds and signs the RS256 client-assertion JWT.
func (a *Authenticator) signAssertion() (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": a.cfg.Issuer,
		"sub": a.cfg.ClientID,
		"aud": a.cfg.Audience,
		"iat": now.Unix(),
		"exp": now.Add(assertionTTL).Unix(),
		"typ": "JWT",
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.privateKey)
	if err != nil {
		return "", fmt.Errorf("mfb: sign client assertion: %w", err)
	}
	return signed, nil
}

// exchange POSTs a form to the token endpoint and stores the result. The
// caller must hold a.mu.
func (a *Authenticator) exchange(ctx context.Context, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mfb: token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mfb: token http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("mfb: decode token response: %w (body: %s)", err, string(body))
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("mfb: token response missing access_token (body: %s)", string(body))
	}

	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 3600 // conservative default if the field is absent
	}
	a.accessToken = tr.AccessToken
	a.expiresAt = time.Now().Add(time.Duration(ttl) * time.Second)
	if tr.RefreshToken != "" {
		a.refreshToken = tr.RefreshToken
	}
	if tr.IBSClientID != "" {
		a.ibsClientID = tr.IBSClientID
	}
	if tr.IBSUserID != "" {
		a.ibsUserID = tr.IBSUserID
	}
	return nil
}
