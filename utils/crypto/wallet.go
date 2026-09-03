package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/crypto"
)

// Default master key fallback for dev/demo if WALLET_MASTER_KEY is unset (32 bytes = 256-bit AES)
const defaultMasterKey = "4f8a2b9c7d1e3f5a6b8c0d2e4f6a8b1c3d5e7f9a1b3c5d7e9f2a4b6c8d0e1f3a"

// GenerateEVMWallet creates a new secp256k1 EVM wallet keypair.
// Returns the 0x-prefixed EVM address, the AES-256-GCM encrypted private key (hex), and any error.
func GenerateEVMWallet(masterKeyHex string) (string, string, error) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		return "", "", fmt.Errorf("generate keypair: %w", err)
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	privKeyBytes := crypto.FromECDSA(privateKey)

	encryptedHex, err := EncryptPrivateKey(privKeyBytes, masterKeyHex)
	if err != nil {
		return "", "", fmt.Errorf("encrypt private key: %w", err)
	}

	return address, encryptedHex, nil
}

// EncryptPrivateKey encrypts raw private key bytes using AES-256-GCM.
func EncryptPrivateKey(privKeyBytes []byte, masterKeyHex string) (string, error) {
	key := getMasterKeyBytes(masterKeyHex)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm cipher: %w", err)
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce generation: %w", err)
	}

	ciphertext := aesGCM.Seal(nonce, nonce, privKeyBytes, nil)
	return hex.EncodeToString(ciphertext), nil
}

// DecryptEVMPrivateKey decrypts an AES-256-GCM encrypted private key string.
func DecryptEVMPrivateKey(encryptedHex string, masterKeyHex string) ([]byte, error) {
	data, err := hex.DecodeString(encryptedHex)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}

	key := getMasterKeyBytes(masterKeyHex)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm cipher: %w", err)
	}

	nonceSize := aesGCM.NonceSize()
	if len(data) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	privKeyBytes, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed: %w", err)
	}

	return privKeyBytes, nil
}

func getMasterKeyBytes(masterKeyHex string) []byte {
	if masterKeyHex == "" {
		masterKeyHex = defaultMasterKey
	}
	b, err := hex.DecodeString(masterKeyHex)
	if err != nil || len(b) != 32 {
		// Fallback to default 32-byte key
		b, _ = hex.DecodeString(defaultMasterKey)
	}
	return b
}
