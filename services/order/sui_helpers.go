// sui_helpers.go — call-arg constructors that produce fully-resolved
// `CallArg::Object` values for PTB inputs.
//
// Why this exists: the block-vision/sui-go-sdk v1.2.1 helper
// `tx.Object(stringID)` creates a `CallArg::UnresolvedObject` (variant
// index 3 of the on-chain `CallArg` BCS enum). On-chain validation
// only accepts variants 0–2 (`Pure`, `Object`, plus the SDK's
// `UnresolvedPure` which Sui doesn't know about either), and rejects
// the whole tx with
//
//	Deserialization error: invalid value: integer `3`, expected variant index 0 <= i < 3
//
// before any of our Move calls run. Every aggregator-side MoveCall in
// services/order/sui.go was hitting this silently (zero
// `sui_receive_addresses` rows have ever reached `forwarded`). See
// docs/incidents/2026-05-25-route-a-stuck-deposit.md and
// 2026-05-26 follow-up.
//
// Use `objectArg(ctx, tx, id, mutable)` everywhere a string ID was
// previously passed to `tx.Object`. The helper queries `sui_getObject`
// once, inspects the owner type:
//
//   - `Shared`           → CallArg::Object::SharedObject with the
//                          initial_shared_version. Cached forever since
//                          a shared object's initial_shared_version is
//                          set at sharing time and never changes.
//   - everything else    → CallArg::Object::ImmOrOwnedObject with the
//                          current version+digest. Queried fresh every
//                          call because owned objects' versions change
//                          on every mutation.
//
// The `mutable` flag only applies to shared objects — it tells Sui's
// scheduler whether this tx writes the object (parallel scheduling
// optimization). Owned objects ignore it.

package order

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"

	"github.com/block-vision/sui-go-sdk/constant"
	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/mystenbcs"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	"github.com/block-vision/sui-go-sdk/sui"
	"github.com/block-vision/sui-go-sdk/transaction"

	shinamiGas "github.com/usezoracle/rails-sui/services/shinami_gas"
)

// sharedRef is the cached resolution for a shared object.
type sharedRef struct {
	initialSharedVersion uint64
}

// sharedObjectCache memoizes the initial_shared_version per object ID.
// Process-lifetime cache because `initial_shared_version` is immutable.
// Concurrent-safe — the SuiDepositWatcher cron and the dispatcher cron
// can both call into helpers that hit this map.
var (
	sharedObjectCache   = map[string]sharedRef{}
	sharedObjectCacheMu sync.RWMutex
)

// objectArg resolves an object ID into the right CallArg variant
// (Shared or ImmOrOwnedObject), adds it as a PTB input, and returns
// the resulting Argument.
//
// `mutable` is only consulted for shared objects — it tells Sui's
// scheduler whether this tx writes the object. Always pass the right
// value (Move function signature determines it): e.g., create_order
// mutates the Gateway and the Clock-derived Order, so pass true for
// the Gateway. Reads-only of the global Clock (0x6) → pass false.
//
// Returns the Argument ready to use as a Move call input.
func objectArg(
	ctx context.Context,
	client *sui.Client,
	tx *transaction.Transaction,
	objectID string,
	mutable bool,
) (transaction.Argument, error) {
	// Try the shared-object cache first — covers the hot path
	// (Gateway, AggregatorCap-as-owned, Clock).
	sharedObjectCacheMu.RLock()
	if cached, ok := sharedObjectCache[objectID]; ok {
		sharedObjectCacheMu.RUnlock()
		return sharedArg(tx, objectID, cached.initialSharedVersion, mutable), nil
	}
	sharedObjectCacheMu.RUnlock()

	// Cache miss — fetch the object and decide.
	resp, err := client.SuiGetObject(ctx, models.SuiGetObjectRequest{
		ObjectId: objectID,
		Options: models.SuiObjectDataOptions{
			ShowOwner:   true,
			ShowContent: false,
		},
	})
	if err != nil {
		return transaction.Argument{}, fmt.Errorf("get object %s: %w", objectID, err)
	}
	if resp.Data == nil {
		return transaction.Argument{}, fmt.Errorf("object %s not found on-chain", objectID)
	}

	owner := ownerKind(resp.Data.Owner)
	switch owner {
	case ownerShared:
		isv, perr := parseUint64(extractInitialSharedVersion(resp.Data.Owner))
		if perr != nil {
			return transaction.Argument{}, fmt.Errorf("object %s: parse initial_shared_version: %w", objectID, perr)
		}
		// Cache forever — initial_shared_version is fixed at sharing time.
		sharedObjectCacheMu.Lock()
		sharedObjectCache[objectID] = sharedRef{initialSharedVersion: isv}
		sharedObjectCacheMu.Unlock()
		return sharedArg(tx, objectID, isv, mutable), nil

	case ownerImmutable, ownerAddressOwned, ownerObjectOwned:
		// Owned/immutable — use ImmOrOwnedObject with the current
		// version+digest. Never cache (version changes on every mutation).
		ref, rerr := transaction.NewSuiObjectRef(
			models.SuiAddress(resp.Data.ObjectId),
			resp.Data.Version,
			models.ObjectDigest(resp.Data.Digest),
		)
		if rerr != nil {
			return transaction.Argument{}, fmt.Errorf("build owned ref for %s: %w", objectID, rerr)
		}
		return tx.Object(transaction.CallArg{
			Object: &transaction.ObjectArg{ImmOrOwnedObject: ref},
		}), nil

	default:
		return transaction.Argument{}, fmt.Errorf("object %s has unknown owner kind %q", objectID, owner)
	}
}

