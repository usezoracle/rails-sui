// Package order implements types.OrderService against the Sui-native Move
// Gateway package at /Users/mac/rails/contracts/gateway/.
//
// Responsibilities:
//   - CreateOrder: only for the Path-2 receive-address forwarding flow
//     (rails-architecture.md "Path 2 — Receive address"). Loads the per-order
//     Sui keypair, decrypts it, builds a PTB calling rails::order::create_order
//     from that wallet, signs, submits. The Path-1 PTB-direct deposit does NOT
//     enter this method — the user's wallet builds the PTB client-side.
//   - SettleOrder: aggregator-signed PTB calling rails::order::settle_order
//     on a pending Order shared object, releasing the LP's share of escrow.
//   - RefundOrder: aggregator-signed PTB calling rails::order::refund_order
//     to return remaining escrow to the order's refund_address.
package order

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
	"github.com/block-vision/sui-go-sdk/sui"
	"github.com/block-vision/sui-go-sdk/transaction"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	cryptoUtils "github.com/usezoracle/rails-sui/utils/crypto"
)

// rateScaleE6 mirrors the Move package's u64-scaled rate format (6 decimals).
var rateScaleE6 = decimal.NewFromInt(1_000_000)

// ErrCreateOrderPath1OnlyClientSide signals that CreateOrder was invoked for
// an order whose deposit path is PTB-direct (Path 1). For Path 1 the user's
// wallet builds and submits the create_order PTB client-side; the backend has
// no role until the indexer sees OrderCreated. The backend-side CreateOrder is
// only valid for Path 2 (receive-address forwarding).
var ErrCreateOrderPath1OnlyClientSide = errors.New("rails: CreateOrder is path-2-only; path-1 deposits are signed by the user's wallet client-side")

// ErrAggregatorNotConfigured is returned when an aggregator-gated operation
// (settle / refund) is invoked but config.SuiAggregatorPrivateKey is empty.
var ErrAggregatorNotConfigured = errors.New("rails: SUI_AGGREGATOR_PRIVATE_KEY not set; cannot sign aggregator PTBs")

// Default gas budget for aggregator PTBs (settle / refund). 50M MIST = 0.05 SUI,
// generous headroom for current Sui mainnet gas — tune down after gas profiling.
const defaultGasBudget uint64 = 50_000_000

// OrderSui implements types.OrderService against the Sui Move Gateway.
type OrderSui struct {
	client          *sui.Client
	signer          *suisigner.Signer // aggregator signer; nil if SUI_AGGREGATOR_PRIVATE_KEY unset
	packageID       string
	gatewayObjectID string
	aggregatorCapID string
}

// NewOrderSui constructs an OrderSui service from config. The returned value
// satisfies types.OrderService. The aggregator signer is built from the raw
// 32-byte Ed25519 seed in SUI_AGGREGATOR_PRIVATE_KEY; if that env var is empty
// (e.g. local dev without an aggregator key), settle/refund return
// ErrAggregatorNotConfigured at call time rather than panicking at startup.
func NewOrderSui() types.OrderService {
	conf := config.OrderConfig()

	apiClient := sui.NewSuiClient(conf.SuiRpcURL)
	client, _ := apiClient.(*sui.Client)

	var signer *suisigner.Signer
	if len(conf.SuiAggregatorPrivateKey) == 32 {
		signer = suisigner.NewSigner(conf.SuiAggregatorPrivateKey)
	}

	return &OrderSui{
		client:          client,
		signer:          signer,
		packageID:       conf.SuiGatewayPackageID,
		gatewayObjectID: conf.SuiGatewayObjectID,
		aggregatorCapID: conf.SuiAggregatorCapID,
	}
}

