// Stubs for the EVM/Tron blockchain test helpers that used to live in
// blockchain.go. The original file deployed a local Ganache + ERC20 + AA
// factory for integration tests; the Sui equivalent will use Sui CLI's
// devnet faucet + the Move package's test scenario.
//
// These stubs exist so utils/test/db.go (which references them in fixture
// setup) compiles. Calling them at runtime returns an error — the tests
// using them are EVM-specific and will be replaced during the test-suite
// port to Sui.
package test

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"

	"github.com/usezoracle/rails-sui/types"
)

var errEVMTestStub = errors.New("rails: EVM test helper not available in Sui-only build (rewrite test suite for Sui)")

// SetUpTestBlockchain stub. Original: dialed a local Ganache node.
func SetUpTestBlockchain() (types.RPCClient, error) {
	return nil, errEVMTestStub
}

// DeployERC20Contract stub. Original: deployed an ERC20 to local Ganache.
func DeployERC20Contract(client types.RPCClient) (*common.Address, error) {
	return nil, errEVMTestStub
}

// CreateSmartAddress stub. Original: derived an AA SimpleAccount address.
func CreateSmartAddress(ctx context.Context, client types.RPCClient) (string, []byte, error) {
	return "", nil, errEVMTestStub
}
