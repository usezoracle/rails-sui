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

// erc20ABI is the parsed ABI for the embedded ERC20ABI JSON. Module-scope
// so we parse once.
var erc20ABI ethabi.ABI

func init() {
	parsed, err := ethabi.JSON(strings.NewReader(ERC20ABI))
	if err != nil {
		panic(fmt.Sprintf("evm: parse ERC20 ABI: %s", err))
	}
	erc20ABI = parsed
}

// ERC20 binds an ERC-20 contract on the active chain to its caller's
// signer. Construct via Client.USDC() for the USDC instance.
type ERC20 struct {
	c    *Client
	addr common.Address
	b    *bind.BoundContract
}

// USDC returns an ERC20 binding for the chain's USDC address.
func (c *Client) USDC() *ERC20 {
	return &ERC20{
		c:    c,
		addr: c.cfg.USDCAddr,
		b:    bind.NewBoundContract(c.cfg.USDCAddr, erc20ABI, c.eth, c.eth, c.eth),
	}
}

// Allowance reads the current allowance owner→spender (subunits).
func (t *ERC20) Allowance(ctx context.Context, owner, spender common.Address) (*big.Int, error) {
	var out []any
	if err := t.b.Call(&bind.CallOpts{Context: ctx}, &out, "allowance", owner, spender); err != nil {
		return nil, fmt.Errorf("erc20: allowance: %w", err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("erc20: allowance: unexpected return type")
	}
	return v, nil
}

// BalanceOf reads the token balance of `owner` in subunits.
func (t *ERC20) BalanceOf(ctx context.Context, owner common.Address) (*big.Int, error) {
	var out []any
	if err := t.b.Call(&bind.CallOpts{Context: ctx}, &out, "balanceOf", owner); err != nil {
		return nil, fmt.Errorf("erc20: balanceOf: %w", err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("erc20: balanceOf: unexpected return type")
	}
	return v, nil
}

// Decimals reads the on-chain decimals. We typically know this already
// (ChainConfig.USDCDecimals) but this is a useful safety check on first
// boot.
func (t *ERC20) Decimals(ctx context.Context) (uint8, error) {
	var out []any
	if err := t.b.Call(&bind.CallOpts{Context: ctx}, &out, "decimals"); err != nil {
		return 0, fmt.Errorf("erc20: decimals: %w", err)
	}
	v, ok := out[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("erc20: decimals: unexpected return type")
	}
	return v, nil
}

// Approve submits an approve(spender, amount) tx, waits for it to mine,
// and returns the receipt. Returns nil receipt + nil error if the
// allowance is already ≥ amount (no-op).
//
// Holds the client's tx mutex for the duration so nonces don't race.
func (t *ERC20) Approve(ctx context.Context, spender common.Address, amount *big.Int) (*types.Receipt, error) {
	current, err := t.Allowance(ctx, t.c.from, spender)
	if err != nil {
		return nil, err
	}
	if current.Cmp(amount) >= 0 {
		return nil, nil
	}

	t.c.txMu.Lock()
	defer t.c.txMu.Unlock()

	opts, err := t.c.newTransactor(ctx)
	if err != nil {
		return nil, err
	}
	// Gas limit: approve is well-known ~46k; pad to 80k.
	opts.GasLimit = 80_000

	tx, err := t.b.Transact(opts, "approve", spender, amount)
	if err != nil {
		return nil, fmt.Errorf("erc20: approve: %w", err)
	}
	return t.c.waitMined(ctx, tx)
}
