package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// GenerateOpaqueToken returns a cryptographically random, URL-safe
// opaque token suitable for email-verification, password-reset, and
// refresh-token use.
//
//   - 32 bytes of entropy from crypto/rand (~256 bits)
//   - base64.RawURLEncoding for the wire form (URL/email safe, no padding)
//   - never write the raw token to the DB — store HashToken(raw) instead
//     and compare hashes on verify.
func GenerateOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the SHA-256 of a token, hex-encoded. Used to compare
// a submitted token against the at-rest hash stored in the DB.
//
// SHA-256 is sufficient here because the input already has 256 bits of
// entropy — bcrypt/argon2 would be overkill (those defend against
// brute-force on low-entropy passwords; opaque random tokens already
// resist brute force by construction).
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
