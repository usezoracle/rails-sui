package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// SuiReceiveAddress holds per-order Sui addresses used in the Path-2
// (exchange / external wallet) deposit flow. Generated fresh by
// ReceiveAddressService.CreateSuiReceiveAddress, persisted with the
// Ed25519 seed encrypted via the protocol's AES master key. When a deposit
// is detected at the address, OrderSui.CreateOrder loads + decrypts the
// seed, builds a PTB calling rails::order::create_order from that wallet,
// and forwards the coin into the on-chain Gateway escrow.
//
// See rails-architecture.md "Path 2 — Receive address" for the full flow.
type SuiReceiveAddress struct {
	ent.Schema
}

func (SuiReceiveAddress) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (SuiReceiveAddress) Fields() []ent.Field {
	return []ent.Field{
		// 0x-prefixed Sui address derived from blake2b256(0x00 || pubKey)[:32].
		field.String("address").Unique(),

		// AES-encrypted 32-byte Ed25519 seed. Decrypted at forwarding time
		// to sign the create_order PTB. Encryption uses cryptoUtils.EncryptPlain
		// against the protocol AES key from CryptoConfig.
		field.Bytes("encrypted_seed").Immutable(),

		// Move type string of the coin we expect the user to send,
		// e.g. "0x...::usdc::USDC". Used by the indexer to filter
		// SuiXGetCoins / event subscriptions per address.
		field.String("coin_type"),

		// Expected amount in the coin's smallest unit (USDC: 6 decimals).
		// Indexer compares incoming deposits against this for validation.
		field.Uint64("expected_amount"),

		// Lifecycle: unused → deposited → forwarded → settled, or expired.
		field.Enum("status").
			Values("unused", "deposited", "forwarded", "settled", "expired").
			Default("unused"),

		// Sui transaction digest of the user's deposit (set once detected).
		field.String("deposit_tx_digest").Optional(),

		// Sui transaction digest of our forwarding create_order PTB
		// (set once OrderSui.CreateOrder submits successfully).
		field.String("forward_tx_digest").Optional(),

		// Hard expiry. After this, the address is marked expired and the
		// reconciliation job sweeps any unexpected late deposits.
		field.Time("valid_until"),
	}
}

func (SuiReceiveAddress) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("payment_order", PaymentOrder.Type).
			Ref("sui_receive_address").
			Unique(),
	}
}
