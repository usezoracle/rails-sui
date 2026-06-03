//go:build ignore

package main

import (
	"context"
	"fmt"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/senderordertoken"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/storage"
)

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(err)
	}
	client := storage.GetClient()
	defer client.Close()

	ctx := context.Background()
	users, err := client.User.Query().All(ctx)
	if err != nil {
		panic(err)
	}

	fmt.Println("=== Users ===")
	for _, u := range users {
		fmt.Printf("ID: %s, Email: %s, Scope: %s\n", u.ID, u.Email, u.Scope)
	}

	senders, err := client.SenderProfile.Query().WithUser().All(ctx)
	if err != nil {
		panic(err)
	}

	fmt.Println("\n=== Senders ===")
	for _, s := range senders {
		var email string
		if s.Edges.User != nil {
			email = s.Edges.User.Email
		}
		fmt.Printf("Sender ID: %s, Email: %s, Active: %v\n", s.ID, email, s.IsActive)

		tokens, err := client.SenderOrderToken.Query().
			Where(senderordertoken.HasSenderWith(senderprofile.IDEQ(s.ID))).
			WithToken().
			All(ctx)
		if err != nil {
			fmt.Printf("Error getting tokens: %v\n", err)
			continue
		}
		for _, sot := range tokens {
			if sot.Edges.Token != nil {
				fmt.Printf("  - Token: %s, FeeAddr: %s, RefundAddr: %s\n",
					sot.Edges.Token.Symbol, sot.FeeAddress, sot.RefundAddress)
			}
		}
	}
}
