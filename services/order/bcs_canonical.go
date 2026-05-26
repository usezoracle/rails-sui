// bcs_canonical.go — spec-correct BCS serializer for Sui TransactionKind.
//
// Why this exists: the block-vision SDK's generic mystenbcs encoder
// declares `SuiObjectRef.Digest` as `models.ObjectDigestBytes` which is
// a `[]byte` slice, so the encoder length-prefixes it (ULEB128 32 byte
// before the 32 raw digest bytes). Sui's wire spec is
// `Digest([u8; 32])` — a fixed array, no length prefix. The SDK has
// special-case handling for `models.SuiAddressBytes` (a `[32]byte`
// fixed array — encoded as 32 raw bytes), but no equivalent for the
// digest type. Result: every aggregator PTB that pins an
// ImmOrOwnedObject (i.e., every Move call that consumes an owned coin)
// emits bytes that Shinami's strict parser rejects with -32602.
//
// This file owns the marshalling for the small set of TransactionKind
// variants we actually use (ProgrammableTransaction with MoveCall /
// SplitCoins / MergeCoins / TransferObjects / MakeMoveVec). For
// commands we don't use (Publish, Upgrade) we return an error rather
// than half-implementing them. If we ever need them, fill them in
// then — not now.
//
// The serializer reads the block-vision struct tree on `tx.Data.V1.Kind`
// but emits Sui-spec-correct bytes, then hands them to Shinami.

package order

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/mystenbcs"
	"github.com/block-vision/sui-go-sdk/transaction"
)

