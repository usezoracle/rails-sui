//go:build ignore

package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/storage"
)

func main() {
	if err := storage.DBConnection(config.DBConfig()); err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer storage.GetClient().Close()
	ctx := context.Background()

	idStr := "a893c983-58a1-47a7-810c-48925db7e05d"
	if len(os.Args) > 1 {
		idStr = os.Args[1]
	}
	id := uuid.MustParse(idStr)
	o, err := storage.GetClient().PaymentOrder.Query().
		Where(paymentorder.IDEQ(id)).
		WithToken().
		WithRecipient().
		WithTransactions().
		WithRouteAOrder().
		Only(ctx)
	if err != nil {
		log.Fatalf("query order: %v", err)
	}

	fmt.Printf("Order %s\n", o.ID)
	fmt.Printf("  status=%s\n", o.Status)
	fmt.Printf("  amount=%s amount_paid=%s amount_returned=%s percent_settled=%s\n",
		o.Amount, o.AmountPaid, o.AmountReturned, o.PercentSettled)
	fmt.Printf("  rate=%s tx_hash=%s gateway_id=%s\n", o.Rate, o.TxHash, o.GatewayID)
	fmt.Printf("  reference=%s\n", o.Reference)
	fmt.Printf("  receive_address=%s from=%s return=%s\n", o.ReceiveAddressText, o.FromAddress, o.ReturnAddress)
	fmt.Printf("  created=%s updated=%s\n", o.CreatedAt, o.UpdatedAt)
	if o.Edges.Token != nil {
		fmt.Printf("  token=%s contract=%s\n", o.Edges.Token.Symbol, o.Edges.Token.ContractAddress)
	}
	if o.Edges.Recipient != nil {
		r := o.Edges.Recipient
		fmt.Printf("  recipient: inst=%s acct=%s name=%s memo=%s\n",
			r.Institution, r.AccountIdentifier, r.AccountName, r.Memo)
	}
	if o.Edges.RouteAOrder != nil {
		fmt.Printf("  RouteAOrder: %+v\n", o.Edges.RouteAOrder)
	} else {
		fmt.Printf("  RouteAOrder: <none>\n")
	}
	fmt.Printf("  Transactions (%d):\n", len(o.Edges.Transactions))
	for _, t := range o.Edges.Transactions {
		fmt.Printf("    - status=%s network=%s created=%s\n      meta=%v\n",
			t.Status, t.Network, t.CreatedAt, t.Metadata)
	}
}
