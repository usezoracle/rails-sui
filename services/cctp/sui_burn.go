package cctp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	suiconst "github.com/block-vision/sui-go-sdk/constant"
	suimodels "github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	"github.com/block-vision/sui-go-sdk/transaction"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/block-vision/sui-go-sdk/mystenbcs"
)

// burnGasBudgetMist is the gas budget for the deposit_for_burn PTB.
// A burn costs ~0.01 SUI; 0.05 leaves generous headroom and the excess
// is refunded. The aggregator wallet always carries SUI for gas (it
// already pays for LiFi bridge txs).
const burnGasBudgetMist = 50_000_000

// maxBurnInputCoins caps how many USDC coin objects we merge in one
// PTB. The aggregator wallet consolidates continuously, so >20
// fragments covering one order means something else is wrong.
const maxBurnInputCoins = 20

// BurnRequest is everything SubmitBurn needs. Amount is USDC subunits
// (6 dp) — the exact value minted on Base; CCTP v1 is 1:1 with no fee.
type BurnRequest struct {
	Net            Network
	AmountSubunits uint64
	// MintRecipient is the Base wallet the USDC mints to (the same
	// aggregator hot wallet LiFi delivers to).
	MintRecipient ethcommon.Address
}

// SubmitBurn builds, signs, and submits the Sui-side deposit_for_burn
// PTB. Returns the Sui tx digest — the one durable key the rest of the
// flow (Iris lookup, Base mint) is derived from.
//
// PTB shape:
//
//	[merge USDC fragments] → split(exact amount) →
//	token_messenger_minter::deposit_for_burn::deposit_for_burn<USDC>(
//	    coin, baseDomain, mintRecipient,
//	    State, &mut MessageTransmitterState, DenyList, &mut Treasury<USDC>)
func SubmitBurn(ctx context.Context, client *suisdk.Client, signer *suisigner.Signer, req BurnRequest) (string, error) {
	if req.AmountSubunits == 0 {
		return "", fmt.Errorf("cctp: burn amount is zero")
	}

	// Source coins: the aggregator's USDC fragments covering the amount.
	coinResp, err := client.SuiXGetCoins(ctx, suimodels.SuiXGetCoinsRequest{
		Owner:    signer.Address,
		CoinType: req.Net.SuiUSDCCoinType,
	})
	if err != nil {
		return "", fmt.Errorf("cctp: list USDC coins: %w", err)
	}
	selected, err := selectCoins(coinResp.Data, new(big.Int).SetUint64(req.AmountSubunits))
	if err != nil {
		return "", err
	}

	// Gas coin: largest SUI coin (distinct coin type, so it can never
	// collide with the USDC inputs).
	gasRef, err := largestGasCoin(ctx, client, signer.Address)
	if err != nil {
		return "", err
	}

	usdcTypeTag, err := structTypeTag(req.Net.SuiUSDCCoinType)
	if err != nil {
		return "", err
	}

	tx := transaction.NewTransaction()
	tx.SetSuiClient(client).
		SetSigner(signer).
		SetSender(suimodels.SuiAddress(signer.Address)).
		SetGasPayment([]transaction.SuiObjectRef{*gasRef}).
		SetGasBudget(burnGasBudgetMist)

	// Owned-coin inputs.
	coinArgs := make([]transaction.Argument, 0, len(selected))
	for _, c := range selected {
		ref, err := transaction.NewSuiObjectRef(
			suimodels.SuiAddress(c.CoinObjectId), c.Version, suimodels.ObjectDigest(c.Digest))
		if err != nil {
			return "", fmt.Errorf("cctp: coin ref %s: %w", c.CoinObjectId, err)
		}
		coinArgs = append(coinArgs, tx.Object(transaction.CallArg{
			Object: &transaction.ObjectArg{ImmOrOwnedObject: ref},
		}))
	}
	primary := coinArgs[0]
	if len(coinArgs) > 1 {
		tx.MergeCoins(primary, coinArgs[1:])
	}
	exact := tx.SplitCoins(primary, []transaction.Argument{tx.Pure(req.AmountSubunits)})

	// Shared-object inputs, with the mutability deposit_for_burn declares.
	stateArg, err := sharedObjectArg(ctx, client, tx, req.Net.SuiTokenMessengerState, false)
	if err != nil {
		return "", err
	}
	mtStateArg, err := sharedObjectArg(ctx, client, tx, req.Net.SuiMessageTransmitterState, true)
	if err != nil {
		return "", err
	}
	denyListArg, err := sharedObjectArg(ctx, client, tx, req.Net.SuiDenyList, false)
	if err != nil {
		return "", err
	}
	treasuryArg, err := sharedObjectArg(ctx, client, tx, req.Net.SuiUSDCTreasury, true)
	if err != nil {
		return "", err
	}

	tx.MoveCall(
		suimodels.SuiAddress(req.Net.SuiTokenMessengerPackage),
		"deposit_for_burn",
		"deposit_for_burn",
		[]transaction.TypeTag{*usdcTypeTag},
		[]transaction.Argument{
			exact,
			tx.Pure(req.Net.BaseDomain),
			tx.Pure(evmAddressToSuiAddress(req.MintRecipient)),
			stateArg,
			mtStateArg,
			denyListArg,
			treasuryArg,
		},
	)

	bcsBytes, err := tx.BuildBCSBytes(ctx)
	if err != nil {
		return "", fmt.Errorf("cctp: build burn tx: %w", err)
	}
	txB64 := mystenbcs.ToBase64(bcsBytes)

	// Same signing path the LiFi flow uses: SignMessage with the
	// transaction-data intent scope (the SDK's SignTransaction helper
	// uses the wrong scope — see startBridge in route_a_dispatcher.go).
	signed, err := signer.SignMessage(txB64, suiconst.TransactionDataIntentScope)
	if err != nil {
		return "", fmt.Errorf("cctp: sign burn tx: %w", err)
	}
	resp, err := client.SuiExecuteTransactionBlock(ctx, suimodels.SuiExecuteTransactionBlockRequest{
		TxBytes:     txB64,
		Signature:   []string{signed.Signature},
		Options:     suimodels.SuiTransactionBlockOptions{ShowEffects: true},
		RequestType: "WaitForLocalExecution",
	})
	if err != nil {
		return "", fmt.Errorf("cctp: submit burn tx: %w", err)
	}
	if resp.Digest == "" {
		return "", fmt.Errorf("cctp: burn tx returned empty digest")
	}
	if s := resp.Effects.Status.Status; s != "" && s != "success" {
		return resp.Digest, fmt.Errorf("cctp: burn tx %s failed on-chain: %s", resp.Digest, resp.Effects.Status.Error)
	}
	return resp.Digest, nil
}

