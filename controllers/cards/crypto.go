// Crypto helpers for the Tap Card vertical.
//
// Three concerns live here, kept narrow so the controller code reads
// like business logic instead of crypto plumbing:
//
//   1. Server nonce generation (32 random bytes, hex-encoded)
//   2. PIN response verification — HMAC-SHA256 challenge-response
//      against the `linking_proof` the PWA committed at link time.
//      Constant-time compare to defeat timing oracles.
//   3. Rotation token + card password generation.
//
// See `rails/docs/tapp-card-spec.md` Appendix A for the math.

package cards

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
)

const (
	serverNonceLen   = 32 // bytes
	rotationTokenLen = 32 // bytes — opaque, server-only entropy
	cardPasswordLen  = 4  // NTAG215 PWD is 32 bits
	hmacOutputLen    = 32 // SHA-256 output
)

// GenerateServerNonce returns a fresh 32-byte nonce for a debit
// request. Caller persists it on CardServerNonce.nonce and emits the
// hex form to the merchant app.
func GenerateServerNonce() ([]byte, error) {
	buf := make([]byte, serverNonceLen)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// GenerateRotationToken returns 32 random bytes. The bytes are stored
// verbatim on TappCard.current_token_ciphertext AND written to the
// card sector — comparison on the next debit is byte-equality. No
// encryption needed because the card holds opaque randomness; the
// only thing that matters is that the value is server-generated and
// uncompromised.
func GenerateRotationToken() ([]byte, error) {
	buf := make([]byte, rotationTokenLen)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// GenerateCardPassword returns a fresh 4-byte NTAG215 PWD. Called
// during PWA linking; stored on TappCard.card_password and returned
// to the merchant on every debit so it can PWD_AUTH before write.
func GenerateCardPassword() ([]byte, error) {
	buf := make([]byte, cardPasswordLen)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// VerifyPinResponse checks whether `pinResponse` matches what we'd
// expect given the cardholder's stored `linkingProof` and the
// per-debit `serverNonce`.
//
//	expected = HMAC-SHA256(linkingProof, serverNonce)
//
// Returns false (no error) for plain mismatch; error only for
// malformed input (lets the caller distinguish 403 from 400).
func VerifyPinResponse(linkingProof, serverNonce, pinResponse []byte) (bool, error) {
	if len(linkingProof) != hmacOutputLen {
		return false, errors.New("linking_proof must be 32 bytes")
	}
	if len(serverNonce) != serverNonceLen {
		return false, errors.New("server_nonce must be 32 bytes")
	}
	if len(pinResponse) != hmacOutputLen {
		return false, errors.New("pin_response must be 32 bytes")
	}

	mac := hmac.New(sha256.New, linkingProof)
	mac.Write(serverNonce)
	expected := mac.Sum(nil)
	return subtle.ConstantTimeCompare(expected, pinResponse) == 1, nil
}

// MustDecodeHex panics on bad hex. Use only with values we control or
// have already validated at the API boundary.
func MustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// DecodeHex is the boundary-safe variant.
func DecodeHex(s string) ([]byte, error) {
	return hex.DecodeString(s)
}

// EncodeHex returns lowercase hex.
func EncodeHex(b []byte) string { return hex.EncodeToString(b) }
