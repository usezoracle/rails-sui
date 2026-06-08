package mfb

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeyPEM returns a fresh PKCS#8 RSA private key in PEM form.
func testKeyPEM(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), key
}

func TestNewAuthenticator_Validation(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)

	if _, err := NewAuthenticator(Config{PrivateKeyPEM: keyPEM}); err == nil {
		t.Fatal("expected error when ClientID is missing")
	}
	if _, err := NewAuthenticator(Config{ClientID: "cid"}); err == nil {
		t.Fatal("expected error when PrivateKeyPEM is missing")
	}
	if _, err := NewAuthenticator(Config{ClientID: "cid", PrivateKeyPEM: "not a key"}); err == nil {
		t.Fatal("expected error for malformed key")
	}
}

func TestNewAuthenticator_Defaults(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	a, err := NewAuthenticator(Config{ClientID: "cid", PrivateKeyPEM: keyPEM})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if a.BaseURL() != ProdBaseURL {
		t.Errorf("BaseURL = %q, want prod default %q", a.BaseURL(), ProdBaseURL)
	}
	if a.cfg.Audience != ProdBaseURL {
		t.Errorf("Audience = %q, want %q", a.cfg.Audience, ProdBaseURL)
	}
	if a.cfg.Issuer != "cid" {
		t.Errorf("Issuer = %q, want ClientID fallback", a.cfg.Issuer)
	}
}

func TestSignAssertion_Claims(t *testing.T) {
	keyPEM, key := testKeyPEM(t)
	a, err := NewAuthenticator(Config{
		ClientID:      "my-client-id",
		PrivateKeyPEM: keyPEM,
		BaseURL:       SandboxBaseURL,
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	tokenStr, err := a.signAssertion()
	if err != nil {
		t.Fatalf("signAssertion: %v", err)
	}

	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tokenStr, claims, func(tok *jwt.Token) (any, error) {
		if tok.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			t.Fatalf("alg = %s, want RS256", tok.Method.Alg())
		}
		return &key.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("verify assertion: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("assertion did not verify against the public key")
	}

	if got := claims["sub"]; got != "my-client-id" {
		t.Errorf("sub = %v, want my-client-id", got)
	}
	if got := claims["iss"]; got != "my-client-id" {
		t.Errorf("iss = %v, want ClientID fallback", got)
	}
	if got := claims["aud"]; got != SandboxBaseURL {
		t.Errorf("aud = %v, want %s", got, SandboxBaseURL)
	}
	if got := claims["typ"]; got != "JWT" {
		t.Errorf("typ = %v, want JWT", got)
	}

	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		t.Fatalf("iat claim: %v", err)
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("exp claim: %v", err)
	}
	if d := exp.Sub(iat.Time); d < assertionTTL-time.Second || d > assertionTTL+time.Second {
		t.Errorf("exp-iat = %s, want ~%s", d, assertionTTL)
	}
}
