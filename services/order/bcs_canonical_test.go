package order

import (
	"encoding/hex"
	"testing"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/mystenbcs"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	"github.com/block-vision/sui-go-sdk/transaction"
	"github.com/stretchr/testify/require"
)

func TestCanonicalSerializer_MatchesSDKForSimplePTB(t *testing.T) {
	// Construct a PTB: split gas coin and transfer to an address.
	// Only pure and GasCoin inputs (no SuiObjectRef).
	tx := transaction.NewTransaction()

	// Split gas coin by 1000 MIST
	arg1 := tx.Pure(uint64(1000))

	coinSplitCmd := transaction.Command{
		SplitCoins: &transaction.SplitCoins{
			Coin: &transaction.Argument{
				GasCoin: struct{}{},
			},
			Amount: []*transaction.Argument{&arg1},
		},
	}
	tx.Data.V1.Kind.ProgrammableTransaction.Commands = append(tx.Data.V1.Kind.ProgrammableTransaction.Commands, &coinSplitCmd)

	// Transfer the split coin to a recipient address
	recipientStr := "0x1234567890123456789012345678901234567890123456789012345678901234"
	addrArg := tx.Pure(recipientStr)

	// The split result is Argument::NestedResult(0, 0)
	resIndex := uint16(0)
	resIdx := uint16(0)
	splitResult := &transaction.Argument{
		NestedResult: &transaction.NestedResult{
			Index:       resIndex,
			ResultIndex: resIdx,
		},
	}

	transferCmd := transaction.Command{
		TransferObjects: &transaction.TransferObjects{
			Objects: []*transaction.Argument{splitResult},
			Address: &addrArg,
		},
	}
	tx.Data.V1.Kind.ProgrammableTransaction.Commands = append(tx.Data.V1.Kind.ProgrammableTransaction.Commands, &transferCmd)

	// Serialize using SDK
	sdkBytes, err := mystenbcs.Marshal(tx.Data.V1.Kind)
	require.NoError(t, err)

	// Serialize using canonical
	canonicalBytes, err := marshalTransactionKindCanonical(tx.Data.V1.Kind)
	require.NoError(t, err)

	// They must match exactly!
	require.Equal(t, sdkBytes, canonicalBytes)
}

func TestCanonicalSerializer_FixesSuiObjectRef(t *testing.T) {
	// Construct a PTB that passes an ImmOrOwnedObject as input.
	// Verify that our canonical serializer produces byte-identical
	// output to the SDK. Both should include the ULEB128 length prefix
	// on the digest (Sui's Digest type is Vec<u8> in BCS, not [u8; 32]).
	tx := transaction.NewTransaction()

	objIdStr := "0xe523f8c245622bc93a670a7fb97c4793e18097f64a4127367f783b727a696657"
	objIdBytesPtr, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(objIdStr))
	require.NoError(t, err)

	// Mock a 32-byte digest
	digestHex := "9840d4334c375fc6ccd91f49c37b232e5fabaec1ed2ba7e9b90b39c46a1a9ccd"
	digestBytes, err := hex.DecodeString(digestHex)
	require.NoError(t, err)

	ref := transaction.SuiObjectRef{
		ObjectId: *objIdBytesPtr,
		Version:  893364839,
		Digest:   models.ObjectDigestBytes(digestBytes),
	}

	arg := tx.Object(transaction.CallArg{
		Object: &transaction.ObjectArg{
			ImmOrOwnedObject: &ref,
		},
	})

	// Add a dummy command using the input
	cmd := transaction.Command{
		TransferObjects: &transaction.TransferObjects{
			Objects: []*transaction.Argument{&arg},
			Address: &arg, // dummy address
		},
	}
	tx.Data.V1.Kind.ProgrammableTransaction.Commands = append(tx.Data.V1.Kind.ProgrammableTransaction.Commands, &cmd)

	// Serialize using SDK
	sdkBytes, err := mystenbcs.Marshal(tx.Data.V1.Kind)
	require.NoError(t, err)

	// Serialize using canonical
	canonicalBytes, err := marshalTransactionKindCanonical(tx.Data.V1.Kind)
	require.NoError(t, err)

	// Both should now be byte-identical since both include the length prefix.
	require.Equal(t, len(sdkBytes), len(canonicalBytes),
		"canonical and SDK TransactionKind bytes should have the same length")
	require.Equal(t, sdkBytes, canonicalBytes,
		"canonical and SDK TransactionKind bytes should be identical")

	// Verify the SDK can unmarshal our canonical bytes back.
	var unmarshaledKind transaction.TransactionKind
	_, unmarshalErr := mystenbcs.Unmarshal(canonicalBytes, &unmarshaledKind)
	require.NoError(t, unmarshalErr, "SDK should be able to unmarshal canonical bytes")
	inputs := unmarshaledKind.ProgrammableTransaction.Inputs
	if len(inputs) > 0 && inputs[0].Object != nil && inputs[0].Object.ImmOrOwnedObject != nil {
		require.Equal(t, digestBytes, []byte(inputs[0].Object.ImmOrOwnedObject.Digest))
	}
}

