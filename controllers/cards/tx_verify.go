// Sui transaction-block verification for /v1/cards/link/complete.
//
// When the cardholder PWA tells us "I published create_cap, here's the
// digest and the object ID it created" — we shouldn't blindly trust
// either. Hit the Sui RPC for the digest, walk its object changes,
// and confirm:
//
//   1. The tx succeeded.
//   2. It created an object whose ID matches `cap_object_id`.
//   3. The created object's type starts with
//      `<package_id>::tapp_card::CardSpendingCap<…>`.
//
// We DON'T verify the embedded `card_uid_hash` against the claim
// here — that lives inside the BCS-encoded object contents and
// requires a `sui_getObject` round-trip + struct decoding. v1 stance:
// the hash is committed at create-time via Move, so the user can't
// later swap a different card to point at the same cap without
// breaking on-chain invariants. Adding `sui_getObject` decoding is a
// reasonable v1.x hardening; left as a TODO inside `verifyCreateCap`.

package cards

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/sui"

	"github.com/usezoracle/rails-sui/config"
)

// VerifyCreateCapResult is what callers care about: did the tx land,
// and what coin-type Twas the CardSpendingCap parameterized with.
type VerifyCreateCapResult struct {
	OK       bool
	CoinType string
	Reason   string
}

// verifyCreateCap fetches the tx block, scans object changes, and
// returns the verification result. Pure function over the chain
// state — no DB writes.
func verifyCreateCap(
	ctx context.Context,
	txDigest, capObjectID string,
) (*VerifyCreateCapResult, error) {
	conf := config.OrderConfig()
	packageID := conf.SuiGatewayPackageID
	if packageID == "" {
		return nil, errors.New("SUI_GATEWAY_PACKAGE_ID not configured")
	}
	if txDigest == "" || capObjectID == "" {
		return nil, errors.New("digest and cap_object_id required")
	}

	client := sui.NewSuiClient(conf.SuiRpcURL)
	resp, err := client.SuiGetTransactionBlock(ctx, models.SuiGetTransactionBlockRequest{
		Digest: txDigest,
		Options: models.SuiTransactionBlockOptions{
			ShowEffects:       true,
			ShowObjectChanges: true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get transaction block: %w", err)
	}

	if !txSuccess(resp) {
		return &VerifyCreateCapResult{OK: false, Reason: "tx did not succeed"}, nil
	}

	// Expected type prefix: "<package_id>::tapp_card::CardSpendingCap<"
	wantPrefix := packageID + "::tapp_card::CardSpendingCap<"

	for _, change := range resp.ObjectChanges {
		// `Type` is the change kind ("created", "mutated", …);
		// `ObjectType` is the on-chain Move type tag.
		if change.Type != "created" {
			continue
		}
		if !sameSuiAddr(change.ObjectId, capObjectID) {
			continue
		}
		moveType := change.ObjectType
		if !strings.HasPrefix(moveType, wantPrefix) {
			return &VerifyCreateCapResult{
				OK:     false,
				Reason: fmt.Sprintf("object %s is not a CardSpendingCap (got type %q)", change.ObjectId, moveType),
			}, nil
		}
		// Extract the coin type arg from "<package>::tapp_card::CardSpendingCap<T>"
		// — between the first '<' and the last '>'.
		coinType := strings.TrimSuffix(strings.TrimPrefix(moveType, wantPrefix), ">")
		return &VerifyCreateCapResult{
			OK:       true,
			CoinType: coinType,
		}, nil
	}

	return &VerifyCreateCapResult{
		OK:     false,
		Reason: fmt.Sprintf("no created CardSpendingCap matching %s found in tx %s", capObjectID, txDigest),
	}, nil
}

// txSuccess mirrors order/sui.go's isTxSuccess for the block-vision
// JSON-RPC response shape.
func txSuccess(resp models.SuiTransactionBlockResponse) bool {
	if resp.Effects.Status.Status == "success" {
		return true
	}
	return false
}

// sameSuiAddr compares two Sui addresses ignoring 0x prefix + casing.
func sameSuiAddr(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(strings.ToLower(a), "0x"),
		strings.TrimPrefix(strings.ToLower(b), "0x"))
}
