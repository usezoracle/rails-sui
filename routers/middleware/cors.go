package middleware

import (
	"github.com/gin-gonic/gin"
)

// CORSMiddleware is a middleware that adds CORS headers to response
func CORSMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		origin := ctx.Request.Header.Get("Origin")
		if origin != "" {
			ctx.Writer.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			ctx.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		}
		ctx.Writer.Header().Set("Access-Control-Max-Age", "86400")
		ctx.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, PATCH, DELETE")
		ctx.Writer.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type, api_key, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, Client-Type, X-Admin-Token, ngrok-skip-browser-warning, Idempotency-Key")
		ctx.Writer.Header().Set("Access-Control-Expose-Headers", "Content-Length")
		ctx.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		ctx.Writer.Header().Set("Cache-Control", "no-cache")

		if ctx.Request.Method == "OPTIONS" {
			ctx.AbortWithStatus(200)
		} else {
			ctx.Next()
		}
	}
}
