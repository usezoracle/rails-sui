package routers

import (
	"strings"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/routers/middleware"
	"github.com/usezoracle/rails-sui/utils/logger"

	"github.com/gin-gonic/gin"
)

// trustedProxyCIDRs converts the TRUSTED_PROXIES setting into CIDRs gin accepts.
// gin requires IPs/CIDRs — passing "*" verbatim (the old behaviour) returns an
// error and crash-loops boot. "*" or empty → trust all (we sit behind a PaaS
// edge proxy and need X-Forwarded-For for real client IPs / rate-limiting);
// otherwise a comma-separated CIDR list to lock trust down.
func trustedProxyCIDRs(raw string) []string {
	if p := strings.TrimSpace(raw); p != "" && p != "*" {
		out := make([]string, 0, 4)
		for _, c := range strings.Split(p, ",") {
			if c = strings.TrimSpace(c); c != "" {
				out = append(out, c)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"0.0.0.0/0", "::/0"}
}

// Routes function registers all routes
func Routes() *gin.Engine {
	conf := config.ServerConfig()

	environment := conf.Debug
	if environment {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.RemoveExtraSlash = true
	proxies := trustedProxyCIDRs(conf.TrustedProxies)
	if err := router.SetTrustedProxies(proxies); err != nil {
		logger.Fatalf("failed to set trusted proxies %v: %v", proxies, err)
	}
	router.Use(gin.Logger())
	router.Use(gin.Recovery())
	router.Use(middleware.CORSMiddleware())

	RegisterRoutes(router) //routes register

	return router
}
