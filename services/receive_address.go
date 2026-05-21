// Package services — receive_address.go handles per-order deposit address
// generation for the Path-2 (exchange / external wallet) deposit flow on
// Sui. See rails-architecture.md "Path 2 — Receive address" for the full
// flow.
package services

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/blake2b"

	cryptoUtils "github.com/usezoracle/rails-sui/utils/crypto"
)

// suiSignatureSchemeFlagEd25519 prefixes the public key before blake2b256-
// hashing to derive the on-chain Sui address — distinguishes Ed25519 from
// Secp256k1 / Secp256r1 / multisig / zkLogin schemes.
const suiSignatureSchemeFlagEd25519 byte = 0x00

// ReceiveAddressService manages per-order receive addresses used in the
// Path-2 deposit flow (exchange / external wallet payments).
type ReceiveAddressService struct{}

// NewReceiveAddressService creates a new ReceiveAddressService.
func NewReceiveAddressService() *ReceiveAddressService {
	return &ReceiveAddressService{}
}

// CreateSuiReceiveAddress generates a fresh Ed25519 keypair, derives the Sui
// address, encrypts the 32-byte seed with the protocol's AES master key,
// and returns (address, encryptedSeed). The caller persists the result in a
// SuiReceiveAddress entity associated with a PaymentOrder.
//
// Address derivation follows the Sui spec:
//
//	address = blake2b256( flag_byte || pubKey )[:32]
//
// where flag_byte = 0x00 for Ed25519. The result is the canonical 32-byte
// Sui address, returned 0x-prefixed.
func (s *ReceiveAddressService) CreateSuiReceiveAddress(ctx context.Context) (address string, encryptedSeed []byte, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("ed25519 keygen: %w", err)
	}

	preimage := make([]byte, 0, 1+len(pubKey))
	preimage = append(preimage, suiSignatureSchemeFlagEd25519)
	preimage = append(preimage, pubKey...)

	digest := blake2b.Sum256(preimage)
	address = "0x" + hex.EncodeToString(digest[:])

	encryptedSeed, err = cryptoUtils.EncryptPlain(privKey.Seed())
	if err != nil {
		return "", nil, fmt.Errorf("encrypt seed: %w", err)
	}

	return address, encryptedSeed, nil
}
