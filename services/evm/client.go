package evm

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ChainConfig groups the per-network knobs we need to make + observe txs.
// One ChainConfig per running EVM network (BSC testnet, BSC mainnet, …).
type ChainConfig struct {
	Name         string
	ChainID      int64
	RPCURL       string
	GatewayAddr  common.Address
	USDCAddr     common.Address
	USDCDecimals uint8
	// SignerHex is the hex (no 0x prefix needed) private key of the
	// aggregator wallet. The wallet receives bridged USDC and signs both
	// approve() and createOrder() calls.
	SignerHex string
}

// Client wraps an ethclient + signer + a tx-submission mutex. One per
// chain. Thread-safe; the dispatcher can submit txs from multiple
// goroutines without nonce collisions.
type Client struct {
	cfg     ChainConfig
	eth     *ethclient.Client
	signer  *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int

	// Serialise tx submission per signer so we never race the nonce.
	// Reading nonce + sending tx must be atomic from our side.
	txMu sync.Mutex
}

// NewClient dials the RPC, parses the signer key, and prepares the
// per-chain client.
func NewClient(ctx context.Context, cfg ChainConfig) (*Client, error) {
	if cfg.RPCURL == "" {
		return nil, fmt.Errorf("evm: RPC URL is empty for %s", cfg.Name)
	}
	if cfg.SignerHex == "" {
		return nil, fmt.Errorf("evm: signer key is empty for %s", cfg.Name)
	}
	if cfg.GatewayAddr == (common.Address{}) {
		return nil, fmt.Errorf("evm: gateway address is empty for %s", cfg.Name)
	}
	if cfg.USDCAddr == (common.Address{}) {
		return nil, fmt.Errorf("evm: USDC address is empty for %s", cfg.Name)
	}

	keyHex := strings.TrimPrefix(cfg.SignerHex, "0x")
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("evm: signer hex decode: %w", err)
	}
	signer, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("evm: signer parse: %w", err)
	}
	pubKey, ok := signer.Public().(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("evm: signer public key not ECDSA")
	}
	from := crypto.PubkeyToAddress(*pubKey)

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	eth, err := ethclient.DialContext(dialCtx, cfg.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("evm: dial %s (%s): %w", cfg.Name, cfg.RPCURL, err)
	}
	return &Client{
		cfg:     cfg,
		eth:     eth,
		signer:  signer,
		from:    from,
		chainID: big.NewInt(cfg.ChainID),
	}, nil
}

// Close releases the underlying RPC connection.
func (c *Client) Close() { c.eth.Close() }

// From returns the aggregator wallet address (the from-address of every
// tx we submit).
func (c *Client) From() common.Address { return c.from }

// Config returns the chain config used to construct this client.
func (c *Client) Config() ChainConfig { return c.cfg }

// Eth exposes the underlying ethclient for read-only operations (event
// log lookups, balance queries) — write operations should go through the
// typed wrappers in gateway.go / erc20.go.
func (c *Client) Eth() *ethclient.Client { return c.eth }

// BalanceNative returns the native-token balance of the aggregator wallet
// in wei. On Base the native token is ETH; on BSC it's BNB; on Polygon
// it's MATIC; etc. Used by the low-balance alert cron.
func (c *Client) BalanceNative(ctx context.Context) (*big.Int, error) {
	return c.eth.BalanceAt(ctx, c.from, nil)
}

// newTransactor builds a bind.TransactOpts ready to submit a tx. Caller
// holds c.txMu while preparing AND submitting the tx so the nonce we
// read stays current.
func (c *Client) newTransactor(ctx context.Context) (*bind.TransactOpts, error) {
	nonce, err := c.eth.PendingNonceAt(ctx, c.from)
	if err != nil {
		return nil, fmt.Errorf("evm: pending nonce: %w", err)
	}
	gasPrice, err := c.eth.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("evm: suggest gas price: %w", err)
	}
	opts, err := bind.NewKeyedTransactorWithChainID(c.signer, c.chainID)
	if err != nil {
		return nil, fmt.Errorf("evm: new transactor: %w", err)
	}
	opts.Context = ctx
	opts.Nonce = big.NewInt(int64(nonce))
	opts.GasPrice = gasPrice
	// GasLimit left zero — bind estimates per-call. Callers can override
	// (e.g. createOrder uses a fixed limit to avoid running an EstimateGas
	// against contracts that revert on dry-run when allowance is fresh).
	return opts, nil
}

// waitMined blocks until the tx is mined (or ctx cancels). Returns a
// non-nil receipt or a non-nil error.
func (c *Client) waitMined(ctx context.Context, tx *types.Transaction) (*types.Receipt, error) {
	// bind.WaitMined polls at 100ms intervals; on BSC blocks are ~3s so
	// we cap by ctx, not by manual timeout.
	rcpt, err := bind.WaitMined(ctx, c.eth, tx)
	if err != nil {
		return nil, fmt.Errorf("evm: wait mined: %w", err)
	}
	if rcpt.Status != types.ReceiptStatusSuccessful {
		return rcpt, fmt.Errorf("evm: tx reverted (hash=%s)", tx.Hash().Hex())
	}
	return rcpt, nil
}
