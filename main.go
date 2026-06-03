package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/routers"
	"github.com/usezoracle/rails-sui/services/baas/safehaven"
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

	// Initialize the Safe Haven (BaaS) fiat rail. Absent credentials are
	// non-fatal (Route A `mode=lp` and other flows don't need it); present but
	// invalid credentials fail fast so misconfiguration surfaces at boot.
	shConf := config.SafehavenConfig()
	switch shClient, err := safehaven.NewClientFromCredentials(
		shConf.ClientID, shConf.PrivateKeyPEM, shConf.BaseURL, shConf.Audience, shConf.Issuer,
	); {
	case errors.Is(err, safehaven.ErrNotConfigured):
		logger.Infof("Safe Haven BaaS rail not configured; fiat payout routes disabled")
	case err != nil:
		logger.Fatalf("Safe Haven init: %s", err)
	default:
		safehaven.SetDefault(shClient)
		logger.Infof("Safe Haven BaaS rail ready")
	}

	// Subscribe to Redis keyspace events
	tasks.SubscribeToRedisKeyspaceEvents()

	// Start cron jobs
	tasks.StartCronJobs()

	// Run the server
	router := routers.Routes()

	appServer := fmt.Sprintf("%s:%s", conf.Host, conf.Port)
	logger.Infof("Server Running at :%v", appServer)

	logger.Fatalf("%v", router.Run(appServer))
}
