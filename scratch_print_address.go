package main

import (
	"fmt"
	"github.com/usezoracle/rails-sui/config"
	"github.com/block-vision/sui-go-sdk/signer"
)

func main() {
	conf := config.OrderConfig()
	if len(conf.SuiAggregatorPrivateKey) != 32 {
		fmt.Printf("PrivateKey length is %d, expected 32\n", len(conf.SuiAggregatorPrivateKey))
		return
	}
	s := signer.NewSigner(conf.SuiAggregatorPrivateKey)
	fmt.Printf("Aggregator Sui Address: %s\n", s.Address)
}
