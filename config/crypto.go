package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// CryptoConfiguration holds the RSA keypair used to encrypt and decrypt the
// `recipient` blob that travels on-chain as `message_hash` on every
// OrderCreated event. Sender encrypts with the public key when constructing
// the create_order PTB; the indexer decrypts with the private key.
//
// Distinct from SUI_AGGREGATOR_PRIVATE_KEY (the Ed25519 seed for signing
// Sui transactions, configured in OrderConfiguration).
type CryptoConfiguration struct {
	AggregatorPublicKey  string
	AggregatorPrivateKey string
}

// CryptoConfig loads the RSA keypair from environment / viper.
func CryptoConfig() *CryptoConfiguration {
	return &CryptoConfiguration{
		AggregatorPublicKey:  viper.GetString("AGGREGATOR_PUBLIC_KEY"),
		AggregatorPrivateKey: viper.GetString("AGGREGATOR_PRIVATE_KEY"),
	}
}

func init() {
	if err := SetupConfig(); err != nil {
		panic(fmt.Sprintf("config SetupConfig() error: %s", err))
	}
}
