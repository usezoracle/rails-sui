package cctp

import (
	"fmt"
	"strings"

	suimodels "github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/transaction"
)

// structTypeTag parses a Move struct coin type ("0xPKG::module::Name",
// no generic params — USDC has none) into the BCS TypeTag the PTB
// MoveCall needs for its type argument.
func structTypeTag(coinType string) (*transaction.TypeTag, error) {
	parts := strings.Split(coinType, "::")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, fmt.Errorf("cctp: coin type %q is not pkg::module::Name", coinType)
	}
	addrBytes, err := transaction.ConvertSuiAddressStringToBytes(suimodels.SuiAddress(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("cctp: coin type package %q: %w", parts[0], err)
	}
	return &transaction.TypeTag{
		Struct: &transaction.StructTag{
			Address: *addrBytes,
			Module:  parts[1],
			Name:    parts[2],
		},
	}, nil
}