// CreateOrder forwards a Path-2 receive-address deposit into the Gateway
// escrow by building a sponsored PTB calling rails::order::create_order from
// the per-order receive wallet. The receive wallet signs the user-data
// portion; the aggregator signs and pays gas (sponsored-tx pattern) so the
// receive wallet never needs to hold SUI.
//
// Path-1 (PTB-direct, user signs client-side) does NOT call this method.
// Callers must check the order has an associated SuiReceiveAddress first.
func (s *OrderSui) CreateOrder(ctx context.Context, orderID uuid.UUID) error {
	if s.signer == nil {
		return ErrAggregatorNotConfigured
	}

	// Load the PaymentOrder + its receive-address + recipient + token.
	order, err := db.Client.PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(orderID)).
		WithSuiReceiveAddress().
		WithRecipient().
		WithToken().
		Only(ctx)
	if err != nil {
		return fmt.Errorf("create_order: load payment order: %w", err)
	}
	if order.Edges.SuiReceiveAddress == nil {
		return ErrCreateOrderPath1OnlyClientSide
	}
	if order.Edges.SuiReceiveAddress.Status != suireceiveaddress.StatusDeposited {
		return fmt.Errorf("create_order: receive address %s not in 'deposited' state (status=%s)",
			order.Edges.SuiReceiveAddress.Address, order.Edges.SuiReceiveAddress.Status)
	}

	// Decrypt the receive wallet's Ed25519 seed and construct its signer.
	seed, err := cryptoUtils.DecryptPlain(order.Edges.SuiReceiveAddress.EncryptedSeed)
	if err != nil {
		return fmt.Errorf("create_order: decrypt receive wallet seed: %w", err)
	}
	if len(seed) != 32 {
		return fmt.Errorf("create_order: decrypted seed length %d != 32", len(seed))
	}
	recvSigner := suisigner.NewSigner(seed)

	// Pick the receive wallet's USDC coin object — the user deposited a single
	// Coin<USDC> object via their exchange withdrawal; we use it whole as the
	// payment argument to create_order.
	coinsResp, err := s.client.SuiXGetCoins(ctx, models.SuiXGetCoinsRequest{
		Owner:    recvSigner.Address,
		CoinType: order.Edges.SuiReceiveAddress.CoinType,
		Limit:    1,
	})
	if err != nil {
		return fmt.Errorf("create_order: query receive wallet coins: %w", err)
	}
	if len(coinsResp.Data) == 0 {
		return fmt.Errorf("create_order: receive wallet %s has no %s coins yet (indexer fired too early?)",
			recvSigner.Address, order.Edges.SuiReceiveAddress.CoinType)
	}
	depositCoin := coinsResp.Data[0]
	depositCoinRef, err := transaction.NewSuiObjectRef(
		models.SuiAddress(depositCoin.CoinObjectId),
		depositCoin.Version,
		models.ObjectDigest(depositCoin.Digest),
	)
	if err != nil {
		return fmt.Errorf("create_order: build deposit coin ref: %w", err)
	}

	gasPriceResp, err := s.client.SuiXGetReferenceGasPrice(ctx)
	if err != nil {
		return fmt.Errorf("create_order: get gas price: %w", err)
	}
	gasPrice, err := strconv.ParseUint(fmt.Sprint(gasPriceResp), 10, 64)
	if err != nil {
		return fmt.Errorf("create_order: parse gas price: %w", err)
	}

	// Aggregator pays gas (sponsored-tx pattern) — receive wallet never needs SUI.
	gasCoin, err := s.selectGasCoin(ctx)
	if err != nil {
		return fmt.Errorf("create_order: select aggregator gas coin: %w", err)
	}

	coinTypeTag, err := parseCoinTypeTag(order.Edges.SuiReceiveAddress.CoinType)
	if err != nil {
		return fmt.Errorf("create_order: parse coin type: %w", err)
	}

	// Recipient bank info goes on-chain as an encrypted blob (message_hash).
	// Mirrors the EVM encryptOrderRecipient pattern: RSA-encrypt a
	// nonced JSON payload with the aggregator's public key; on the indexer
	// side, decryptMessageHash (sui_event_indexer.go) reverses this.
	messageHash, err := encryptRecipient(order.Edges.Recipient)
	if err != nil {
		return fmt.Errorf("create_order: encrypt recipient: %w", err)
	}

	// Refund address: returns to the receive wallet on refund. The downstream
	// reconciliation flow then sweeps that to the original sender.
	refundAddress := recvSigner.Address

	tx := transaction.NewTransaction()
	tx.SetSuiClient(s.client).
		SetSigner(recvSigner).
		SetSender(models.SuiAddress(recvSigner.Address)).
		SetSponsoredSigner(s.signer).
		SetGasOwner(models.SuiAddress(s.signer.Address)).
		SetGasPayment([]transaction.SuiObjectRef{*gasCoin}).
		SetGasPrice(gasPrice).
		SetGasBudget(defaultGasBudget)

	// Note: we use 0 sender_fee on Path-2 forwards — fee policy is set when the
	// order was initiated via the B2B API, captured separately. The on-chain
	// sender_fee field exists for protocols where the sender wants to tip a
	// fee recipient at deposit time, which doesn't apply here.
	senderFee := uint64(0)
	senderFeeRecipient := s.signer.Address // unused since senderFee==0; must be non-zero address

	tx.MoveCall(
		models.SuiAddress(s.packageID),
		"order",
		"create_order",
		[]transaction.TypeTag{coinTypeTag},
		[]transaction.Argument{
			tx.Object(s.gatewayObjectID),
			tx.Object(
				transaction.CallArg{
					Object: &transaction.ObjectArg{
						ImmOrOwnedObject: depositCoinRef,
					},
				},
			),
			tx.Pure(rateAsU64(order.Rate)),
			tx.Pure([]byte(order.Edges.Recipient.Institution)),
			tx.Pure(messageHash),
			tx.Pure(senderFee),
			tx.Pure(models.SuiAddress(senderFeeRecipient)),
			tx.Pure(models.SuiAddress(refundAddress)),
			tx.Object("0x6"), // Sui Clock shared object
		},
	)

	resp, err := tx.Execute(
		ctx,
		models.SuiTransactionBlockOptions{
			ShowEffects: true,
			ShowEvents:  true,
		},
		"WaitForLocalExecution",
	)
	if err != nil {
		return fmt.Errorf("create_order: submit: %w", err)
	}
	if !isTxSuccess(resp) {
		return fmt.Errorf("create_order: on-chain failure: digest=%s", resp.Digest)
	}

	// Mark the receive address as forwarded and persist the forwarding digest.
	if _, err = order.Edges.SuiReceiveAddress.Update().
		SetStatus(suireceiveaddress.StatusForwarded).
		SetForwardTxDigest(resp.Digest).
		Save(ctx); err != nil {
		return fmt.Errorf("create_order: persist receive address forward state: %w", err)
	}

	return nil
}

