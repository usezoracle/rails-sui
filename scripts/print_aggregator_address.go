//go:build ignore

package main

import (
	"context"
	"fmt"

	"github.com/usezoracle/rails-sui/config"
	suisdk "github.com/block-vision/sui-go-sdk/sui"
	suimodels "github.com/block-vision/sui-go-sdk/models"
	suisigner "github.com/block-vision/sui-go-sdk/signer"
)

func main() {
	conf := config.OrderConfig()

	if len(conf.SuiAggregatorPrivateKey) != 32 {
		fmt.Printf("Error: SuiAggregatorPrivateKey has length %d, expected 32\n", len(conf.SuiAggregatorPrivateKey))
		return
	}

	signer := suisigner.NewSigner(conf.SuiAggregatorPrivateKey)
	fmt.Printf("Aggregator Wallet Address: %s\n", signer.Address)

	apiClient := suisdk.NewSuiClient(conf.SuiRpcURL)
	suiClient, _ := apiClient.(*suisdk.Client)

	ctx := context.Background()

	// Get SUI balance
	suiBal, err := suiClient.SuiXGetBalance(ctx, suimodels.SuiXGetBalanceRequest{
		Owner:    signer.Address,
		CoinType: "0x2::sui::SUI",
	})
	if err != nil {
		fmt.Printf("Error getting SUI balance: %v\n", err)
	} else {
		fmt.Printf("SUI Balance: %s MIST\n", suiBal.TotalBalance)
	}

	// Get USDC balance
	usdcBal, err := suiClient.SuiXGetBalance(ctx, suimodels.SuiXGetBalanceRequest{
		Owner:    signer.Address,
		CoinType: "0xdba34672e30cb065b1f93e3ab55318768fd6fef66c15942c9f7cb846e2f900e7::usdc::USDC",
	})
	if err != nil {
		fmt.Printf("Error getting USDC balance: %v\n", err)
	} else {
		fmt.Printf("USDC Balance: %s\n", usdcBal.TotalBalance)
	}
}
