package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/usezoracle/rails-sui/config"
)

var conf = config.AuthConfig()

// GenerateAccessJWT issues a short-lived access token with the full
// RFC-7519 claim set (sub, iat, nbf, exp, iss, aud, jti, scope).
//
// jti is included so individual access tokens can be denylisted in
// future (e.g. compromise-revoke) without needing a JWT-replacement.
// iss/aud anchor validation to this deployment — tokens from a sibling
// service would fail to validate.
func GenerateAccessJWT(userID string, scope string) (string, error) {
	now := time.Now()
	jti, err := newJTI()
	if err != nil {
		return "", err
	}
	token := jwt.New(jwt.SigningMethodHS256)
	claims := token.Claims.(jwt.MapClaims)
	claims["sub"]   = userID
	claims["scope"] = scope
	claims["iat"]   = now.Unix()
	claims["nbf"]   = now.Unix()
	claims["exp"]   = now.Add(conf.JwtAccessLifespan).Unix()
	claims["iss"]   = conf.JwtIssuer
	claims["aud"]   = conf.JwtAudience
	claims["jti"]   = jti
	return token.SignedString([]byte(conf.JwtSigningKey))
}

// ValidateJWT validates the token string and returns its claims.
// Requires iss/aud to match the deployment so tokens minted by another
// service can't slip through.
func ValidateJWT(tokenString string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(conf.JwtSigningKey), nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(conf.JwtIssuer),
		jwt.WithAudience(conf.JwtAudience),
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

// newJTI returns a random 128-bit token id, hex-encoded.
func newJTI() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GeneratePrivateKey generates a private key (Secret Key).
func GeneratePrivateKey() (string, error) {
	// Generate random bytes for the key
	keySize := 32 // 32 bytes = 256 bits -- for HMAC-SHA256 hashing function
	privateKeyBytes := make([]byte, keySize)
	_, err := rand.Read(privateKeyBytes)
	if err != nil {
		return "", err
	}

	// Encode private key to base64 string
	privateKey := base64.URLEncoding.EncodeToString(privateKeyBytes)

	return privateKey, nil
}

// VerifyHMACSignature verifies the HMAC signature for the given payload using the private key
// and returns true if the signature is valid.
func VerifyHMACSignature(payload map[string]interface{}, privateKey string, signature string) bool {
	expectedSignature := []byte(GenerateHMACSignature(payload, privateKey))
	computedSignature := []byte(signature)
	return hmac.Equal(expectedSignature, computedSignature)
}

// GenerateHMACSignature generates the HMAC signature for the given payload using the private key.
// The signature is returned as a hex-encoded string.
func GenerateHMACSignature(payload map[string]interface{}, privateKey string) string {
	key := []byte(privateKey)
	h := hmac.New(sha256.New, key)
	payload = SortMapRecursively(payload)
	payloadBytes, _ := json.Marshal(payload)
	h.Write(payloadBytes)
	return hex.EncodeToString(h.Sum(nil))
}

// SortMapRecursively sorts a map recursively by its keys
func SortMapRecursively(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := m[k]
		switch v := v.(type) {
		case map[string]interface{}:
			result[k] = SortMapRecursively(v)
		case []interface{}:
			result[k] = SortSliceRecursively(v)
		default:
			result[k] = v
		}
	}
	return result
}

// SortMapRecursively sorts a map recursively by its keys
func SortSliceRecursively(s []interface{}) []interface{} {
	for i, v := range s {
		switch v := v.(type) {
		case map[string]interface{}:
			s[i] = SortMapRecursively(v)
		case []interface{}:
			s[i] = SortSliceRecursively(v)
		}
	}
	return s
}
