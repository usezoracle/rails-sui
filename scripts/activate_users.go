//go:build ignore

package main

import (
	"context"
	"fmt"
	"github.com/shopspring/decimal"
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
	senders, err := client.SenderProfile.Query().WithUser().All(ctx)
	if err != nil {
		panic(err)
	}

	tokens, err := client.Token.Query().All(ctx)
	if err != nil {
		panic(err)
	}

	fmt.Println("Activating and configuring senders...")
	for _, s := range senders {
		var email string
		if s.Edges.User != nil {
			email = s.Edges.User.Email
		}

		// Activate sender
		if !s.IsActive {
			_, err = client.SenderProfile.UpdateOne(s).SetIsActive(true).Save(ctx)
			if err != nil {
				fmt.Printf("Failed to activate sender %s: %v\n", email, err)
				continue
			}
			fmt.Printf("Activated sender: %s\n", email)
		}

		// Check if sender has order tokens configured
		count, err := client.SenderOrderToken.Query().
			Where(senderordertoken.HasSenderWith(senderprofile.IDEQ(s.ID))).
			Count(ctx)
		if err != nil {
			fmt.Printf("Failed to count tokens for %s: %v\n", email, err)
			continue
		}

		if count == 0 {
			fmt.Printf("Configuring tokens for sender: %s\n", email)
			for _, t := range tokens {
				_, err = client.SenderOrderToken.
					Create().
					SetSender(s).
					SetToken(t).
					SetFeePercent(decimal.NewFromFloat(0.01)). // 1% fee
					SetFeeAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8").
					SetRefundAddress("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC").
					Save(ctx)
				if err != nil {
					fmt.Printf("Failed to configure token %s for sender %s: %v\n", t.Symbol, email, err)
				} else {
					fmt.Printf("  - Token %s configured successfully\n", t.Symbol)
				}
			}
		} else {
			fmt.Printf("Sender %s already has %d tokens configured\n", email, count)
		}
	}
	fmt.Println("All senders successfully activated and configured!")
}
