package order

import (
	"encoding/hex"
	"testing"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/mystenbcs"
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

	// Canonical bytes should be exactly 1 byte shorter due to the omission of the 0x20 prefix
	require.Equal(t, len(sdkBytes)-1, len(canonicalBytes))

	// We can locate the digest in both and check.
	// Since ObjectID is 32 bytes, Version is 8 bytes, the ObjectRef starts after some input envelope.
	// In the inputs list:
	// Inputs len is ULEB128 (1 byte: 0x01)
	// Input variant is CallArg::Object (1 byte: 0x01)
	// ObjectArg variant is ImmOrOwnedObject (1 byte: 0x00)
	// Then ObjectRef:
	//   ObjectID: 32 bytes (indices 3..35)
	//   Version: 8 bytes (indices 35..43)
	//   Digest:
	//     For SDK: 1 byte length prefix (0x20) followed by 32 bytes (indices 43..76)
	//     For Canonical: 32 raw bytes (indices 43..75)

	require.Equal(t, byte(0x20), sdkBytes[44])
	require.Equal(t, digestBytes, sdkBytes[45:77])
	require.Equal(t, digestBytes, canonicalBytes[44:76])

	var unmarshaledKind transaction.TransactionKind
	_, unmarshalErr := mystenbcs.Unmarshal(canonicalBytes, &unmarshaledKind)
	t.Logf("Unmarshal of canonical bytes err: %v", unmarshalErr)
	if unmarshalErr == nil {
		inputs := unmarshaledKind.ProgrammableTransaction.Inputs
		if len(inputs) > 0 && inputs[0].Object != nil && inputs[0].Object.ImmOrOwnedObject != nil {
			t.Logf("Unmarshaled digest hex: %s", hex.EncodeToString(unmarshaledKind.ProgrammableTransaction.Inputs[0].Object.ImmOrOwnedObject.Digest))
			t.Logf("Original digest hex:    %s", hex.EncodeToString(digestBytes))
		}
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

	// Length difference: SDK has 0x20 in the gasCoin digest (1 byte) + 0x20 in any inputs if present.
	// Since there are no input SuiObjectRefs, the difference is exactly 1 byte (from the single GasPayment).
	require.Equal(t, len(sdkBytes)-1, len(canonicalBytes))
	_ = senderBytesPtr
}
