package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// CryptoConfiguration holds keys + mnemonic used by the aggregator backend.
type CryptoConfiguration struct {
	// Optional HD wallet mnemonic for auxiliary derived accounts. Sui chain
	// signing uses SUI_AGGREGATOR_PRIVATE_KEY (an Ed25519 seed) from
	// OrderConfig, not this mnemonic.
	HDWalletMnemonic string

	// RSA keypair used to encrypt/decrypt recipient bank details carried in
	// the on-chain message_hash field of OrderCreated. Sender encrypts with
	// the public key when building the create_order PTB; aggregator decrypts
	// with the private key in the indexer's handleOrderCreated path.
	AggregatorPublicKey  string
	AggregatorPrivateKey string
}

// CryptoConfig loads the crypto configuration from environment / viper.
func CryptoConfig() *CryptoConfiguration {
	return &CryptoConfiguration{
		HDWalletMnemonic:     viper.GetString("HD_WALLET_MNEMONIC"),
		AggregatorPublicKey:  viper.GetString("AGGREGATOR_PUBLIC_KEY"),
		AggregatorPrivateKey: viper.GetString("AGGREGATOR_PRIVATE_KEY"),
	}
}

func init() {
	if err := SetupConfig(); err != nil {
		panic(fmt.Sprintf("config SetupConfig() error: %s", err))
	}
}
