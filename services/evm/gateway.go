package evm

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	ethabi "github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// gatewayABI is parsed once; reused across all Gateway instances on a chain.
var gatewayABI ethabi.ABI

func init() {
	parsed, err := ethabi.JSON(strings.NewReader(GatewayABI))
	if err != nil {
		panic(fmt.Sprintf("evm: parse Gateway ABI: %s", err))
	}
	gatewayABI = parsed
}

// Gateway binds the settlement Gateway contract on the active chain.
type Gateway struct {
	c    *Client
	addr common.Address
	b    *bind.BoundContract
}

// Gateway returns the bound Gateway for the chain in this client.
func (c *Client) Gateway() *Gateway {
	return &Gateway{
		c:    c,
		addr: c.cfg.GatewayAddr,
		b:    bind.NewBoundContract(c.cfg.GatewayAddr, gatewayABI, c.eth, c.eth, c.eth),
	}
}

// CreateOrderParams groups the args for Gateway.createOrder.
type CreateOrderParams struct {
	Token              common.Address
	Amount             *big.Int // token subunits (e.g. USDC at 6-dec on Base)
	Rate               *big.Int // uint96; fiat-per-token × 100 fixed-point
	SenderFeeRecipient common.Address
	SenderFee          *big.Int
	RefundAddress      common.Address
	MessageHash        string // base64 RSA ciphertext of recipient JSON
}

// CreateOrderResult is what we surface to callers after a successful submit.
type CreateOrderResult struct {
	TxHash  common.Hash
	OrderID common.Hash // bytes32
	GasUsed uint64
}

// CreateOrder submits a createOrder tx and waits for it to mine. On
// success, parses the OrderCreated event log and returns the bytes32
// orderId. Errors out on revert / tx-not-found / log-missing.
//
// Holds the client's tx mutex for the duration.
func (g *Gateway) CreateOrder(ctx context.Context, p CreateOrderParams) (*CreateOrderResult, error) {
	if p.Amount == nil || p.Rate == nil || p.SenderFee == nil {
		return nil, fmt.Errorf("gateway: amount/rate/senderFee must be non-nil")
	}
	if p.MessageHash == "" {
		return nil, fmt.Errorf("gateway: messageHash must be set")
	}

	g.c.txMu.Lock()
	defer g.c.txMu.Unlock()

	opts, err := g.c.newTransactor(ctx)
	if err != nil {
		return nil, err
	}
	// createOrder is the heaviest call we make; cap at 400k. Real usage
	// has been observed at ~280k on BSC; Base similar.
	opts.GasLimit = 400_000

	tx, err := g.b.Transact(opts, "createOrder",
		p.Token,
		p.Amount,
		p.Rate,
		p.SenderFeeRecipient,
		p.SenderFee,
		p.RefundAddress,
		p.MessageHash,
	)
	if err != nil {
		return nil, fmt.Errorf("gateway: createOrder submit: %w", err)
	}
	rcpt, err := g.c.waitMined(ctx, tx)
	if err != nil {
		return nil, err
	}

	orderID, err := parseOrderCreated(rcpt)
	if err != nil {
		return nil, err
	}
	return &CreateOrderResult{
		TxHash:  tx.Hash(),
		OrderID: orderID,
		GasUsed: rcpt.GasUsed,
	}, nil
}

// parseOrderCreated finds the first OrderCreated log in the receipt and
// extracts orderId. We scan in order; createOrder emits exactly one such
// event per call.
func parseOrderCreated(rcpt *types.Receipt) (common.Hash, error) {
	ev, ok := gatewayABI.Events["OrderCreated"]
	if !ok {
		return common.Hash{}, fmt.Errorf("gateway: OrderCreated event not in ABI")
	}
	topic := ev.ID
	for _, l := range rcpt.Logs {
		if len(l.Topics) == 0 || l.Topics[0] != topic {
			continue
		}
		// Non-indexed fields: protocolFee, orderId, rate, messageHash
		out := map[string]any{}
		if err := gatewayABI.UnpackIntoMap(out, "OrderCreated", l.Data); err != nil {
			return common.Hash{}, fmt.Errorf("gateway: unpack OrderCreated: %w", err)
		}
		raw, ok := out["orderId"]
		if !ok {
			return common.Hash{}, fmt.Errorf("gateway: OrderCreated missing orderId")
		}
		// abi decodes bytes32 to [32]byte
		arr, ok := raw.([32]byte)
		if !ok {
			return common.Hash{}, fmt.Errorf("gateway: OrderCreated orderId wrong type %T", raw)
		}
		return common.Hash(arr), nil
	}
	return common.Hash{}, fmt.Errorf("gateway: OrderCreated log not found in receipt (hash=%s)", rcpt.TxHash.Hex())
}

// OrderInfo mirrors the Solidity struct returned by getOrderInfo.
type OrderInfo struct {
	Sender             common.Address
	Token              common.Address
	SenderFeeRecipient common.Address
	SenderFee          *big.Int
	ProtocolFee        *big.Int
	IsFulfilled        bool
	IsRefunded         bool
	RefundAddress      common.Address
	CurrentBPS         *big.Int
	Amount             *big.Int
}

// GetOrderInfo reads the on-chain state for a known orderId. Returns
// zero-valued struct when the order doesn't exist (sender == zero address).
func (g *Gateway) GetOrderInfo(ctx context.Context, orderID common.Hash) (*OrderInfo, error) {
	var out []any
	if err := g.b.Call(&bind.CallOpts{Context: ctx}, &out, "getOrderInfo", [32]byte(orderID)); err != nil {
		return nil, fmt.Errorf("gateway: getOrderInfo: %w", err)
	}
	if len(out) != 1 {
		return nil, fmt.Errorf("gateway: getOrderInfo: unexpected output arity %d", len(out))
	}
	// The bound contract returns the tuple as a generic struct via reflection.
	// We coerce through the ABI-emitted shape.
	tuple, ok := out[0].(struct {
		Sender             common.Address `json:"sender"`
		Token              common.Address `json:"token"`
		SenderFeeRecipient common.Address `json:"senderFeeRecipient"`
		SenderFee          *big.Int       `json:"senderFee"`
		ProtocolFee        *big.Int       `json:"protocolFee"`
		IsFulfilled        bool           `json:"isFulfilled"`
		IsRefunded         bool           `json:"isRefunded"`
		RefundAddress      common.Address `json:"refundAddress"`
		CurrentBPS         *big.Int       `json:"currentBPS"`
		Amount             *big.Int       `json:"amount"`
	})
	if !ok {
		return nil, fmt.Errorf("gateway: getOrderInfo: unexpected tuple shape %T", out[0])
	}
	return &OrderInfo{
		Sender:             tuple.Sender,
		Token:              tuple.Token,
		SenderFeeRecipient: tuple.SenderFeeRecipient,
		SenderFee:          tuple.SenderFee,
		ProtocolFee:        tuple.ProtocolFee,
		IsFulfilled:        tuple.IsFulfilled,
		IsRefunded:         tuple.IsRefunded,
		RefundAddress:      tuple.RefundAddress,
		CurrentBPS:         tuple.CurrentBPS,
		Amount:             tuple.Amount,
	}, nil
}
