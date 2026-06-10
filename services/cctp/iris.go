package cctp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors the poller branches on. Everything else is a transport/decode
// error: log and retry next tick.
var (
	// ErrNotIndexed — Circle hasn't seen the burn tx yet (404). Normal
	// for the first ~1 min after submit; stale-window logic decides when
	// it stops being normal.
	ErrNotIndexed = errors.New("cctp: burn tx not indexed by Circle yet")
	// ErrAttestationPending — burn indexed, attestation not signed yet
	// (waiting on Sui finality). Keep polling.
	ErrAttestationPending = errors.New("cctp: attestation pending")
)

// Iris is the client for Circle's v1 attestation service.
// Rate limit upstream is 35 req/s — the dispatcher polls once per
// order per minute, so we stay orders of magnitude under it.
type Iris struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewIris builds a client for the given host (Network.IrisBaseURL).
func NewIris(baseURL string) *Iris {
	return &Iris{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// AttestedMessage is one entry from /v1/messages — the attested burn
// ready (or not) for redemption.
type AttestedMessage struct {
	// Message is the raw CCTP message bytes (decoded from 0x-hex).
	Message []byte
	// Attestation is Circle's signature bundle over keccak256(Message),
	// passed verbatim to receiveMessage. Nil until complete.
	Attestation []byte
}

type irisMessagesResponse struct {
	Messages []struct {
		Attestation string `json:"attestation"` // "PENDING" until signed
		Message     string `json:"message"`
		EventNonce  string `json:"eventNonce"`
	} `json:"messages"`
	Error string `json:"error"`
}

// MessageFor fetches the burn message + attestation for a source-chain
// tx (Sui digest, passed verbatim). Stateless by design: the digest we
// already persist on the order row is the only key needed to resume the
// bridge after any crash or redeploy.
//
// Returns ErrNotIndexed / ErrAttestationPending for the two expected
// wait states. A tx with multiple burns isn't something our PTB can
// produce (one deposit_for_burn per bridge), so >1 message is an error.
func (i *Iris) MessageFor(ctx context.Context, sourceDomain uint32, txHash string) (*AttestedMessage, error) {
	u := fmt.Sprintf("%s/v1/messages/%d/%s", i.BaseURL, sourceDomain, url.PathEscape(txHash))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("cctp: build iris request: %w", err)
	}
	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cctp: iris request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cctp: read iris response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotIndexed
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cctp: iris status %d: %s", resp.StatusCode, truncate(body, 200))
	}

	var parsed irisMessagesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("cctp: decode iris response: %w", err)
	}
	if len(parsed.Messages) == 0 {
		return nil, ErrNotIndexed
	}
	if len(parsed.Messages) > 1 {
		return nil, fmt.Errorf("cctp: %d messages for tx %s, expected exactly 1", len(parsed.Messages), txHash)
	}

	entry := parsed.Messages[0]
	msg, err := decodeHex(entry.Message)
	if err != nil {
		return nil, fmt.Errorf("cctp: decode message hex: %w", err)
	}
	out := &AttestedMessage{Message: msg}

	if strings.EqualFold(entry.Attestation, "PENDING") || entry.Attestation == "" {
		return out, ErrAttestationPending
	}
	att, err := decodeHex(entry.Attestation)
	if err != nil {
		return nil, fmt.Errorf("cctp: decode attestation hex: %w", err)
	}
	out.Attestation = att
	return out, nil
}

func decodeHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
