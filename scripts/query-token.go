//go:build ignore

package main

import (
	"context"
	"fmt"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/network"
	tokenEnt "github.com/usezoracle/rails-sui/ent/token"
	"github.com/usezoracle/rails-sui/storage"
)

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(err)
	}
	defer storage.GetClient().Close()

	ctx := context.Background()

	// 1. List networks
	nets, _ := storage.Client.Network.Query().All(ctx)
	fmt.Println("Networks in DB:")
	for _, n := range nets {
		fmt.Printf(" - ID: %d, Identifier: %s\n", n.ID, n.Identifier)
	}

	// 2. List tokens
	toks, _ := storage.Client.Token.Query().WithNetwork().All(ctx)
	fmt.Println("Tokens in DB:")
	for _, t := range toks {
		fmt.Printf(" - ID: %d, Symbol: %s, Network: %s, Enabled: %v\n", t.ID, t.Symbol, t.Edges.Network.Identifier, t.IsEnabled)
	}

	// 3. Query
	token, err := storage.Client.Token.
		Query().
		Where(
			tokenEnt.SymbolEQ("USDC"),
			tokenEnt.HasNetworkWith(network.IdentifierEQ("sui-testnet")),
			tokenEnt.IsEnabledEQ(true),
		).
		WithNetwork().
		Only(ctx)

	if err != nil {
		fmt.Printf("Query error: %v\n", err)
	} else {
		fmt.Printf("Query success: ID=%d, Symbol=%s\n", token.ID, token.Symbol)
	}
}