// sharedArg builds a SharedObject CallArg argument once we have its
// initial_shared_version. Pulled out so cached + uncached paths share
// the construction.
func sharedArg(
	tx *transaction.Transaction,
	objectID string,
	initialSharedVersion uint64,
	mutable bool,
) transaction.Argument {
	addr, _ := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(objectID))
	return tx.Object(transaction.CallArg{
		Object: &transaction.ObjectArg{
			SharedObject: &transaction.SharedObjectRef{
				ObjectId:             *addr,
				InitialSharedVersion: initialSharedVersion,
				Mutable:              mutable,
			},
		},
	})
}

// Sui's RPC returns object owners as one of several shapes:
//
//	"Immutable"
//	{ "AddressOwner": "0x..." }
//	{ "ObjectOwner":  "0x..." }
//	{ "Shared":       { "initial_shared_version": N } }
//
// The block-vision SDK exposes Owner as `any`; we sniff the shape and
// classify.
type ownerKindKind string

const (
	ownerShared       ownerKindKind = "shared"
	ownerImmutable    ownerKindKind = "immutable"
	ownerAddressOwned ownerKindKind = "address"
	ownerObjectOwned  ownerKindKind = "object"
	ownerUnknown      ownerKindKind = "unknown"
)

func ownerKind(o any) ownerKindKind {
	if s, ok := o.(string); ok && s == "Immutable" {
		return ownerImmutable
	}
	if m, ok := o.(map[string]any); ok {
		if _, ok := m["Shared"]; ok {
			return ownerShared
		}
		if _, ok := m["AddressOwner"]; ok {
			return ownerAddressOwned
		}
		if _, ok := m["ObjectOwner"]; ok {
			return ownerObjectOwned
		}
	}
	return ownerUnknown
}

// extractInitialSharedVersion pulls the version field out of the
// shared-owner shape, accepting either int or string (JSON numbers
// land as float64 / json.Number / string depending on decoder).
func extractInitialSharedVersion(o any) string {
	m, ok := o.(map[string]any)
	if !ok {
		return ""
	}
	shared, ok := m["Shared"].(map[string]any)
	if !ok {
		return ""
	}
	v, ok := shared["initial_shared_version"]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatUint(uint64(t), 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func parseUint64(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return strconv.ParseUint(s, 10, 64)
}

// submitSponsoredViaShinami signs `tx` as `senderSigner`, sends the
// TransactionKind to Shinami for gas sponsorship, then submits both
// signatures to Sui. Returns the SuiExecuteTransactionBlockResponse.
//
// Replaces the SDK's broken SetSponsoredSigner / SetGasOwner path.
// Caller must have populated tx.Data (sender, package, MoveCall args,
// gas price) but NOT gas owner/payment — Shinami attaches its fund's
// gas coin.
//
// Gas price + budget left to Shinami's auto-budgeting (recommended).
// Returns ErrShinamiGasNotConfigured if no Shinami client was wired
// at OrderSui construction.
func submitSponsoredViaShinami(
	ctx context.Context,
	client *sui.Client,
	sg *shinamiGas.Client,
	senderSigner *suisigner.Signer,
	senderAddress string,
	tx *transaction.Transaction,
) (*models.SuiTransactionBlockResponse, error) {
	if sg == nil {
		return nil, ErrShinamiGasNotConfigured
	}

	// Step 1: serialize the TransactionKind (no gas data).
	//
	// The block-vision SDK's `BuildBCSBytes` resolves sender + gas
	// fields and emits TransactionData (with gas), not TransactionKind.
	// We want TransactionKind-only so Shinami can attach gas. There's
	// no `onlyTransactionKind: true` toggle on this SDK, so we build
	// the kind manually from the ProgrammableTransaction we already
	// staged on `tx.Data.V1.Kind`.
	kindBytes, err := mystenbcs.Marshal(tx.Data.V1.Kind)
	if err != nil {
		return nil, fmt.Errorf("marshal tx kind: %w", err)
	}
	kindB64 := base64.StdEncoding.EncodeToString(kindBytes)

	// Step 2: ask Shinami to sponsor — they attach gas + sign as sponsor.
	sponsored, err := sg.SponsorTransactionBlock(ctx, kindB64, senderAddress, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("shinami sponsor: %w", err)
	}

	// Step 3: sign the sponsored TransactionData with the sender key.
	senderMsg, err := senderSigner.SignMessage(sponsored.TxBytes, constant.TransactionDataIntentScope)
	if err != nil {
		return nil, fmt.Errorf("sender sign: %w", err)
	}

	// Step 4: submit to Sui with both signatures (sponsor + sender).
	resp, err := client.SuiExecuteTransactionBlock(ctx, models.SuiExecuteTransactionBlockRequest{
		TxBytes:     sponsored.TxBytes,
		Signature:   []string{sponsored.Signature, senderMsg.Signature},
		Options:     models.SuiTransactionBlockOptions{ShowEffects: true, ShowEvents: true},
		RequestType: "WaitForLocalExecution",
	})
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	return &resp, nil
}

// pureAddress is the address-Pure constructor that works around the
// SDK's Pure() bug: `tx.Pure(models.SuiAddress(x))` falls into the
// BCS-encoder branch because `input.(string)` won't match a
// `SuiAddress` (named type), and emits a length-prefixed string blob
// instead of 32 raw address bytes. Use this anywhere a Move argument
// wants an `address` Pure value.
//
// Accepts any string-ish address — `models.SuiAddress` and the
// plain-`string` `signer.Signer.Address` both convert cleanly.
func pureAddress[T ~string](tx *transaction.Transaction, addr T) transaction.Argument {
	return tx.Pure(string(addr))
}
