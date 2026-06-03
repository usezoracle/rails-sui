package order

import (
	"context"
	"fmt"
	"testing"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/signer"
	"github.com/block-vision/sui-go-sdk/transaction"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	"github.com/usezoracle/rails-sui/storage"
	cryptoUtils "github.com/usezoracle/rails-sui/utils/crypto"
)

func TestSweepIncident(t *testing.T) {
	ctx := context.Background()

	// Initialize DB
	dsn := config.DBConfig()
	if err := storage.DBConnection(dsn); err != nil {
		t.Fatalf("Init DB: %v", err)
	}

	sInterface := NewOrderSui()
	s, ok := sInterface.(*OrderSui)
	if !ok {
		t.Fatalf("Failed to cast NewOrderSui to *OrderSui")
	}

	receiveAddr := "0x2eeba17319e66bbec5e81248439e255fddea51e398159cfee41bce34e68135c2"
	destAddr := "0x0cf5f5be83fa3ea7666e532c0af0097244eacabc95906e35092957cbfcc9c0ea"

	// Fetch receive address row
	row, err := storage.GetClient().SuiReceiveAddress.
		Query().
		Where(suireceiveaddress.AddressEQ(receiveAddr)).
		Only(ctx)
	if err != nil {
		t.Fatalf("Fetch receive address row: %v", err)
	}

	// Decrypt seed
	seed, err := cryptoUtils.DecryptPlain(row.EncryptedSeed)
	if err != nil {
		t.Fatalf("Decrypt seed: %v", err)
	}

	recvSigner := signer.NewSigner(seed)
	fmt.Printf("Signer address: %s\n", recvSigner.Address)

	// Get USDC coins
	coinsResp, err := s.client.SuiXGetCoins(ctx, models.SuiXGetCoinsRequest{
		Owner:    recvSigner.Address,
		CoinType: row.CoinType,
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("Query coins: %v", err)
	}

	if len(coinsResp.Data) == 0 {
		t.Fatalf("No coins found to sweep")
	}

	tx := transaction.NewTransaction()
	tx.SetSuiClient(s.client).
		SetSigner(recvSigner).
		SetSender(models.SuiAddress(recvSigner.Address))

	var coinArgs []transaction.Argument
	for _, coin := range coinsResp.Data {
		ref, err := transaction.NewSuiObjectRef(
			models.SuiAddress(coin.CoinObjectId),
			coin.Version,
			models.ObjectDigest(coin.Digest),
		)
		if err != nil {
			t.Fatalf("Build coin ref: %v", err)
		}
		arg := tx.Object(transaction.CallArg{
			Object: &transaction.ObjectArg{
				ImmOrOwnedObject: ref,
			},
		})
		coinArgs = append(coinArgs, arg)
	}

	tx.TransferObjects(coinArgs, tx.Pure(destAddr))

	resp, err := submitSponsoredViaShinami(ctx, s.client, s.shinami, recvSigner, recvSigner.Address, tx)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !isTxSuccess(resp) {
		t.Fatalf("Transaction failed: %+v", resp.Effects.Status)
	}

	fmt.Printf("\n>>> SWEEP SUCCESS: %s <<<\n\n", resp.Digest)
}
