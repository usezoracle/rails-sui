// Stateless resync-nonce helpers. The cardholder /v1/cards/me/resync
// flow needs a one-shot nonce that proves "the server just issued this
// for you" — but we don't want to round-trip a DB row for it. So we
// HMAC (user_id || card_id || expiry) with the server secret and emit
//
//	<expiry-unix>.<hex-mac>
//
// On verify: split on '.', parse expiry, recompute the MAC over the
// same triple, constant-time compare, check expiry. Stateless,
// replay-prevention is per-request (the MAC ties the nonce to one
// user+card+window).
//
// Note: this is "one-shot per window" not "one-shot per use" — within
// the 5-minute TTL a captured nonce could in theory be replayed by an
// attacker who'd already exfiltrated the user's session. The bigger
// guard against that is the underlying device — if someone's PWA
// session is compromised they have bigger problems than card resync.
// A stricter "single-use" variant would require a DB row + atomic
// consume, which we're deliberately deferring as the security gain
// here is small.

package cards

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/usezoracle/rails-sui/config"
)

func issueResyncNonce(userID, cardID uuid.UUID, expiresAt int64) (string, error) {
	mac := resyncMac(userID, cardID, expiresAt)
	return strconv.FormatInt(expiresAt, 10) + "." + hex.EncodeToString(mac), nil
}

func verifyResyncNonce(userID, cardID uuid.UUID, nonce string) error {
	parts := strings.SplitN(nonce, ".", 2)
	if len(parts) != 2 {
		return errors.New("malformed nonce")
	}
	expiresAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return errors.New("bad nonce expiry")
	}
	if time.Now().Unix() > expiresAt {
		return errors.New("expired")
	}
	given, err := hex.DecodeString(parts[1])
	if err != nil {
		return errors.New("bad nonce mac")
	}
	want := resyncMac(userID, cardID, expiresAt)
	if subtle.ConstantTimeCompare(given, want) != 1 {
		return errors.New("nonce mac mismatch")
	}
	return nil
}

func resyncMac(userID, cardID uuid.UUID, expiresAt int64) []byte {
	secret := []byte(config.AuthConfig().Secret)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("tapp-card-resync-v1"))
	mac.Write(userID[:])
	mac.Write(cardID[:])
	expBytes := []byte(strconv.FormatInt(expiresAt, 10))
	mac.Write(expBytes)
	return mac.Sum(nil)
}
