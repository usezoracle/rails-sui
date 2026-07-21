package mfb

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
)

func TestNewAuthenticator_PrivateKeyParsing(t *testing.T) {
	// Generate a temporary RSA private key for testing
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test private key: %v", err)
	}

	// Encode to PEM format
	privBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	pemBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	}
	pemBytes := pem.EncodeToMemory(pemBlock)
	rawPEMString := string(pemBytes)

	tests := []struct {
		name          string
		privateKeyPEM string
		wantErr       bool
	}{
		{
			name:          "Standard PEM",
			privateKeyPEM: rawPEMString,
			wantErr:       false,
		},
		{
			name:          "PEM with escaped newlines",
			privateKeyPEM: strings.ReplaceAll(rawPEMString, "\n", `\n`),
			wantErr:       false,
		},
		{
			name:          "PEM wrapped in double quotes",
			privateKeyPEM: fmt.Sprintf(`"%s"`, rawPEMString),
			wantErr:       false,
		},
		{
			name:          "PEM wrapped in double quotes with escaped newlines",
			privateKeyPEM: fmt.Sprintf(`"%s"`, strings.ReplaceAll(rawPEMString, "\n", `\n`)),
			wantErr:       false,
		},
		{
			name:          "PEM wrapped in single quotes with escaped newlines",
			privateKeyPEM: fmt.Sprintf(`'%s'`, strings.ReplaceAll(rawPEMString, "\n", `\n`)),
			wantErr:       false,
		},
		{
			name:          "Invalid key",
			privateKeyPEM: "not-a-valid-key",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAuthenticator(Config{
				ClientID:      "test-client-id",
				PrivateKeyPEM: tt.privateKeyPEM,
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("NewAuthenticator() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
