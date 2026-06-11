package config

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/viper"
)

// OrderConfiguration type defines payment order configurations.
//
// EVM/Tron-specific fields (BundlerUrl*, PaymasterUrl*, EntryPoint*,
// TronProApiKey, ActiveAAService) have been removed during the Sui port.
// Sui equivalents are below.
type OrderConfiguration struct {
	// Generic, chain-agnostic.
	OrderFulfillmentValidity         time.Duration
	ReceiveAddressValidity           time.Duration
	OrderRequestValidity             time.Duration
	BucketQueueRebuildInterval       int // in hours
	RefundCancellationCount          int
	PercentDeviationFromExternalRate decimal.Decimal
	PercentDeviationFromMarketRate   decimal.Decimal

	// Sui-specific.
	SuiRpcURL               string
	SuiWsURL                string // wss:// endpoint for event subscriptions (e.g. BlockVision); falls back to SuiRpcURL scheme-converted
	SuiGrpcURL              string
	SuiGrpcToken            string
	SuiGatewayPackageID     string
	SuiGatewayObjectID      string
	SuiAggregatorCapID      string
	SuiAggregatorPrivateKey []byte // raw 32-byte Ed25519 seed; hex-decoded from SUI_AGGREGATOR_PRIVATE_KEY env var

	// LiFi (Route A bridging).
	LiFiBaseURL  string
	LiFiAPIKey   string // optional; free tier when empty (rate-limited)

	// Direct-CCTP bridge fallback (services/route_a_cctp.go) — engages
	// only after repeated LiFi quote failures on USDC-source orders.
	CCTPFallbackEnabled bool   // kill switch; default true
	CCTPIrisURL         string // optional override of Circle's attestation host (tests/proxies)

	// Route C — managed-float instant payouts (route_a_treasury.go).
	// CardTapMode picks which rail card taps settle on: "lp" (today's
	// direct Paycrest flow) or "treasury" (instant payout from float,
	// Paycrest reloads us). The float fields describe OUR bank account:
	// the BaaS payout debit source AND the reload recipient Paycrest
	// pays back into.
	CardTapMode                string
	TreasuryFloatInstitution   string // bank code of the float account
	TreasuryFloatAccountNumber string // float account number
	TreasuryFloatAccountName   string // float account display name

	// Shinami Gas Station — sponsors all aggregator-initiated Move
	// calls (CreateOrder, SettleOrder, RefundOrder, DebitCard). When
	// empty, the OrderSui code path falls back to a typed error so
	// misconfiguration surfaces immediately rather than silently
	// failing. See services/shinami_gas/client.go.
	ShinamiGasAPIKey string
	ShinamiGasBaseURL string

	// Base — Route A's EVM destination chain. Same env block works for
	// Base mainnet (8453) and Base Sepolia (84532); flip BASE_CHAIN_ID
	// + BASE_GATEWAY_CONTRACT + BASE_USDC_CONTRACT + BASE_RPC_URL to
	// switch networks. USDC on Base is 6-decimal native Circle.
	BaseRpcURL              string
	BaseAggregatorAddress   string // our Base hot wallet; receives bridged USDC
	BaseGatewayContract     string // settlement Gateway proxy on the active network
	BaseUSDCContract        string // Circle USDC ERC-20 on the active network
	BaseChainID             int64  // 8453 mainnet, 84532 Sepolia
	BaseSignerKey           string // hex private key for the aggregator wallet (signs approve + createOrder)
	BaseSenderFeeBPS        int64  // sender fee skim charged on each order, in basis points (50 = 0.5%)
	BaseNativeLowThresholdWei string // big.Int as string; below this we Slack-alert ops (native = ETH on Base)
	BaseUSDCDecimals        int    // 6 on Base for Circle native USDC

	// Settlement aggregator (default upstream: api.paycrest.io).
	SettlementAPIURL           string
	SettlementPubkeyTTLSeconds int
	SettlementSenderAPIKeyID   string        // UUID identifying our sender for attribution + LP routing
	SettlementPollInterval     time.Duration // cadence for advanceDispatching status polling
}

