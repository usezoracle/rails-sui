package main

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	orderpkg "github.com/usezoracle/rails-sui/services/order"
	"github.com/usezoracle/rails-sui/storage"
)

func main() {
	ctx := context.Background()

	// Initialize config
	config.ServerConfig()

	// Connect to database
	DSN := config.DBConfig()
	if err := storage.DBConnection(DSN); err != nil {
		panic(err)
	}
	defer storage.GetClient().Close()

	// Initialize Redis
	if err := storage.InitializeRedis(); err != nil {
		panic(err)
	}

	// 1. Setup OrderSui service
	var orderSui *orderpkg.OrderSui
	if svc, ok := orderpkg.NewOrderSui().(*orderpkg.OrderSui); ok {
		orderSui = svc
	} else {
		panic("Failed to instantiate OrderSui")
	}

	gatewayOrderID := "0xea390cdfae9d5030309d6e905fb84f9f35ff74d8ecfbbf54e015a6a75ec72526"
	coinType := "0xdba34672e30cb065b1f93e3ab55318768fd6fef66c15942c9f7cb846e2f900e7::usdc::USDC"
	orderUUIDStr := "4084b178-b869-4036-902d-32f404dbf07a"
	txDigest := "2e1g6vHNisV8QfXdgHcQc8dRTinvnCfcHrjYD81xStCv"

	orderUUID, err := uuid.Parse(orderUUIDStr)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Starting manual self-settle for order %s...\n", orderUUIDStr)

	// Call on-chain SelfSettleToAggregator
	err = orderSui.SelfSettleToAggregator(ctx, gatewayOrderID, coinType)
	if err != nil {
		fmt.Printf("SelfSettleToAggregator on-chain call failed: %v\n", err)
		return
	}

	fmt.Printf("On-chain self-settle succeeded! Now updating database for order %s...\n", orderUUIDStr)

	// Update PaymentOrder table
	po, err := storage.GetClient().PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(orderUUID)).
		Only(ctx)
	if err != nil {
		panic(fmt.Errorf("lookup payment order: %w", err))
	}

	_, err = po.Update().
		SetGatewayID(gatewayOrderID).
		SetTxHash(txDigest).
		Save(ctx)
	if err != nil {
		panic(fmt.Errorf("save payment order: %w", err))
	}

	fmt.Printf("Successfully updated database! Order %s marked with gatewayID and txHash.\n", orderUUIDStr)
}
