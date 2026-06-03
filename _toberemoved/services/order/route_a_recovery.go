package order

import (
	"context"
	"errors"
	"fmt"

	"github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	"github.com/block-vision/sui-go-sdk/transaction"
)

// RefundGatewayOrder refunds a Route-A Gateway order by object ID.
//
// Route-A orders do not create LockPaymentOrder rows, so RefundOrder cannot be
// used for this path. This method performs the same on-chain Move call but
// takes the coin type explicitly.
func (s *OrderSui) RefundGatewayOrder(ctx context.Context, gatewayOrderID, coinType string) (string, error) {
	if s.signer == nil {
		return "", ErrAggregatorNotConfigured
	}
	if gatewayOrderID == "" {
		return "", errors.New("route_a_refund: gatewayOrderID is empty")
	}

	coinTypeTag, err := parseCoinTypeTag(coinType)
	if err != nil {
		return "", fmt.Errorf("route_a_refund: parse coin type: %w", err)
	}

	tx, err := s.newAggregatorTx(ctx)
	if err != nil {
		return "", fmt.Errorf("route_a_refund: prepare tx: %w", err)
	}

	aggCapArg, err := objectArg(ctx, s.client, tx, s.aggregatorCapID, false)
	if err != nil {
		return "", fmt.Errorf("route_a_refund: resolve aggregator cap: %w", err)
	}
	gatewayArg, err := objectArg(ctx, s.client, tx, s.gatewayObjectID, true)
	if err != nil {
		return "", fmt.Errorf("route_a_refund: resolve gateway: %w", err)
	}
	orderArg, err := objectArg(ctx, s.client, tx, gatewayOrderID, true)
	if err != nil {
		return "", fmt.Errorf("route_a_refund: resolve order %s: %w", gatewayOrderID, err)
	}

	tx.MoveCall(
		models.SuiAddress(s.packageID),
		"order",
		"refund_order",
		[]transaction.TypeTag{coinTypeTag},
		[]transaction.Argument{
			aggCapArg,
			gatewayArg,
			orderArg,
			tx.Pure(uint64(0)),
		},
	)

	resp, err := submitSponsoredViaShinami(ctx, s.client, s.shinami, s.signer, s.signer.Address, tx)
	if err != nil {
		return "", fmt.Errorf("route_a_refund: submit: %w", err)
	}
	if !isTxSuccess(resp) {
		return "", fmt.Errorf("route_a_refund: on-chain failure: digest=%s", resp.Digest)
	}
	return resp.Digest, nil
}

// SweepReceiveAddress transfers all coin objects of coinType from a generated
// receive wallet to destination. The receive wallet signs; Shinami sponsors gas.
func (s *OrderSui) SweepReceiveAddress(ctx context.Context, receiveSeed []byte, coinType, destination string) (string, error) {
	if len(receiveSeed) != 32 {
		return "", fmt.Errorf("route_a_sweep: receive seed length %d != 32", len(receiveSeed))
	}
	recvSigner := suisigner.NewSigner(receiveSeed)

	coinsResp, err := s.client.SuiXGetCoins(ctx, models.SuiXGetCoinsRequest{
		Owner:    recvSigner.Address,
		CoinType: coinType,
		Limit:    50,
	})
	if err != nil {
		return "", fmt.Errorf("route_a_sweep: query receive wallet coins: %w", err)
	}
	if len(coinsResp.Data) == 0 {
		return "", fmt.Errorf("route_a_sweep: receive wallet %s has no %s coins", recvSigner.Address, coinType)
	}

	tx := transaction.NewTransaction()
	tx.SetSuiClient(s.client).
		SetSigner(recvSigner).
		SetSender(models.SuiAddress(recvSigner.Address))

	coinArgs := make([]transaction.Argument, 0, len(coinsResp.Data))
	for _, coin := range coinsResp.Data {
		ref, err := transaction.NewSuiObjectRef(
			models.SuiAddress(coin.CoinObjectId),
			coin.Version,
			models.ObjectDigest(coin.Digest),
		)
		if err != nil {
			return "", fmt.Errorf("route_a_sweep: build coin ref %s: %w", coin.CoinObjectId, err)
		}
		coinArgs = append(coinArgs, tx.Object(transaction.CallArg{
			Object: &transaction.ObjectArg{ImmOrOwnedObject: ref},
		}))
	}

	tx.TransferObjects(coinArgs, tx.Pure(destination))

	resp, err := submitSponsoredViaShinami(ctx, s.client, s.shinami, recvSigner, recvSigner.Address, tx)
	if err != nil {
		return "", fmt.Errorf("route_a_sweep: submit: %w", err)
	}
	if !isTxSuccess(resp) {
		return "", fmt.Errorf("route_a_sweep: on-chain failure: digest=%s", resp.Digest)
	}
	return resp.Digest, nil
}
