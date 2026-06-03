package main

import (
	"context"
	"fmt"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/storage"
)

func main() {
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(err)
	}
	defer storage.GetClient().Close()

	ctx := context.Background()

	// Delete in correct order to avoid foreign key violations
	fmt.Println("Deleting child tables...")
	_, _ = storage.Client.SenderOrderToken.Delete().Exec(ctx)
	_, _ = storage.Client.ProviderOrderToken.Delete().Exec(ctx)
	_, _ = storage.Client.SuiReceiveAddress.Delete().Exec(ctx)
	_, _ = storage.Client.ReceiveAddress.Delete().Exec(ctx)
	_, _ = storage.Client.LockOrderFulfillment.Delete().Exec(ctx)
	_, _ = storage.Client.LockPaymentOrder.Delete().Exec(ctx)
	_, _ = storage.Client.PaymentOrderRecipient.Delete().Exec(ctx)
	_, _ = storage.Client.PaymentOrder.Delete().Exec(ctx)
	
	fmt.Println("Deleting tokens and networks...")
	_, err := storage.Client.Token.Delete().Exec(ctx)
	if err != nil {
		fmt.Printf("Error deleting tokens: %v\n", err)
	}
	_, err = storage.Client.Network.Delete().Exec(ctx)
	if err != nil {
		fmt.Printf("Error deleting networks: %v\n", err)
	}

	fmt.Println("Database cleaned!")
}