func TestCanonicalSerializer_TransactionDataMatchesSDK(t *testing.T) {
	// Construct TransactionData with no SuiObjectRef and verify it matches the SDK's encoding.
	tx := transaction.NewTransaction()

	senderStr := "0x1111111111111111111111111111111111111111111111111111111111111111"
	senderBytesPtr, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(senderStr))
	require.NoError(t, err)
	tx.SetSender(models.SuiAddress(senderStr))

	tx.SetGasBudget(100000)
	tx.SetGasPrice(1000)

	gasOwnerStr := "0x2222222222222222222222222222222222222222222222222222222222222222"
	tx.SetGasOwner(models.SuiAddress(gasOwnerStr))

	// Split gas coin by 1000 MIST
	arg1 := tx.Pure(uint64(1000))

	coinSplitCmd := transaction.Command{
		SplitCoins: &transaction.SplitCoins{
			Coin: &transaction.Argument{
				GasCoin: struct{}{},
			},
			Amount: []*transaction.Argument{&arg1},
		},
	}
	tx.Data.V1.Kind.ProgrammableTransaction.Commands = append(tx.Data.V1.Kind.ProgrammableTransaction.Commands, &coinSplitCmd)

	coinIdStr := "0x3333333333333333333333333333333333333333333333333333333333333333"
	coinIdBytesPtr, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(coinIdStr))
	require.NoError(t, err)

	digestBytes := make([]byte, 32)
	copy(digestBytes, []byte("digestdigestdigestdigestdigestdi"))

	gasCoin := transaction.SuiObjectRef{
		ObjectId: *coinIdBytesPtr,
		Version:  1,
		Digest:   models.ObjectDigestBytes(digestBytes),
	}
	tx.SetGasPayment([]transaction.SuiObjectRef{gasCoin})

	sdkBytes, err := tx.Data.Marshal()
	require.NoError(t, err)

	canonicalBytes, err := marshalTransactionDataCanonical(&tx.Data)
	require.NoError(t, err)

	// Both should now be byte-identical since we include the ULEB128 length prefix on digests.
	require.Equal(t, len(sdkBytes), len(canonicalBytes),
		"canonical and SDK TransactionData bytes should have the same length")
	require.Equal(t, sdkBytes, canonicalBytes,
		"canonical and SDK TransactionData bytes should be identical")
	_ = senderBytesPtr
}

func TestCanonicalSerializer_marshalTransactionDataWithSerializedKind(t *testing.T) {
	// Construct a simple PTB
	tx := transaction.NewTransaction()

	senderStr := "0x1111111111111111111111111111111111111111111111111111111111111111"
	tx.SetSender(models.SuiAddress(senderStr))

	gasOwnerStr := "0x2222222222222222222222222222222222222222222222222222222222222222"
	tx.SetGasOwner(models.SuiAddress(gasOwnerStr))

	tx.SetGasPrice(1000)
	tx.SetGasBudget(100000)

	coinIdStr := "0x3333333333333333333333333333333333333333333333333333333333333333"
	coinIdBytesPtr, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(coinIdStr))
	require.NoError(t, err)

	digestBytes := make([]byte, 32)
	copy(digestBytes, []byte("digestdigestdigestdigestdigestdi"))

	gasCoin := transaction.SuiObjectRef{
		ObjectId: *coinIdBytesPtr,
		Version:  1,
		Digest:   models.ObjectDigestBytes(digestBytes),
	}
	tx.SetGasPayment([]transaction.SuiObjectRef{gasCoin})

	// Split gas coin
	arg := tx.Pure(uint64(500))
	cmd := transaction.Command{
		SplitCoins: &transaction.SplitCoins{
			Coin: &transaction.Argument{
				GasCoin: struct{}{},
			},
			Amount: []*transaction.Argument{&arg},
		},
	}
	tx.Data.V1.Kind.ProgrammableTransaction.Commands = append(tx.Data.V1.Kind.ProgrammableTransaction.Commands, &cmd)

	// Get serialized kind bytes canonically
	kindBytes, err := marshalTransactionKindCanonical(tx.Data.V1.Kind)
	require.NoError(t, err)

	// Serialize full data using the helper
	splicedBytes, err := marshalTransactionDataWithSerializedKind(
		kindBytes,
		senderStr,
		gasOwnerStr,
		1000,
		100000,
		gasCoin,
	)
	require.NoError(t, err)

	// Serialize full data canonically using the existing method
	canonicalBytes, err := marshalTransactionDataCanonical(&tx.Data)
	require.NoError(t, err)

	// They must match exactly!
	require.Equal(t, canonicalBytes, splicedBytes)

	// Print aggregator address
	privKeyHex := "6dd76dad8f9c91cc613a244a6c47c6c093bdfe0af3f3bf2209e0fb9976644223"
	seedBytes, err := hex.DecodeString(privKeyHex)
	require.NoError(t, err)
	s := suisigner.NewSigner(seedBytes)
	t.Logf("AGGREGATOR ADDRESS: %s", s.Address)
}
