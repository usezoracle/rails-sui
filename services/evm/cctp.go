// cctp.go — additive support for redeeming Circle CCTP v1 messages on
// the active EVM chain (Route A's direct-CCTP bridge fallback; see
// services/cctp). Lives in this package so receiveMessage txs share the
// client's nonce mutex with approve/createOrder — same signer wallet,
// same serialisation discipline. No existing file in this package is
// touched by the fallback.
package evm

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	ethabi "github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// messageTransmitterABIJSON is the v1 MessageTransmitter subset we use:
// the mint entrypoint and the replay-protection mapping that makes
// re-submits after a crash a cheap read instead of a revert.
const messageTransmitterABIJSON = `[
  {"name":"receiveMessage","type":"function","stateMutability":"nonpayable",
   "inputs":[{"name":"message","type":"bytes"},{"name":"attestation","type":"bytes"}],
   "outputs":[{"name":"success","type":"bool"}]},
  {"name":"usedNonces","type":"function","stateMutability":"view",
   "inputs":[{"name":"","type":"bytes32"}],
   "outputs":[{"name":"","type":"uint256"}]}
]`

var messageTransmitterABI ethabi.ABI

func init() {
	parsed, err := ethabi.JSON(strings.NewReader(messageTransmitterABIJSON))
	if err != nil {
		panic(fmt.Sprintf("evm: parse MessageTransmitter ABI: %s", err))
	}
	messageTransmitterABI = parsed
}

// MessageTransmitter binds Circle's v1 MessageTransmitter at `addr` to
// this client's signer. Construct via Client.CCTPTransmitter.
type MessageTransmitter struct {
	c    *Client
	addr common.Address
	b    *bind.BoundContract
}

// CCTPTransmitter returns a binding for the v1 MessageTransmitter at
// the given address (per-network constant from cctp.Network).
func (c *Client) CCTPTransmitter(addr common.Address) *MessageTransmitter {
	return &MessageTransmitter{
		c:    c,
		addr: addr,
		b:    bind.NewBoundContract(addr, messageTransmitterABI, c.eth, c.eth, c.eth),
	}
}

// NonceUsed reports whether the (sourceDomain, nonce) pair has already
// been received on this chain — i.e. the mint already happened. Key is
// keccak256(abi.encodePacked(uint32 sourceDomain, uint64 nonce)), per
// v1 MessageTransmitter._hashSourceAndNonce.
func (t *MessageTransmitter) NonceUsed(ctx context.Context, sourceDomain uint32, nonce uint64) (bool, error) {
	packed := make([]byte, 12)
	binary.BigEndian.PutUint32(packed[0:4], sourceDomain)
	binary.BigEndian.PutUint64(packed[4:12], nonce)
	key := crypto.Keccak256Hash(packed)

	var out []any
	if err := t.b.Call(&bind.CallOpts{Context: ctx}, &out, "usedNonces", key); err != nil {
		return false, fmt.Errorf("evm: usedNonces: %w", err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return false, fmt.Errorf("evm: usedNonces: unexpected return type")
	}
	return v.Sign() != 0, nil
}

// ReceiveMessage submits receiveMessage(message, attestation), waits
// for it to mine, and returns the receipt. On success Circle's
// TokenMinter has minted the burn amount of native USDC to the
// message's mint recipient.
//
// Holds the client's tx mutex for the duration so nonces don't race
// with approve/createOrder from the same wallet.
func (t *MessageTransmitter) ReceiveMessage(ctx context.Context, message, attestation []byte) (*types.Receipt, error) {
	t.c.txMu.Lock()
	defer t.c.txMu.Unlock()

	opts, err := t.c.newTransactor(ctx)
	if err != nil {
		return nil, err
	}
	// receiveMessage + USDC mint runs ~170k gas; fixed generous limit
	// (same rationale as createOrder: skip EstimateGas, which dry-runs
	// against state that may shift between estimate and submit).
	opts.GasLimit = 400_000

	tx, err := t.b.Transact(opts, "receiveMessage", message, attestation)
	if err != nil {
		return nil, fmt.Errorf("evm: receiveMessage: %w", err)
	}
	return t.c.waitMined(ctx, tx)
}
