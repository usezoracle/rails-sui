//go:build ignore

package main

import (
	"context"
	"fmt"
	"log"

	"github.com/usezoracle/rails-sui/config"
	userEnt "github.com/usezoracle/rails-sui/ent/user"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/block-vision/sui-go-sdk/sui"
	"github.com/block-vision/sui-go-sdk/models"
)

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		log.Fatalf("database DBConnection: %s", err)
	}
	defer storage.GetClient().Close()

	ctx := context.Background()
	userEmail := "silascyrax@gmail.com" // From the screenshot
	u, err := storage.GetClient().User.Query().
		Where(userEnt.EmailEQ(userEmail)).
		WithTappCards().
		Only(ctx)
	if err != nil {
		log.Fatalf("Failed to query user: %v", err)
	}

	fmt.Printf("User: ID=%s, Email=%s\n", u.ID, u.Email)
	fmt.Printf("Cards: %d\n", len(u.Edges.TappCards))

	for i, card := range u.Edges.TappCards {
		fmt.Printf("Card %d: ID=%s, Status=%s, CapObjectID=%v, CoinType=%v\n",
			i, card.ID, card.Status, card.CapObjectID, card.CoinType)
		if card.CapObjectID != nil && *card.CapObjectID != "" {
			client := sui.NewSuiClient(config.OrderConfig().SuiRpcURL)
			resp, err := client.SuiGetObject(ctx, models.SuiGetObjectRequest{
				ObjectId: *card.CapObjectID,
				Options: models.SuiObjectDataOptions{
					ShowContent: true,
					ShowOwner: true,
				},
			})
			if err != nil {
				fmt.Printf("  SuiGetObject Error: %v\n", err)
			} else {
				fmt.Printf("  On-chain Object found!\n")
				if resp.Data != nil {
					fmt.Printf("    Owner: %+v\n", resp.Data.Owner)
					if resp.Data.Content != nil {
						fmt.Printf("    Fields: %+v\n", resp.Data.Content.Fields)
					}
				}
			}
		}
	}
}