// selectCoins picks USDC coin objects (largest first would minimize
// inputs, but the RPC already returns a stable order; we take in order
// and stop once covered) totalling at least `amount`.
func selectCoins(coins []suimodels.CoinData, amount *big.Int) ([]suimodels.CoinData, error) {
	var (
		selected []suimodels.CoinData
		total    = new(big.Int)
	)
	for _, c := range coins {
		bal, ok := new(big.Int).SetString(c.Balance, 10)
		if !ok {
			return nil, fmt.Errorf("cctp: parse coin balance %q (%s)", c.Balance, c.CoinObjectId)
		}
		if bal.Sign() <= 0 {
			continue
		}
		selected = append(selected, c)
		total.Add(total, bal)
		if total.Cmp(amount) >= 0 {
			break
		}
		if len(selected) >= maxBurnInputCoins {
			return nil, fmt.Errorf("cctp: need >%d coin objects to cover %s — wallet too fragmented", maxBurnInputCoins, amount)
		}
	}
	if total.Cmp(amount) < 0 {
		return nil, fmt.Errorf("cctp: insufficient USDC: have %s, need %s", total, amount)
	}
	return selected, nil
}

// largestGasCoin returns an object ref for the wallet's biggest SUI
// coin, used as gas payment.
func largestGasCoin(ctx context.Context, client *suisdk.Client, owner string) (*transaction.SuiObjectRef, error) {
	resp, err := client.SuiXGetCoins(ctx, suimodels.SuiXGetCoinsRequest{
		Owner:    owner,
		CoinType: "0x2::sui::SUI",
	})
	if err != nil {
		return nil, fmt.Errorf("cctp: list gas coins: %w", err)
	}
	var (
		best    *suimodels.CoinData
		bestBal = new(big.Int)
	)
	for i := range resp.Data {
		bal, ok := new(big.Int).SetString(resp.Data[i].Balance, 10)
		if !ok {
			continue
		}
		if bal.Cmp(bestBal) > 0 {
			best, bestBal = &resp.Data[i], bal
		}
	}
	if best == nil {
		return nil, fmt.Errorf("cctp: no SUI gas coin in aggregator wallet")
	}
	if bestBal.Cmp(big.NewInt(burnGasBudgetMist)) < 0 {
		return nil, fmt.Errorf("cctp: largest SUI coin (%s MIST) below gas budget %d", bestBal, burnGasBudgetMist)
	}
	return transaction.NewSuiObjectRef(
		suimodels.SuiAddress(best.CoinObjectId), best.Version, suimodels.ObjectDigest(best.Digest))
}

