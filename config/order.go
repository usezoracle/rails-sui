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
	SuiGatewayPackageID     string
	SuiGatewayObjectID      string
	SuiAggregatorCapID      string
	SuiAggregatorPrivateKey []byte // raw 32-byte Ed25519 seed; hex-decoded from SUI_AGGREGATOR_PRIVATE_KEY env var

	// LiFi (Route A bridging).
	LiFiBaseURL  string
	LiFiAPIKey   string // optional; free tier when empty (rate-limited)

	// BSC (Route A destination). The BSC USDC type used as LiFi's toToken
	// is hard-coded since Circle's address is canonical and immutable.
	BSCRpcURL              string
	BSCAggregatorAddress   string // our BSC hot wallet, receives bridged USDC
	BSCGatewayContract     string // existing EVM Gateway address (for mode=lp dispatch)
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
	viper.SetDefault("BSC_RPC_URL", "https://bsc-dataseed.bnbchain.org")

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
		SuiGatewayPackageID:              viper.GetString("SUI_GATEWAY_PACKAGE_ID"),
		SuiGatewayObjectID:               viper.GetString("SUI_GATEWAY_OBJECT_ID"),
		SuiAggregatorCapID:               viper.GetString("SUI_AGGREGATOR_CAP_ID"),
		SuiAggregatorPrivateKey:          aggregatorKey,
		LiFiBaseURL:                      viper.GetString("LIFI_BASE_URL"),
		LiFiAPIKey:                       viper.GetString("LIFI_API_KEY"),
		BSCRpcURL:                        viper.GetString("BSC_RPC_URL"),
		BSCAggregatorAddress:             viper.GetString("BSC_AGGREGATOR_ADDRESS"),
		BSCGatewayContract:               viper.GetString("BSC_GATEWAY_CONTRACT"),
	}
}

func init() {
	if err := SetupConfig(); err != nil {
		panic(fmt.Sprintf("config SetupConfig() error: %s", err))
	}
}
