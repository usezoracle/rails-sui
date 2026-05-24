package settlement

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	mrand "math/rand"
	"strconv"
	"time"
)

// Recipient is the plaintext payload that gets RSA-PKCS1v15-encrypted under
// the aggregator's pubkey and passed as the Gateway's `messageHash` argument.
//
// Field names + JSON tags MUST match the on-wire schema the upstream
// aggregator expects (today: api.paycrest.io's recipient blob).
type Recipient struct {
	AccountIdentifier string            `json:"accountIdentifier"`
	AccountName       string            `json:"accountName"`
	Institution       string            `json:"institution"`
	Memo              string            `json:"memo,omitempty"`
	ProviderID        string            `json:"providerId,omitempty"`
	Nonce             string            `json:"nonce"`
	Metadata          map[string]string `json:"metadata"` // {"apiKey": senderApiKeyID}
}

// NewNonce returns a 12-char nonce: 6 chars of unix-millis base36 + 5 chars
// random base36. Used by the aggregator for replay protection on
// duplicate-order detection.
func NewNonce() string {
	now := strconv.FormatInt(time.Now().UnixMilli(), 36)
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = charset[mrand.Intn(len(charset))]
	}
	return now + string(b)
}

// EncryptRecipient returns the base64-encoded RSA-PKCS1v15 ciphertext that
// `Gateway.createOrder` expects as its `messageHash` string argument.
//
// pemPEM is a PEM-encoded RSA public key (the body the aggregator serves
// at GET /v1/pubkey). Errors thrown:
//
//   - "invalid PEM": pemPEM didn't contain a parseable PEM block
//   - "not an RSA key": PEM decoded but wasn't an RSA pubkey
//   - any error from rsa.EncryptPKCS1v15 (e.g. plaintext too long for key)
func EncryptRecipient(r Recipient, pemPEM string) (string, error) {
	if r.Metadata == nil {
		r.Metadata = map[string]string{}
	}
	if r.Nonce == "" {
		r.Nonce = NewNonce()
	}

	plain, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal recipient: %w", err)
	}

	block, _ := pem.Decode([]byte(pemPEM))
	if block == nil {
		return "", errors.New("settlement: invalid PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		// Older PEMs may use PKCS#1 RSAPublicKey directly.
		alt, altErr := x509.ParsePKCS1PublicKey(block.Bytes)
		if altErr != nil {
			return "", fmt.Errorf("parse pubkey: %w", err)
		}
		pub = alt
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("settlement: pubkey is not an RSA key")
	}

	cipher, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, plain)
	if err != nil {
		return "", fmt.Errorf("rsa encrypt: %w", err)
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}