// sharedObjectArg fetches a shared object's initial_shared_version and
// registers it as a PTB input with the given mutability. The version is
// immutable chain data; one RPC round-trip per object per burn keeps
// this stateless rather than caching deployment constants we'd have to
// maintain per network.
func sharedObjectArg(
	ctx context.Context, client *suisdk.Client, tx *transaction.Transaction,
	objectID string, mutable bool,
) (transaction.Argument, error) {
	resp, err := client.SuiGetObject(ctx, suimodels.SuiGetObjectRequest{
		ObjectId: objectID,
		Options:  suimodels.SuiObjectDataOptions{ShowOwner: true},
	})
	if err != nil {
		return transaction.Argument{}, fmt.Errorf("cctp: fetch shared object %s: %w", objectID, err)
	}
	version, err := initialSharedVersion(resp.Data.Owner)
	if err != nil {
		return transaction.Argument{}, fmt.Errorf("cctp: object %s: %w", objectID, err)
	}
	idBytes, err := transaction.ConvertSuiAddressStringToBytes(suimodels.SuiAddress(objectID))
	if err != nil {
		return transaction.Argument{}, fmt.Errorf("cctp: object id %s: %w", objectID, err)
	}
	return tx.Object(transaction.CallArg{
		Object: &transaction.ObjectArg{
			SharedObject: &transaction.SharedObjectRef{
				ObjectId:             *idBytes,
				InitialSharedVersion: version,
				Mutable:              mutable,
			},
		},
	}), nil
}

// initialSharedVersion digs initial_shared_version out of the untyped
// owner field ({"Shared": {"initial_shared_version": N}}).
func initialSharedVersion(owner any) (uint64, error) {
	raw, err := json.Marshal(owner)
	if err != nil {
		return 0, fmt.Errorf("owner field not JSON-encodable: %w", err)
	}
	var parsed struct {
		Shared struct {
			InitialSharedVersion *uint64 `json:"initial_shared_version"`
		} `json:"Shared"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Shared.InitialSharedVersion == nil {
		return 0, fmt.Errorf("not a shared object (owner=%s)", strings.TrimSpace(string(raw)))
	}
	return *parsed.Shared.InitialSharedVersion, nil
}

// evmAddressToSuiAddress left-pads a 20-byte EVM address to the 32-byte
// hex string Move's `address` type (and CCTP's mint_recipient) expects.
func evmAddressToSuiAddress(a ethcommon.Address) string {
	return "0x000000000000000000000000" + strings.ToLower(strings.TrimPrefix(a.Hex(), "0x"))
}
