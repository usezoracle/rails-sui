//go:build ignore

// One-shot helper to convert a Sui bech32 private key (suiprivkey1...) into
// the 32-byte hex Ed25519 seed the Rails backend expects in
// SUI_AGGREGATOR_PRIVATE_KEY.
//
// Usage:
//   go run scripts/export-key.go suiprivkey1...
package main

import (
	"encoding/hex"
	"fmt"
	"os"

	suisigner "github.com/block-vision/sui-go-sdk/signer"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/export-key.go suiprivkey1...")
		os.Exit(1)
	}
	s, err := suisigner.NewSignerWithSecretKey(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode error:", err)
		os.Exit(1)
	}
	fmt.Println("address:", s.Address)
	fmt.Println("SUI_AGGREGATOR_PRIVATE_KEY=" + hex.EncodeToString(s.PriKey[:32]))
}
