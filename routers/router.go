package routers

import (
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/routers/middleware"
	"github.com/usezoracle/rails-sui/utils/logger"

	"github.com/gin-gonic/gin"
)

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
	err := router.SetTrustedProxies([]string{conf.AllowedHosts})
	if err != nil {
		logger.Fatalf("failed to set trusted proxies")
	}
	router.Use(gin.Logger())
	router.Use(gin.Recovery())
	router.Use(middleware.CORSMiddleware())

	RegisterRoutes(router) //routes register

	return router
}
