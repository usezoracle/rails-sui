package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/routers"
	"github.com/usezoracle/rails-sui/services/baas"
	"github.com/usezoracle/rails-sui/services/baas/fintava"
	"github.com/usezoracle/rails-sui/services/baas/korapay"
	"github.com/usezoracle/rails-sui/services/baas/mfb"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/tasks"
	"github.com/usezoracle/rails-sui/utils/logger"
)

func main() {
	// Set timezone
	conf := config.ServerConfig()
	loc, _ := time.LoadLocation(conf.Timezone)
	time.Local = loc

	// Connect to the database
	DSN := config.DBConfig()

	if err := storage.DBConnection(DSN); err != nil {
		logger.Fatalf("database DBConnection: %s", err)
	}

	defer storage.GetClient().Close()

	// Initialize Redis
	if err := storage.InitializeRedis(); err != nil {
		logger.Fatalf("Redis initialization: %s", err)
	}

	// Initialize the BaaS (NGN fiat) rail behind the provider-agnostic baas
	// adapter, selected by BAAS_PROVIDER (default "safehaven"). Absent
	// credentials are non-fatal (Route A `mode=lp` and other flows don't need
	// it); present but invalid credentials fail fast so misconfiguration
	// surfaces at boot. To switch providers, add a case here — no consumer
	// changes (they all depend on baas.Default()).
	initBaaSRail()

	// Background workers: Redis keyspace listeners + cron jobs (event
	// indexers, Route-A dispatcher, reconcilers). DISABLE_BACKGROUND_JOBS=true
	// runs this process API-only — exactly one worker instance should ever
	// run against a database, so side-by-side instances (canary builds,
	// separate HTTP scaling) must set this flag.
	viper.SetDefault("DISABLE_BACKGROUND_JOBS", false)
	if viper.GetBool("DISABLE_BACKGROUND_JOBS") {
		logger.Infof("DISABLE_BACKGROUND_JOBS=true — API-only mode (no keyspace subscriptions, no cron jobs)")
	} else {
		// Subscribe to Redis keyspace events
		tasks.SubscribeToRedisKeyspaceEvents()

		// Start cron jobs
		tasks.StartCronJobs()
	}

	// Run the server
	router := routers.Routes()

	appServer := fmt.Sprintf("%s:%s", conf.Host, conf.Port)
	logger.Infof("Server Running at :%v", appServer)

	logger.Fatalf("%v", router.Run(appServer))
}

// initBaaSRail builds the configured BaaS provider and registers it as the
// process-wide baas.Default(). The composition root is the only place that
// knows a concrete vendor; everything else depends on the baas interface.
func initBaaSRail() {
	viper.SetDefault("BAAS_PROVIDER", "safehaven")
	switch provider := viper.GetString("BAAS_PROVIDER"); provider {
	case "safehaven":
		shConf := config.BaaSConfig()
		switch shClient, err := mfb.NewClientFromCredentials(
			shConf.ClientID, shConf.PrivateKeyPEM, shConf.BaseURL, shConf.Audience, shConf.Issuer,
		); {
		case errors.Is(err, mfb.ErrNotConfigured):
			logger.Infof("BaaS rail (mfb) not configured; fiat payout routes disabled")
		case err != nil:
			logger.Fatalf("BaaS rail (mfb) init: %s", err)
		default:
			baas.SetDefault(mfb.NewAdapter(shClient, shConf.WebhookSecret))
			logger.Infof("BaaS rail ready (provider=mfb)")
		}
	case "korapay":
		kConf := config.BaaSConfig()
		if kConf.KorapaySecretKey == "" {
			logger.Infof("BaaS rail (korapay) not configured (KORAPAY_SECRET_KEY empty); fiat payout routes disabled")
			return
		}
		baas.SetDefault(korapay.NewAdapter(korapay.New(
			kConf.KorapaySecretKey, kConf.KorapayPublicKey, kConf.KorapayBaseURL,
			kConf.KorapayPayoutEmail, kConf.KorapayVBABankCode,
		)))
		logger.Infof("BaaS rail ready (provider=korapay)")
	case "fintava":
		fConf := config.BaaSConfig()
		if fConf.FintavaAPIKey == "" {
			logger.Infof("BaaS rail (fintava) not configured (FINTAVA_API_KEY empty); fiat payout routes disabled")
			return
		}
		baas.SetDefault(fintava.NewAdapter(fintava.New(
			fConf.FintavaAPIKey, fConf.FintavaWebhookSecret, fConf.FintavaBaseURL,
		)))
		logger.Infof("BaaS rail ready (provider=fintava)")
	default:
		logger.Fatalf("BaaS rail: unknown BAAS_PROVIDER %q", provider)
	}
}