// marshalTransactionKindCanonical writes the Sui-spec BCS bytes for a
// TransactionKind. Only the ProgrammableTransaction variant is
// supported; calling with any other variant returns an error.
func marshalTransactionKindCanonical(kind *transaction.TransactionKind) ([]byte, error) {
	if kind == nil {
		return nil, fmt.Errorf("nil TransactionKind")
	}
	if kind.ProgrammableTransaction == nil {
		return nil, fmt.Errorf("only ProgrammableTransaction is supported (got a non-PT variant)")
	}
	var buf bytes.Buffer
	// TransactionKind variant 0 == ProgrammableTransaction.
	buf.WriteByte(0)
	if err := writeProgrammableTransaction(&buf, kind.ProgrammableTransaction); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// marshalTransactionDataCanonical writes the Sui-spec BCS bytes for a
// TransactionData. Only the V1 variant is supported.
func marshalTransactionDataCanonical(td *transaction.TransactionData) ([]byte, error) {
	if td == nil {
		return nil, fmt.Errorf("nil TransactionData")
	}
	if td.V1 == nil {
		return nil, fmt.Errorf("only V1 TransactionData is supported")
	}
	var buf bytes.Buffer
	// TransactionData variant 0 == V1.
	buf.WriteByte(0)
	if err := writeTransactionDataV1(&buf, td.V1); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeTransactionDataV1(buf *bytes.Buffer, v1 *transaction.TransactionDataV1) error {
	if v1 == nil {
		return fmt.Errorf("nil TransactionDataV1")
	}
	// 1. Kind (TransactionKind)
	if err := writeTransactionKindCanonical(buf, v1.Kind); err != nil {
		return fmt.Errorf("kind: %w", err)
	}
	// 2. Sender (SuiAddressBytes)
	if v1.Sender == nil {
		return fmt.Errorf("nil Sender")
	}
	buf.Write((*v1.Sender)[:])
	// 3. GasData (GasData)
	if err := writeGasDataCanonical(buf, v1.GasData); err != nil {
		return fmt.Errorf("gas_data: %w", err)
	}
	// 4. Expiration (optional TransactionExpiration)
	if v1.Expiration == nil {
		buf.WriteByte(0) // Option::None
	} else {
		buf.WriteByte(1) // Option::Some
		if err := writeTransactionExpiration(buf, v1.Expiration); err != nil {
			return fmt.Errorf("expiration: %w", err)
		}
	}
	return nil
}

func writeTransactionKindCanonical(buf *bytes.Buffer, kind *transaction.TransactionKind) error {
	if kind == nil {
		return fmt.Errorf("nil TransactionKind")
	}
	if kind.ProgrammableTransaction == nil {
		return fmt.Errorf("only ProgrammableTransaction is supported")
	}
	buf.WriteByte(0) // TransactionKind::ProgrammableTransaction
	return writeProgrammableTransaction(buf, kind.ProgrammableTransaction)
}

func writeGasDataCanonical(buf *bytes.Buffer, gd *transaction.GasData) error {
	if gd == nil {
		return fmt.Errorf("nil GasData")
	}
	if gd.Payment == nil {
		return fmt.Errorf("nil GasData.Payment")
	}
	payment := *gd.Payment
	writeULEB128(buf, len(payment))
	for i, ref := range payment {
		if err := writeSuiObjectRef(buf, &ref); err != nil {
			return fmt.Errorf("payment %d: %w", i, err)
		}
	}
	if gd.Owner == nil {
		return fmt.Errorf("nil GasData.Owner")
	}
	buf.Write((*gd.Owner)[:])
	if gd.Price == nil {
		return fmt.Errorf("nil GasData.Price")
	}
	writeU64(buf, *gd.Price)
	if gd.Budget == nil {
		return fmt.Errorf("nil GasData.Budget")
	}
	writeU64(buf, *gd.Budget)
	return nil
}

func writeTransactionExpiration(buf *bytes.Buffer, exp *transaction.TransactionExpiration) error {
	if exp == nil {
		return fmt.Errorf("nil TransactionExpiration")
	}
	if exp.Epoch != nil {
		buf.WriteByte(1) // TransactionExpiration::Epoch
		writeU64(buf, *exp.Epoch)
		return nil
	}
	// default to None
	buf.WriteByte(0) // TransactionExpiration::None
	return nil
}

func writeProgrammableTransaction(buf *bytes.Buffer, pt *transaction.ProgrammableTransaction) error {
	writeULEB128(buf, len(pt.Inputs))
	for i, in := range pt.Inputs {
		if err := writeCallArg(buf, in); err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
	}
	writeULEB128(buf, len(pt.Commands))
	for i, cmd := range pt.Commands {
		if err := writeCommand(buf, cmd); err != nil {
			return fmt.Errorf("command %d: %w", i, err)
		}
	}
	return nil
}

func writeCallArg(buf *bytes.Buffer, ca *transaction.CallArg) error {
	switch {
	case ca == nil:
		return fmt.Errorf("nil CallArg")
	case ca.Pure != nil:
		buf.WriteByte(0) // CallArg::Pure
		// Pure(Vec<u8>) — length-prefix the already-BCS-encoded user bytes.
		writeBytes(buf, ca.Pure.Bytes)
		return nil
	case ca.Object != nil:
		buf.WriteByte(1) // CallArg::Object
		return writeObjectArg(buf, ca.Object)
	case ca.UnresolvedPure != nil:
		// Sui RPC rejects this variant. objectArg() + pureAddress() are
		// supposed to keep us out of these branches. If we see one, it's
		// a code bug, not data — fail loud.
		return fmt.Errorf("UnresolvedPure CallArg cannot be serialized — resolve before calling")
	case ca.UnresolvedObject != nil:
		return fmt.Errorf("UnresolvedObject CallArg cannot be serialized — use objectArg() helper")
	}
	return fmt.Errorf("CallArg has no variant set")
}

func writeObjectArg(buf *bytes.Buffer, oa *transaction.ObjectArg) error {
	switch {
	case oa.ImmOrOwnedObject != nil:
		buf.WriteByte(0)
		return writeSuiObjectRef(buf, oa.ImmOrOwnedObject)
	case oa.SharedObject != nil:
		buf.WriteByte(1)
		ref := oa.SharedObject
		buf.Write(ref.ObjectId[:])
		writeU64(buf, ref.InitialSharedVersion)
		if ref.Mutable {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		return nil
	case oa.Receiving != nil:
		buf.WriteByte(2)
		return writeSuiObjectRef(buf, oa.Receiving)
	}
	return fmt.Errorf("ObjectArg has no variant set")
}

// writeSuiObjectRef writes a SuiObjectRef in BCS:
//
//	32 raw bytes  ObjectId   ([u8; 32], no prefix)
//	 8 raw bytes  Version    (u64 little-endian)
//	ULEB128 + N   Digest     (Vec<u8> — length-prefixed byte sequence)
//
// Sui's Rust type `Digest([u8; 32])` uses `serde_as(as = "Readable<Base58,
// Bytes>")`. The `serde_with::Bytes` adapter treats `[u8; 32]` as a byte
// sequence (Vec<u8>) in binary formats, so BCS writes a ULEB128 length
// prefix (0x20 = 32) before the 32 raw digest bytes. Total: 73 bytes.
//
// The block-vision Go SDK also length-prefixes the digest (because its
// Go type is `[]byte`), which turns out to be correct for the same reason.
func writeSuiObjectRef(buf *bytes.Buffer, ref *transaction.SuiObjectRef) error {
	buf.Write(ref.ObjectId[:])
	writeU64(buf, ref.Version)
	if len(ref.Digest) != 32 {
		return fmt.Errorf("SuiObjectRef.Digest must be 32 bytes, got %d", len(ref.Digest))
	}
	writeBytes(buf, ref.Digest) // Vec<u8>: ULEB128 length prefix + raw bytes
	return nil
}

func writeCommand(buf *bytes.Buffer, c *transaction.Command) error {
	switch {
	case c.MoveCall != nil:
		buf.WriteByte(0)
		mc := c.MoveCall
		buf.Write(mc.Package[:])
		writeString(buf, mc.Module)
		writeString(buf, mc.Function)
		writeULEB128(buf, len(mc.TypeArguments))
		for i, t := range mc.TypeArguments {
			if err := writeTypeTag(buf, t); err != nil {
				return fmt.Errorf("type arg %d: %w", i, err)
			}
		}
		writeULEB128(buf, len(mc.Arguments))
		for i, a := range mc.Arguments {
			if err := writeArgument(buf, a); err != nil {
				return fmt.Errorf("arg %d: %w", i, err)
			}
		}
		return nil
	case c.TransferObjects != nil:
		buf.WriteByte(1)
		to := c.TransferObjects
		writeULEB128(buf, len(to.Objects))
		for _, o := range to.Objects {
			if err := writeArgument(buf, o); err != nil {
				return err
			}
		}
		return writeArgument(buf, to.Address)
	case c.SplitCoins != nil:
		buf.WriteByte(2)
		sc := c.SplitCoins
		if err := writeArgument(buf, sc.Coin); err != nil {
			return err
		}
		writeULEB128(buf, len(sc.Amount))
		for _, a := range sc.Amount {
			if err := writeArgument(buf, a); err != nil {
				return err
			}
		}
		return nil
	case c.MergeCoins != nil:
		buf.WriteByte(3)
		mc := c.MergeCoins
		if err := writeArgument(buf, mc.Destination); err != nil {
			return err
		}
		writeULEB128(buf, len(mc.Sources))
		for _, s := range mc.Sources {
			if err := writeArgument(buf, s); err != nil {
				return err
			}
		}
		return nil
	case c.MakeMoveVec != nil:
		buf.WriteByte(5)
		mv := c.MakeMoveVec
		// Option<TypeTag>: 0 == None, 1 == Some
		if mv.Type == nil {
			buf.WriteByte(0)
		} else {
			// MakeMoveVec.Type is a *string on this SDK. We don't use
			// MakeMoveVec anywhere; if we ever do, the type tag would
			// have to be a parsed TypeTag value, not a string. Fail
			// loud rather than write a body whose interpretation is
			// guessed.
			return fmt.Errorf("MakeMoveVec with non-nil Type is not supported (SDK exposes Type as *string; parse to TypeTag before calling)")
		}
		writeULEB128(buf, len(mv.Elements))
		for _, el := range mv.Elements {
			if err := writeArgument(buf, el); err != nil {
				return err
			}
		}
		return nil
	case c.Publish != nil:
		return fmt.Errorf("Publish command not supported by canonical serializer")
	case c.Upgrade != nil:
		return fmt.Errorf("Upgrade command not supported by canonical serializer")
	}
	return fmt.Errorf("Command has no variant set")
}

func writeArgument(buf *bytes.Buffer, a *transaction.Argument) error {
	switch {
	case a == nil:
		return fmt.Errorf("nil Argument")
	case a.GasCoin != nil:
		buf.WriteByte(0)
		return nil
	case a.Input != nil:
		buf.WriteByte(1)
		writeU16(buf, *a.Input)
		return nil
	case a.Result != nil:
		buf.WriteByte(2)
		writeU16(buf, *a.Result)
		return nil
	case a.NestedResult != nil:
		buf.WriteByte(3)
		writeU16(buf, a.NestedResult.Index)
		writeU16(buf, a.NestedResult.ResultIndex)
		return nil
	}
	return fmt.Errorf("Argument has no variant set")
}

// writeTypeTag writes a TypeTag per Sui spec variant ordering. The
// block-vision SDK's TypeTag struct field order does NOT match Sui's
// variant indices (U64 is at struct-index 10 but is variant 2 in Sui),
// which is fine for us because we map explicitly here. The variant
// numbers below come from move-core-types/src/language_storage.rs.
func writeTypeTag(buf *bytes.Buffer, t *transaction.TypeTag) error {
	if t == nil {
		return fmt.Errorf("nil TypeTag")
	}
	switch {
	case t.Bool != nil:
		buf.WriteByte(0)
	case t.U8 != nil:
		buf.WriteByte(1)
	case t.U64 != nil:
		buf.WriteByte(2)
	case t.U128 != nil:
		buf.WriteByte(3)
	case t.Address != nil:
		buf.WriteByte(4)
	case t.Signer != nil:
		buf.WriteByte(5)
	case t.Vector != nil:
		buf.WriteByte(6)
		return writeTypeTag(buf, t.Vector)
	case t.Struct != nil:
		buf.WriteByte(7)
		return writeStructTag(buf, t.Struct)
	case t.U16 != nil:
		buf.WriteByte(8)
	case t.U32 != nil:
		buf.WriteByte(9)
	case t.U256 != nil:
		buf.WriteByte(10)
	default:
		return fmt.Errorf("TypeTag has no variant set")
	}
	return nil
}

func writeStructTag(buf *bytes.Buffer, s *transaction.StructTag) error {
	buf.Write(s.Address[:])
	writeString(buf, s.Module)
	writeString(buf, s.Name)
	writeULEB128(buf, len(s.TypeParams))
	for _, tp := range s.TypeParams {
		if err := writeTypeTag(buf, tp); err != nil {
			return err
		}
	}
	return nil
}

// writeULEB128 writes a length / index as ULEB128. Reuses the SDK's
// helper so we don't fork the variable-length encoding logic.
func writeULEB128(buf *bytes.Buffer, n int) {
	buf.Write(mystenbcs.ULEB128Encode(n))
}

func writeBytes(buf *bytes.Buffer, b []byte) {
	writeULEB128(buf, len(b))
	buf.Write(b)
}

func writeString(buf *bytes.Buffer, s string) {
	writeBytes(buf, []byte(s))
}

func writeU16(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeU64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

// marshalTransactionDataWithSerializedKind constructs the TransactionData BCS bytes
// by directly splicing the pre-serialized TransactionKind bytes.
func marshalTransactionDataWithSerializedKind(
	kindBytes []byte,
	sender string,
	gasOwner string,
	gasPrice uint64,
	gasBudget uint64,
	gasPayment transaction.SuiObjectRef,
) ([]byte, error) {
	var buf bytes.Buffer

	// TransactionData variant 0 == V1.
	buf.WriteByte(0)

	// 1. Kind (pre-serialized TransactionKind bytes)
	buf.Write(kindBytes)

	// 2. Sender (SuiAddressBytes)
	senderBytes, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(sender))
	if err != nil {
		return nil, fmt.Errorf("invalid sender address: %w", err)
	}
	buf.Write((*senderBytes)[:])

	// 3. GasData
	gasOwnerBytes, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(gasOwner))
	if err != nil {
		return nil, fmt.Errorf("invalid gas owner address: %w", err)
	}

	// GasData:
	//   payment: Vec<SuiObjectRef>
	//   owner: SuiAddress
	//   price: u64
	//   budget: u64

	// payment is Vec<SuiObjectRef>. ULEB128 length prefix followed by SuiObjectRefs.
	// Since we always have exactly 1 gas coin:
	writeULEB128(&buf, 1)
	if err := writeSuiObjectRef(&buf, &gasPayment); err != nil {
		return nil, fmt.Errorf("gas payment object ref: %w", err)
	}

	// owner
	buf.Write((*gasOwnerBytes)[:])

	// price
	writeU64(&buf, gasPrice)

	// budget
	writeU64(&buf, gasBudget)

	// 4. Expiration (optional TransactionExpiration)
	// We default to None (0)
	buf.WriteByte(0)

	return buf.Bytes(), nil
}