// rateAsU64 converts the PaymentOrder.Rate decimal into the u64 (1e6-scaled)
// the Move package's create_order expects. PaymentOrder.Rate is fiat-per-coin
// as a Decimal; the Move package stores it as a u64 scaled by 1e6.
func rateAsU64(rate decimal.Decimal) uint64 {
	scaled := rate.Mul(rateScaleE6).IntPart()
	if scaled < 0 {
		return 0
	}
	return uint64(scaled)
}

// encryptRecipient mirrors the EVM encryptOrderRecipient. It
// RSA-encrypts a nonced JSON {Nonce, AccountIdentifier, AccountName,
// Institution, ProviderID, Memo} blob with the aggregator's public key and
// returns the base64 ciphertext suitable to pass as the create_order
// message_hash argument. The indexer's decryptMessageHash reverses it.
func encryptRecipient(r *ent.PaymentOrderRecipient) (string, error) {
	if r == nil {
		return "", errors.New("recipient is nil")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	payload := struct {
		Nonce             string
		AccountIdentifier string
		AccountName       string
		Institution       string
		ProviderID        string
		Memo              string
	}{
		Nonce:             base64.StdEncoding.EncodeToString(nonce),
		AccountIdentifier: r.AccountIdentifier,
		AccountName:       r.AccountName,
		Institution:       r.Institution,
		ProviderID:        r.ProviderID,
		Memo:              r.Memo,
	}
	cipher, err := cryptoUtils.PublicKeyEncryptJSON(payload, config.CryptoConfig().AggregatorPublicKey)
	if err != nil {
		return "", fmt.Errorf("rsa encrypt: %w", err)
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}

// SettleOrder signs + submits a PTB calling rails::order::settle_order on a
// pending Order shared object.
func (s *OrderSui) SettleOrder(ctx context.Context, orderID uuid.UUID) error {
	if s.signer == nil {
		return ErrAggregatorNotConfigured
	}

	order, err := db.Client.LockPaymentOrder.
		Query().
		Where(lockpaymentorder.IDEQ(orderID)).
		WithProvider().
		WithToken().
		Only(ctx)
	if err != nil {
		return fmt.Errorf("settle_order: load lock_payment_order: %w", err)
	}
	if order.GatewayID == "" {
		return fmt.Errorf("settle_order: order %s has no on-chain GatewayID (Order object ID) recorded", orderID)
	}
	if order.Edges.Provider == nil {
		return fmt.Errorf("settle_order: order %s has no assigned provider", orderID)
	}

	// Resolve LP wallet address from the provider's order tokens. v1 takes the
	// first configured Sui address; multi-address selection is a future refinement.
	lpAddress, err := s.lpSuiAddress(ctx, order)
	if err != nil {
		return fmt.Errorf("settle_order: resolve LP address: %w", err)
	}

	// settle_percent is in basis points (max 10_000 = 100%). For v1 we settle
	// 100% in a single call; partial multi-LP settlements come once the
	// matching engine splits orders across LPs.
	settlePercent := uint64(10_000)

	// split_order_id is a unique identifier per settlement call. Using the DB
	// order ID gives traceability between the on-chain event and our records.
	splitOrderID := []byte(order.ID.String())

	coinTypeTag, err := parseCoinTypeTag(order.Edges.Token.ContractAddress)
	if err != nil {
		return fmt.Errorf("settle_order: parse coin type: %w", err)
	}

	tx, err := s.newAggregatorTx(ctx)
	if err != nil {
		return fmt.Errorf("settle_order: prepare tx: %w", err)
	}

	tx.MoveCall(
		models.SuiAddress(s.packageID),
		"order",
		"settle_order",
		[]transaction.TypeTag{coinTypeTag},
		[]transaction.Argument{
			tx.Object(s.aggregatorCapID),
			tx.Object(s.gatewayObjectID),
			tx.Object(order.GatewayID),
			tx.Pure(lpAddress),
			tx.Pure(settlePercent),
			tx.Pure(splitOrderID),
		},
	)

	resp, err := tx.Execute(
		ctx,
		models.SuiTransactionBlockOptions{
			ShowEffects: true,
			ShowEvents:  true,
		},
		"WaitForLocalExecution",
	)
	if err != nil {
		return fmt.Errorf("settle_order: submit: %w", err)
	}
	if !isTxSuccess(resp) {
		return fmt.Errorf("settle_order: on-chain failure: digest=%s", resp.Digest)
	}

	// Persist the settlement tx digest so the indexer / dashboard can correlate.
	_, err = order.Update().SetTxHash(resp.Digest).Save(ctx)
	if err != nil {
		return fmt.Errorf("settle_order: persist tx digest: %w", err)
	}

	return nil
}

// RefundOrder signs + submits a PTB calling rails::order::refund_order on a
// pending Order shared object. orderID here is the Sui Order object ID
// (lockpaymentorder.GatewayID), not a DB UUID, mirroring the EVM interface
// that took a bytes32 string.
func (s *OrderSui) RefundOrder(ctx context.Context, orderID string) error {
	if s.signer == nil {
		return ErrAggregatorNotConfigured
	}

	order, err := db.Client.LockPaymentOrder.
		Query().
		Where(lockpaymentorder.GatewayIDEQ(orderID)).
		WithToken().
		Only(ctx)
	if err != nil {
		return fmt.Errorf("refund_order: load lock_payment_order by GatewayID=%s: %w", orderID, err)
	}

	// Refund fee — v1 is 0 (no penalty for refunds during early rollout).
	// Tune via config when refund-abuse data warrants it.
	refundFee := uint64(0)

	coinTypeTag, err := parseCoinTypeTag(order.Edges.Token.ContractAddress)
	if err != nil {
		return fmt.Errorf("refund_order: parse coin type: %w", err)
	}

	tx, err := s.newAggregatorTx(ctx)
	if err != nil {
		return fmt.Errorf("refund_order: prepare tx: %w", err)
	}

	tx.MoveCall(
		models.SuiAddress(s.packageID),
		"order",
		"refund_order",
		[]transaction.TypeTag{coinTypeTag},
		[]transaction.Argument{
			tx.Object(s.aggregatorCapID),
			tx.Object(s.gatewayObjectID),
			tx.Object(orderID),
			tx.Pure(refundFee),
		},
	)

	resp, err := tx.Execute(
		ctx,
		models.SuiTransactionBlockOptions{
			ShowEffects: true,
			ShowEvents:  true,
		},
		"WaitForLocalExecution",
	)
	if err != nil {
		return fmt.Errorf("refund_order: submit: %w", err)
	}
	if !isTxSuccess(resp) {
		return fmt.Errorf("refund_order: on-chain failure: digest=%s", resp.Digest)
	}

	_, err = order.Update().SetTxHash(resp.Digest).Save(ctx)
	if err != nil {
		return fmt.Errorf("refund_order: persist tx digest: %w", err)
	}

	return nil
}

// newAggregatorTx returns a transaction.Transaction configured with the
// aggregator signer + a freshly selected gas coin + current reference gas
// price. Used by SettleOrder and RefundOrder, which both submit aggregator-
// signed PTBs against the Gateway.
func (s *OrderSui) newAggregatorTx(ctx context.Context) (*transaction.Transaction, error) {
	gasPriceResp, err := s.client.SuiXGetReferenceGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get reference gas price: %w", err)
	}
	gasPrice, err := strconv.ParseUint(fmt.Sprint(gasPriceResp), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse gas price: %w", err)
	}

	gasCoin, err := s.selectGasCoin(ctx)
	if err != nil {
		return nil, fmt.Errorf("select gas coin: %w", err)
	}

	tx := transaction.NewTransaction()
	tx.SetSuiClient(s.client).
		SetSigner(s.signer).
		SetSender(models.SuiAddress(s.signer.Address)).
		SetGasPrice(gasPrice).
		SetGasBudget(defaultGasBudget).
		SetGasPayment([]transaction.SuiObjectRef{*gasCoin}).
		SetGasOwner(models.SuiAddress(s.signer.Address))

	return tx, nil
}

// selectGasCoin picks a SUI coin from the aggregator wallet to pay gas with.
// Strategy: query SuiXGetCoins (filtered to 0x2::sui::SUI), take the first
// returned coin. Sui's gas coin pool is typically small (we'll have a known
// "gas float" coin in production), so first-result is fine.
func (s *OrderSui) selectGasCoin(ctx context.Context) (*transaction.SuiObjectRef, error) {
	coinsResp, err := s.client.SuiXGetCoins(ctx, models.SuiXGetCoinsRequest{
		Owner:    s.signer.Address,
		CoinType: "0x2::sui::SUI",
		Limit:    1,
	})
	if err != nil {
		return nil, err
	}
	if len(coinsResp.Data) == 0 {
		return nil, errors.New("aggregator wallet has no SUI gas coins — top it up")
	}
	coin := coinsResp.Data[0]
	ref, err := transaction.NewSuiObjectRef(
		models.SuiAddress(coin.CoinObjectId),
		coin.Version,
		models.ObjectDigest(coin.Digest),
	)
	if err != nil {
		return nil, err
	}
	return ref, nil
}

// lpSuiAddress resolves the LP's Sui payout address for the given order's
// token. Reads from ProviderOrderToken.addresses (the JSON list of
// {network, address} pairs that LPs configure during onboarding).
func (s *OrderSui) lpSuiAddress(ctx context.Context, order *ent.LockPaymentOrder) (string, error) {
	if order.Edges.Provider == nil {
		return "", errors.New("order has no provider")
	}
	tokens, err := order.Edges.Provider.QueryOrderTokens().All(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range tokens {
		for _, a := range t.Addresses {
			if a.Network == "sui-mainnet" || a.Network == "sui-testnet" {
				return a.Address, nil
			}
		}
	}
	return "", errors.New("provider has no Sui payout address configured")
}

// parseCoinTypeTag converts a Move coin type string like
// "0xabc...::usdc::USDC" into a transaction.TypeTag the SDK can serialize
// into a PTB type argument.
func parseCoinTypeTag(coinType string) (transaction.TypeTag, error) {
	// Move type string format: <address>::<module>::<TYPE>
	// e.g. "0x2::sui::SUI" or
	//      "0x5d4b302506645c37ff133b98c4b50a5ae14841659738d6d733d59d0d217a93bf::coin::COIN"
	var pkg, mod, name string
	parts := splitMoveType(coinType)
	if len(parts) != 3 {
		return transaction.TypeTag{}, fmt.Errorf("invalid coin type %q (expected addr::module::TYPE)", coinType)
	}
	pkg, mod, name = parts[0], parts[1], parts[2]

	addrBytes, err := transaction.ConvertSuiAddressStringToBytes(models.SuiAddress(pkg))
	if err != nil {
		return transaction.TypeTag{}, fmt.Errorf("invalid coin type package address %q: %w", pkg, err)
	}

	return transaction.TypeTag{
		Struct: &transaction.StructTag{
			Address: *addrBytes,
			Module:  mod,
			Name:    name,
		},
	}, nil
}

// splitMoveType splits "addr::module::TYPE" on "::" into 3 parts. Returns
// fewer than 3 if the string is malformed.
func splitMoveType(s string) []string {
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i+1 < len(s); i++ {
		if s[i] == ':' && s[i+1] == ':' {
			out = append(out, s[start:i])
			start = i + 2
			i++ // skip second ':'
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// isTxSuccess inspects a Sui transaction response and reports whether the
// effects status is "success". block-vision's response shape varies by API
// version; we be defensive and accept any non-empty digest with no error
// status string as success.
func isTxSuccess(resp *models.SuiTransactionBlockResponse) bool {
	if resp == nil || resp.Digest == "" {
		return false
	}
	if resp.Effects.Status.Status == "" {
		return true // some endpoints don't populate Status; presence of a digest is the signal
	}
	return resp.Effects.Status.Status == "success"
}
