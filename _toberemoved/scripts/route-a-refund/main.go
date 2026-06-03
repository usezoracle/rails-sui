package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	orderpkg "github.com/usezoracle/rails-sui/services/order"
	"github.com/usezoracle/rails-sui/storage"
	cryptoUtils "github.com/usezoracle/rails-sui/utils/crypto"
)

func main() {
	var (
		paymentOrderID = flag.String("payment-order", "", "PaymentOrder UUID")
		gatewayOrderID = flag.String("gateway-order", "", "Sui Gateway Order object ID")
		destination    = flag.String("destination", "", "final refund destination Sui address")
		execute        = flag.Bool("execute", false, "submit on-chain transactions and update DB")
	)
	flag.Parse()

	if !*execute {
		panic("refusing to run without --execute")
	}
	if *paymentOrderID == "" || *gatewayOrderID == "" || *destination == "" {
		panic("missing required flags: --payment-order, --gateway-order, --destination")
	}

	ctx := context.Background()
	config.ServerConfig()

	if err := storage.DBConnection(config.DBConfig()); err != nil {
		panic(fmt.Errorf("connect db: %w", err))
	}
	defer storage.GetClient().Close()

	orderUUID, err := uuid.Parse(*paymentOrderID)
	if err != nil {
		panic(fmt.Errorf("parse payment order: %w", err))
	}

	po, err := storage.GetClient().PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(orderUUID)).
		WithSuiReceiveAddress().
		WithRouteAOrder().
		WithToken().
		Only(ctx)
	if err != nil {
		panic(fmt.Errorf("load payment order: %w", err))
	}
	if po.Edges.SuiReceiveAddress == nil {
		panic("payment order has no sui receive address")
	}
	if po.Edges.RouteAOrder == nil {
		panic("payment order has no route-a order")
	}
	if po.Edges.Token == nil {
		panic("payment order has no token")
	}

	receive := po.Edges.SuiReceiveAddress
	routeA := po.Edges.RouteAOrder
	coinType := receive.CoinType
	if coinType == "" {
		coinType = po.Edges.Token.ContractAddress
	}

	fmt.Printf("Refunding payment_order=%s route_a_order=%d amount=%s coin=%s\n", po.ID, routeA.ID, po.Amount.String(), coinType)
	fmt.Printf("Gateway order: %s\n", *gatewayOrderID)
	fmt.Printf("Receive wallet: %s\n", receive.Address)
	fmt.Printf("Destination: %s\n", *destination)

	svc, ok := orderpkg.NewOrderSui().(*orderpkg.OrderSui)
	if !ok {
		panic("failed to initialize OrderSui")
	}

	refundDigest, err := svc.RefundGatewayOrder(ctx, *gatewayOrderID, coinType)
	if err != nil {
		panic(fmt.Errorf("refund gateway order: %w", err))
	}
	fmt.Printf("Gateway refund tx: %s\n", refundDigest)

	seed, err := cryptoUtils.DecryptPlain(receive.EncryptedSeed)
	if err != nil {
		panic(fmt.Errorf("decrypt receive seed: %w", err))
	}

	var sweepDigest string
	for attempt := 1; attempt <= 12; attempt++ {
		sweepDigest, err = svc.SweepReceiveAddress(ctx, seed, coinType, *destination)
		if err == nil {
			break
		}
		fmt.Printf("Sweep attempt %d waiting for refunded coin: %v\n", attempt, err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		panic(fmt.Errorf("sweep receive wallet: %w", err))
	}
	fmt.Printf("Sweep tx: %s\n", sweepDigest)

	tx, err := storage.GetClient().Tx(ctx)
	if err != nil {
		panic(fmt.Errorf("begin db tx: %w", err))
	}
	defer tx.Rollback()

	if _, err := tx.PaymentOrder.
		UpdateOneID(po.ID).
		SetStatus(paymentorder.StatusRefunded).
		SetAmountReturned(po.Amount).
		SetTxHash(refundDigest).
		Save(ctx); err != nil {
		panic(fmt.Errorf("update payment order: %w", err))
	}

	reason := fmt.Sprintf("manual Route-A refund: gateway_refund=%s sweep=%s destination=%s", refundDigest, sweepDigest, *destination)
	if _, err := tx.RouteAOrder.
		UpdateOneID(routeA.ID).
		SetBridgeStatus(routeaorder.BridgeStatusRefunded).
		SetFailureReason(reason).
		Save(ctx); err != nil {
		panic(fmt.Errorf("update route-a order: %w", err))
	}

	if _, err := tx.SuiReceiveAddress.
		UpdateOneID(receive.ID).
		SetStatus(suireceiveaddress.StatusSettled).
		Save(ctx); err != nil {
		panic(fmt.Errorf("update receive address: %w", err))
	}

	if err := tx.Commit(); err != nil {
		panic(fmt.Errorf("commit db tx: %w", err))
	}

	fmt.Println("DB marked refunded.")
}
