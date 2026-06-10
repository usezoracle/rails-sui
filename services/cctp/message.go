package cctp

import (
	"encoding/binary"
	"fmt"
	"math/big"
)

// CCTP v1 wire format (big-endian throughout, per Circle's TypedMemView
// encoding — https://developers.circle.com/cctp/technical-guide):
//
//	Message header (116 bytes)
//	  [0:4)    version            u32
//	  [4:8)    sourceDomain       u32
//	  [8:12)   destinationDomain  u32
//	  [12:20)  nonce              u64
//	  [20:52)  sender             bytes32
//	  [52:84)  recipient          bytes32
//	  [84:116) destinationCaller  bytes32
//
//	BurnMessage body (132 bytes, follows the header)
//	  [0:4)    bodyVersion        u32
//	  [4:36)   burnToken          bytes32
//	  [36:68)  mintRecipient      bytes32
//	  [68:100) amount             uint256
//	  [100:132) messageSender     bytes32
const (
	headerLen     = 116
	burnBodyLen   = 132
	burnMessageLen = headerLen + burnBodyLen
)

// Message is the decoded subset of a CCTP v1 burn message we act on.
// Decoded straight from the bytes Circle attests — the message itself
// is the single source of truth for what will be minted where, so the
// dispatcher never has to trust locally cached amounts.
type Message struct {
	SourceDomain      uint32
	DestinationDomain uint32
	Nonce             uint64
	// MintRecipient is the 32-byte recipient from the burn body; for an
	// EVM destination the address is the last 20 bytes.
	MintRecipient [32]byte
	// Amount is the burned (and to-be-minted) USDC in subunits (6 dp).
	Amount *big.Int
}

// ParseBurnMessage decodes a CCTP v1 message carrying a BurnMessage
// body. Errors on anything that isn't exactly a v1 burn message —
// better to stall an order loudly than act on a misparsed amount.
func ParseBurnMessage(raw []byte) (*Message, error) {
	if len(raw) != burnMessageLen {
		return nil, fmt.Errorf("cctp: message length %d, want %d (v1 burn message)", len(raw), burnMessageLen)
	}
	if v := binary.BigEndian.Uint32(raw[0:4]); v != 0 {
		return nil, fmt.Errorf("cctp: message version %d, want 0 (v1)", v)
	}
	m := &Message{
		SourceDomain:      binary.BigEndian.Uint32(raw[4:8]),
		DestinationDomain: binary.BigEndian.Uint32(raw[8:12]),
		Nonce:             binary.BigEndian.Uint64(raw[12:20]),
		Amount:            new(big.Int).SetBytes(raw[headerLen+68 : headerLen+100]),
	}
	copy(m.MintRecipient[:], raw[headerLen+36:headerLen+68])
	return m, nil
}

// MintRecipientEVM returns the EVM address encoded in MintRecipient
// (last 20 of the 32 bytes).
func (m *Message) MintRecipientEVM() [20]byte {
	var a [20]byte
	copy(a[:], m.MintRecipient[12:])
	return a
}
