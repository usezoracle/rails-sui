// Package lifi is the Go client for the LiFi cross-chain bridge aggregator
// (https://li.quest/v1). Used in Route A to bridge Sui USDC to Base USDC
// before fiat dispatch through settlement's Gateway.
//
// LiFi-on-Sui specifics:
//
//   - Sui chainType is "MVM" (not "SVM"). Live catalog:
//     https://li.quest/v1/chains?chainTypes=MVM
//   - Sui chain id is 9270000000000000.
//   - Tokens on Sui are addressed by Move type strings
//     ("0x...::module::TYPE"), NOT 20-byte addresses.
//   - For Sui as source, the /quote response's `transactionRequest.data`
//     field is a base64-encoded transaction the client deserializes,
//     signs with the Sui keypair, and submits via sui_executeTransactionBlock
//     (NOT the EVM {to, data, value, gas} shape).
package lifi

// QuoteRequest mirrors LiFi's /quote query parameters. Sui-side fields use
// the Sui chain id (9270000000000000) and Move-type tokens. Destination
// fields use standard EVM addressing — Base mainnet = 8453, Sepolia = 84532.
type QuoteRequest struct {
	FromChain    string  `json:"fromChain"`             // Sui = "9270000000000000"
	ToChain      string  `json:"toChain"`               // Base = "8453", Base Sepolia = "84532"
	FromToken    string  `json:"fromToken"`             // Sui: "0x...::usdc::USDC"
	ToToken      string  `json:"toToken"`               // EVM USDC: "0x..." (20-byte)
	FromAmount   string  `json:"fromAmount"`            // in smallest unit (USDC: 6 decimals)
	FromAddress  string  `json:"fromAddress"`           // sender on Sui (our bridge hot wallet)
	ToAddress    string  `json:"toAddress"`             // recipient on destination chain (our EVM hot wallet)
	Slippage     float64 `json:"slippage,omitempty"`    // e.g. 0.003 = 0.3%; default 0.003
	IntegratorID string  `json:"integrator,omitempty"`  // optional LiFi integrator id (rate-limit allocation)
}

// QuoteResponse is the subset of LiFi's /quote response we use. LiFi returns
// many more fields (gas estimates, included steps, tool details); we ignore
// what we don't need.
type QuoteResponse struct {
	ID                 string             `json:"id"`
	Tool               string             `json:"tool"` // e.g. "wormhole", "mayan"
	Estimate           Estimate           `json:"estimate"`
	TransactionRequest TransactionRequest `json:"transactionRequest"`
	IncludedSteps      []IncludedStep     `json:"includedSteps,omitempty"`
}

// Estimate is the LiFi-computed economics of the bridge.
type Estimate struct {
	FromAmount     string `json:"fromAmount"`
	ToAmount       string `json:"toAmount"`
	ToAmountMin    string `json:"toAmountMin"`
	ApprovalAddress string `json:"approvalAddress,omitempty"`
	ExecutionDuration int  `json:"executionDuration"` // seconds
}

// IncludedStep describes one hop in a multi-step quote (e.g. swap + bridge).
type IncludedStep struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "swap" | "cross" | "protocol"
	Tool string `json:"tool"`
}

// TransactionRequest shape depends on the source chain:
//
//   - EVM source: { To, Data, Value, GasPrice, GasLimit } — directly callable
//     via eth_sendTransaction.
//   - MVM source (Sui): { Data: base64 } — client deserializes, signs with
//     Sui key, submits via sui_executeTransactionBlock.
//   - SVM source (Solana): same { Data: base64 } pattern as Sui.
//
// We use only Data for Sui-side; the EVM fields are populated for the
// destination-side leg LiFi might also return (though typically we don't
// need to act on those because LiFi orchestrates the cross-chain delivery).
type TransactionRequest struct {
	Data     string `json:"data"`     // base64 for MVM/SVM; hex calldata for EVM
	To       string `json:"to,omitempty"`
	Value    string `json:"value,omitempty"`
	GasPrice string `json:"gasPrice,omitempty"`
	GasLimit string `json:"gasLimit,omitempty"`
	From     string `json:"from,omitempty"`
	ChainID  int64  `json:"chainId,omitempty"`
}

// StatusRequest is the /status endpoint query.
type StatusRequest struct {
	TxHash    string `json:"txHash"`              // source-chain tx (Sui digest, base58)
	Bridge    string `json:"bridge,omitempty"`    // the tool from QuoteResponse.Tool
	FromChain string `json:"fromChain,omitempty"` // chain id
	ToChain   string `json:"toChain,omitempty"`
}

// StatusResponse is what LiFi reports for an in-flight bridge.
type StatusResponse struct {
	Status        string      `json:"status"`         // "NOT_FOUND" | "INVALID" | "PENDING" | "DONE" | "FAILED"
	Substatus     string      `json:"substatus"`      // e.g. "WAIT_SOURCE_CONFIRMATIONS", "COMPLETED", etc.
	SubstatusMsg  string      `json:"substatusMessage"`
	Tool          string      `json:"tool"`
	Sending       *StatusLeg  `json:"sending,omitempty"`
	Receiving     *StatusLeg  `json:"receiving,omitempty"`
}

// StatusLeg is the source- or destination-side detail of a bridge.
type StatusLeg struct {
	ChainID int64  `json:"chainId"`
	TxHash  string `json:"txHash"`
	Amount  string `json:"amount"`
	Token   Token  `json:"token"`
}

// Token is the LiFi token descriptor (address, symbol, decimals).
type Token struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
	ChainID  int64  `json:"chainId"`
}
