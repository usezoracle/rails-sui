// Package cctp is the direct Circle CCTP v1 rail for Route A's bridge
// fallback: burn native USDC on Sui, fetch Circle's attestation, mint
// native USDC on Base. It is the same underlying rail Wormhole's CCTP
// product (and LiFi's "cctp" tool) ride on, minus their relayer layer —
// we redeem on Base ourselves with the aggregator signer the dispatcher
// already runs, so the only external dependency is Circle's attestation
// service.
//
// The package is deliberately self-contained: nothing in here imports
// the dispatcher, LiFi, or ent. If a fallback bridge misbehaves, the
// bug is in this package (or its wiring in services/route_a_cctp.go),
// never in the LiFi path.
//
// Flow (three inspectable steps, each idempotent to re-poll):
//
//  1. SubmitBurn        — PTB: merge/split aggregator USDC to the exact
//     amount, call token_messenger_minter::deposit_for_burn with
//     mint_recipient = our Base hot wallet. One Sui tx digest out.
//  2. Iris.MessageFor   — GET /v1/messages/{suiDomain}/{digest}; Circle
//     indexes the burn and (after Sui finality) attaches an attestation.
//     Stateless: everything needed to finish lives on-chain + at Circle,
//     keyed by the digest we persisted.
//  3. evm.ReceiveMessage — submit receiveMessage(message, attestation)
//     on Base's MessageTransmitter; Circle's TokenMinter mints USDC
//     1:1 to mint_recipient. No slippage, no quote, no tool selection.
//
// References:
//   - Sui packages/objects: https://developers.circle.com/cctp/v1/sui-packages
//   - EVM contracts:        https://developers.circle.com/cctp/v1/evm-smart-contracts
//   - Attestation API:      https://developers.circle.com/cctp/v1/cctp-apis
package cctp

// Network bundles every chain/contract constant the CCTP v1 Sui→Base
// path needs. Constants, not config: these are Circle's canonical
// deployments and change never (v1 is feature-frozen "legacy"); baking
// them in removes an entire class of misconfiguration. Only the Iris
// URL is overridable (tests, proxies) — see WithIrisURL.
type Network struct {
	// Circle domain ids (NOT chain ids): Sui = 8, Base = 6.
	SuiDomain  uint32
	BaseDomain uint32

	// Sui-side CCTP v1 deployment.
	SuiTokenMessengerPackage   string // token_messenger_minter package id
	SuiTokenMessengerState     string // shared State object (immutable ref in deposit_for_burn)
	SuiMessageTransmitterState string // shared MessageTransmitterState (mutable ref)
	SuiUSDCTreasury            string // shared Treasury<USDC> (mutable ref)
	SuiDenyList                string // 0x403 system DenyList (immutable ref)
	SuiUSDCCoinType            string // canonical native-USDC coin type — the fallback's eligibility gate

	// Base-side CCTP v1 deployment.
	BaseMessageTransmitter string

	// Circle attestation service.
	IrisBaseURL string
}

// suiDenyListObjectID is the Sui framework's system DenyList shared
// object — fixed address on every Sui network.
const suiDenyListObjectID = "0x0000000000000000000000000000000000000000000000000000000000000403"

var mainnet = Network{
	SuiDomain:  8,
	BaseDomain: 6,

	SuiTokenMessengerPackage:   "0x2aa6c5d56376c371f88a6cc42e852824994993cb9bab8d3e6450cbe3cb32b94e",
	SuiTokenMessengerState:     "0x45993eecc0382f37419864992c12faee2238f5cfe22b98ad3bf455baf65c8a2f",
	SuiMessageTransmitterState: "0xf68268c3d9b1df3215f2439400c1c4ea08ac4ef4bb7d6f3ca6a2a239e17510af",
	SuiUSDCTreasury:            "0x57d6725e7a8b49a7b2a612f6bd66ab5f39fc95332ca48be421c3229d514a6de7",
	SuiDenyList:                suiDenyListObjectID,
	SuiUSDCCoinType:            "0xdba34672e30cb065b1f93e3ab55318768fd6fef66c15942c9f7cb846e2f900e7::usdc::USDC",

	BaseMessageTransmitter: "0xAD09780d193884d503182aD4588450C416D6F9D4",

	IrisBaseURL: "https://iris-api.circle.com",
}

var testnet = Network{
	SuiDomain:  8,
	BaseDomain: 6,

	SuiTokenMessengerPackage:   "0x31cc14d80c175ae39777c0238f20594c6d4869cfab199f40b69f3319956b8beb",
	SuiTokenMessengerState:     "0x5252abd1137094ed1db3e0d75bc36abcd287aee4bc310f8e047727ef5682e7c2",
	SuiMessageTransmitterState: "0x98234bd0fa9ac12cc0a20a144a22e36d6a32f7e0a97baaeaf9c76cdc6d122d2e",
	SuiUSDCTreasury:            "0x7170137d4a6431bf83351ac025baf462909bffe2877d87716374fb42b9629ebe",
	SuiDenyList:                suiDenyListObjectID,
	SuiUSDCCoinType:            "0xa1ec7fc00a6f40db9693ad1415d0c193ad3906494428cf252621037bd7117e29::usdc::USDC",

	BaseMessageTransmitter: "0x7865fAfC2db2093669d92c0F33AeEF291086BEFD",

	IrisBaseURL: "https://iris-api-sandbox.circle.com",
}

// ForBaseChainID maps the dispatcher's destination chain id to the
// matching CCTP deployment pair: Base mainnet (8453) ↔ Sui mainnet,
// Base Sepolia (84532) ↔ Sui testnet. Unknown chain → ok=false and the
// fallback simply never engages.
func ForBaseChainID(baseChainID int64) (Network, bool) {
	switch baseChainID {
	case 8453:
		return mainnet, true
	case 84532:
		return testnet, true
	default:
		return Network{}, false
	}
}

// WithIrisURL returns a copy of the network with the attestation host
// overridden (CCTP_IRIS_URL env; httptest servers in tests). Empty
// override keeps the canonical host.
func (n Network) WithIrisURL(url string) Network {
	if url != "" {
		n.IrisBaseURL = url
	}
	return n
}