// OrderConfig sets the order configuration
func OrderConfig() *OrderConfiguration {
	viper.SetDefault("RECEIVE_ADDRESS_VALIDITY", 30)
	viper.SetDefault("ORDER_REQUEST_VALIDITY", 120)
	viper.SetDefault("ORDER_FULFILLMENT_VALIDITY", 10)
	viper.SetDefault("BUCKET_QUEUE_REBUILD_INTERVAL", 1)
	viper.SetDefault("REFUND_CANCELLATION_COUNT", 3)
	viper.SetDefault("NETWORK_FEE", 0.05)
	viper.SetDefault("PERCENT_DEVIATION_FROM_EXTERNAL_RATE", 0.01)
	viper.SetDefault("PERCENT_DEVIATION_FROM_MARKET_RATE", 0.1)
	viper.SetDefault("SUI_RPC_URL", "https://fullnode.testnet.sui.io:443")
	viper.SetDefault("LIFI_BASE_URL", "https://li.quest/v1")
	viper.SetDefault("CCTP_FALLBACK_ENABLED", true)
	viper.SetDefault("CARD_TAP_MODE", "lp") // flip to "treasury" to enable Route C instant payouts
	viper.SetDefault("BASE_RPC_URL", "https://sepolia.base.org")                       // Sepolia default; mainnet = https://mainnet.base.org
	viper.SetDefault("BASE_CHAIN_ID", 84532)                                           // Base Sepolia; mainnet = 8453
	viper.SetDefault("BASE_SENDER_FEE_BPS", 50)                                        // 0.5% sender fee
	viper.SetDefault("BASE_NATIVE_LOW_THRESHOLD_WEI", "10000000000000000")             // 0.01 ETH (Base L2 gas is cheap)
	viper.SetDefault("BASE_USDC_DECIMALS", 6)
	viper.SetDefault("SETTLEMENT_API_URL", "https://api.paycrest.io")
	viper.SetDefault("SETTLEMENT_PUBKEY_CACHE_TTL_SECONDS", 3600)
	viper.SetDefault("SETTLEMENT_POLL_INTERVAL_SECONDS", 30)

	// SUI_AGGREGATOR_PRIVATE_KEY is expected hex-encoded; decoded once at startup.
	aggregatorKeyHex := viper.GetString("SUI_AGGREGATOR_PRIVATE_KEY")
	var aggregatorKey []byte
	if aggregatorKeyHex != "" {
		decoded, err := hex.DecodeString(aggregatorKeyHex)
		if err != nil {
			panic(fmt.Sprintf("SUI_AGGREGATOR_PRIVATE_KEY hex decode failed: %s", err))
		}
		aggregatorKey = decoded
	}

	return &OrderConfiguration{
		OrderFulfillmentValidity:         time.Duration(viper.GetInt("ORDER_FULFILLMENT_VALIDITY")) * time.Minute,
		ReceiveAddressValidity:           time.Duration(viper.GetInt("RECEIVE_ADDRESS_VALIDITY")) * time.Minute,
		OrderRequestValidity:             time.Duration(viper.GetInt("ORDER_REQUEST_VALIDITY")) * time.Second,
		BucketQueueRebuildInterval:       viper.GetInt("BUCKET_QUEUE_REBUILD_INTERVAL"),
		RefundCancellationCount:          viper.GetInt("REFUND_CANCELLATION_COUNT"),
		PercentDeviationFromExternalRate: decimal.NewFromFloat(viper.GetFloat64("PERCENT_DEVIATION_FROM_EXTERNAL_RATE")),
		PercentDeviationFromMarketRate:   decimal.NewFromFloat(viper.GetFloat64("PERCENT_DEVIATION_FROM_MARKET_RATE")),
		SuiRpcURL:                        viper.GetString("SUI_RPC_URL"),
		SuiWsURL:                         viper.GetString("SUI_WS_URL"),
		SuiGrpcURL:                       viper.GetString("SUI_GRPC_URL"),
		SuiGrpcToken:                     viper.GetString("SUI_GRPC_TOKEN"),
		SuiGatewayPackageID:              viper.GetString("SUI_GATEWAY_PACKAGE_ID"),
		SuiGatewayObjectID:               viper.GetString("SUI_GATEWAY_OBJECT_ID"),
		SuiAggregatorCapID:               viper.GetString("SUI_AGGREGATOR_CAP_ID"),
		SuiAggregatorPrivateKey:          aggregatorKey,
		LiFiBaseURL:                      viper.GetString("LIFI_BASE_URL"),
		LiFiAPIKey:                       viper.GetString("LIFI_API_KEY"),
		CCTPFallbackEnabled:              viper.GetBool("CCTP_FALLBACK_ENABLED"),
		CCTPIrisURL:                      viper.GetString("CCTP_IRIS_URL"),
		CardTapMode:                      viper.GetString("CARD_TAP_MODE"),
		TreasuryFloatInstitution:         viper.GetString("TREASURY_FLOAT_INSTITUTION"),
		TreasuryFloatAccountNumber:       viper.GetString("TREASURY_FLOAT_ACCOUNT_NUMBER"),
		TreasuryFloatAccountName:         viper.GetString("TREASURY_FLOAT_ACCOUNT_NAME"),
		ShinamiGasAPIKey:                 viper.GetString("SHINAMI_GAS_API_KEY"),
		ShinamiGasBaseURL:                viper.GetString("SHINAMI_GAS_BASE_URL"),
		BaseRpcURL:                       viper.GetString("BASE_RPC_URL"),
		BaseAggregatorAddress:            viper.GetString("BASE_AGGREGATOR_ADDRESS"),
		BaseGatewayContract:              viper.GetString("BASE_GATEWAY_CONTRACT"),
		BaseUSDCContract:                 viper.GetString("BASE_USDC_CONTRACT"),
		BaseChainID:                      viper.GetInt64("BASE_CHAIN_ID"),
		BaseSignerKey:                    viper.GetString("BASE_SIGNER_KEY"),
		BaseSenderFeeBPS:                 viper.GetInt64("BASE_SENDER_FEE_BPS"),
		BaseNativeLowThresholdWei:        viper.GetString("BASE_NATIVE_LOW_THRESHOLD_WEI"),
		BaseUSDCDecimals:                 viper.GetInt("BASE_USDC_DECIMALS"),
		SettlementAPIURL:                 viper.GetString("SETTLEMENT_API_URL"),
		SettlementPubkeyTTLSeconds:       viper.GetInt("SETTLEMENT_PUBKEY_CACHE_TTL_SECONDS"),
		SettlementSenderAPIKeyID:         viper.GetString("SETTLEMENT_SENDER_API_KEY_ID"),
		SettlementPollInterval:           time.Duration(viper.GetInt("SETTLEMENT_POLL_INTERVAL_SECONDS")) * time.Second,
	}
}

func init() {
	if err := SetupConfig(); err != nil {
		panic(fmt.Sprintf("config SetupConfig() error: %s", err))
	}
}
