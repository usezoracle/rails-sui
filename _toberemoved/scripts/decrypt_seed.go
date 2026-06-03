package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	"github.com/usezoracle/rails-sui/storage"
	cryptoUtils "github.com/usezoracle/rails-sui/utils/crypto"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run scripts/decrypt_seed.go <receive_address>")
		os.Exit(1)
	}
	targetAddr := os.Args[1]

	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(err)
	}
	client := storage.GetClient()
	defer client.Close()

	ctx := context.Background()
	row, err := client.SuiReceiveAddress.
		Query().
		Where(suireceiveaddress.AddressEQ(targetAddr)).
		Only(ctx)
	if err != nil {
		fmt.Printf("Error fetching receive address row: %v\n", err)
		os.Exit(1)
	}

	seed, err := cryptoUtils.DecryptPlain(row.EncryptedSeed)
	if err != nil {
		fmt.Printf("Error decrypting seed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Decrypted Seed Hex: %s\n", hex.EncodeToString(seed))
}
