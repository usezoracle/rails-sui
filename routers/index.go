package routers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/usezoracle/rails-sui/controllers"
	"github.com/usezoracle/rails-sui/controllers/accounts"
	"github.com/usezoracle/rails-sui/controllers/cards"
	"github.com/usezoracle/rails-sui/controllers/provider"
	"github.com/usezoracle/rails-sui/controllers/sender"
	"github.com/usezoracle/rails-sui/routers/middleware"
	u "github.com/usezoracle/rails-sui/utils"
)

// RegisterRoutes add all routing list here automatically get main router
func RegisterRoutes(route *gin.Engine) {
	route.NoRoute(func(ctx *gin.Context) {
		u.APIResponse(ctx, http.StatusNotFound, "error", "Route Not Found", nil)
	})
	route.GET("/health", func(ctx *gin.Context) { ctx.JSON(http.StatusOK, gin.H{"live": "ok"}) })

	// API docs — public. Swagger UI shell loads SWG assets from a CDN
	// and points at /openapi.yaml. The raw spec is hand-written at
	// docs/openapi.yaml (single source of truth, no codegen).
	docsCtrl := controllers.NewController()
	route.GET("/docs", docsCtrl.ServeSwaggerUI)
	route.GET("/openapi.yaml", docsCtrl.ServeOpenAPISpec)

	// Add all routes
	authRoutes(route)
	senderRoutes(route)
	providerRoutes(route)
	cardsRoutes(route)

	ctrl := controllers.NewController()

	v1 := route.Group("/v1/")

	v1.GET(
		"currencies",
		ctrl.GetFiatCurrencies,
	)
	v1.GET(
		"institutions/:currency_code",
		ctrl.GetInstitutionsByCurrency,
	)
	v1.GET("rates/:token/:amount/:fiat", ctrl.GetTokenRate)
	v1.GET("pubkey", ctrl.GetAggregatorPublicKey)
	v1.POST("verify-account", ctrl.VerifyAccount)
	v1.GET("orders/:id", ctrl.GetLockPaymentOrderStatus)
	// Public order-scoped SSE — customer checkout PWA subscribes after
	// submitting their on-chain payment to advance through the bridge
	// → settle pipeline in real time.
	v1.GET("orders/:id/stream", ctrl.StreamOrderStatus)
	// Customer "I sent it" ack — pre-emits payment.deposited so the
	// merchant UI advances without waiting for the Sui indexer.
	v1.POST("orders/:id/confirm", ctrl.ConfirmOrderPayment)
	v1.POST("gas-station/sponsor", middleware.JWTMiddleware, ctrl.SponsorTransaction)

	// KYC routes
	v1.POST("kyc", ctrl.RequestIDVerification)
	v1.GET("kyc/:wallet_address", ctrl.GetIDVerificationStatus)
	v1.POST("kyc/webhook", ctrl.KYCWebhook)

}

func authRoutes(route *gin.Engine) {
	authCtrl := accounts.NewAuthController()
	var profileCtrl accounts.ProfileController

	// OnlyWebMiddleware was retired — mobile clients are first-class now.
	// The previous gate required all callers to send `Client-Type: web`,
	// which mobile apps had to spoof for no security gain. Auth + scope
	// middleware handle access control; transport headers aren't auth.
	//
	// Rate limits are per-IP (fixed-window via Redis). Tunable
	// per-bucket; the limits below are tight enough to stop password
	// spraying but generous enough that an honest user fat-fingering
	// won't get locked out.
	v1 := route.Group("/v1/")
	v1.POST("auth/register", middleware.RateLimit("auth.register", 5, time.Hour, nil), authCtrl.Register)
	v1.POST("auth/login", middleware.RateLimit("auth.login", 10, 15*time.Minute, nil), authCtrl.Login)
	v1.POST("auth/google", middleware.RateLimit("auth.google", 20, 15*time.Minute, nil), authCtrl.GoogleAuth)
	v1.POST("auth/confirm-account", middleware.RateLimit("auth.confirm", 10, 10*time.Minute, nil), authCtrl.ConfirmEmail)
	v1.POST("auth/resend-token", middleware.RateLimit("auth.resend", 3, 10*time.Minute, nil), authCtrl.ResendVerificationToken)
	v1.POST("auth/refresh", middleware.RateLimit("auth.refresh", 60, time.Minute, nil), authCtrl.RefreshJWT)
	// Logout is intentionally unauthenticated. It revokes the refresh
	// token in the request body (idempotent), so a client whose access
	// JWT is already expired can still sign out cleanly. Returning 401
	// here would force the client to give up + clear local anyway —
	// just do the revocation.
	v1.POST("auth/logout", middleware.RateLimit("auth.logout", 30, time.Minute, nil), authCtrl.Logout)
	v1.POST("auth/reset-password-token", middleware.RateLimit("auth.reset.request", 3, time.Hour, nil), authCtrl.ResetPasswordToken)
	v1.PATCH("auth/reset-password", middleware.RateLimit("auth.reset.submit", 10, time.Hour, nil), authCtrl.ResetPassword)
	v1.PATCH("auth/change-password", middleware.JWTMiddleware, authCtrl.ChangePassword)

	v1.GET(
		"settings/provider",
		middleware.JWTMiddleware,
		middleware.OnlyProviderMiddleware,
		profileCtrl.GetProviderProfile,
	)
	v1.PATCH(
		"settings/provider",
		middleware.JWTMiddleware,
		middleware.OnlyProviderMiddleware,
		profileCtrl.UpdateProviderProfile,
	)

	v1.GET(
		"settings/sender",
		middleware.JWTMiddleware,
		middleware.OnlySenderMiddleware,
		profileCtrl.GetSenderProfile,
	)
	v1.PATCH(
		"settings/sender",
		middleware.JWTMiddleware,
		middleware.OnlySenderMiddleware,
		profileCtrl.UpdateSenderProfile,
	)

	v1.GET("me", middleware.JWTMiddleware, authCtrl.Me)
	v1.PATCH("me", middleware.JWTMiddleware, authCtrl.UpdateMe)
}

func senderRoutes(route *gin.Engine) {
	senderCtrl := sender.NewSenderController()

	v1 := route.Group("/v1/sender/")
	v1.Use(middleware.DynamicAuthMiddleware)
	v1.Use(middleware.OnlySenderMiddleware)

	v1.POST("orders", senderCtrl.InitiatePaymentOrder)
	v1.POST("orders/route-a", senderCtrl.InitiateRouteAOrder)
	v1.GET("orders/:id", senderCtrl.GetPaymentOrderByID)
	v1.GET("orders", senderCtrl.GetPaymentOrders)
	v1.POST("orders/:id/cancel", senderCtrl.CancelOrder)
	v1.GET("stats", senderCtrl.Stats)

	// Tapp Merchant — mobile-first merchant API.
	cardsCtrl := cards.NewController()
	me := v1.Group("me/")
	me.POST("bank-account", senderCtrl.SaveMerchantBankAccount)
	me.GET("bank-account", senderCtrl.GetMerchantBankAccount)
	me.POST("tap", senderCtrl.InitiateTapPayment)
	me.GET("payments/stream", senderCtrl.StreamPayments)

	// Tap Card vertical (replaces the 501 stub on /tap-card).
	me.GET("tap-card/nonce", cardsCtrl.TapCardNonce)
	me.POST("tap-card", cardsCtrl.TapCardDebit)
	me.POST("tap-card/:order_id/token-ack", cardsCtrl.TapCardTokenAck)
	me.GET("tap-card/step-up", cardsCtrl.TapCardStepUpPoll)
}

func providerRoutes(route *gin.Engine) {
	providerCtrl := provider.NewProviderController()

	v1 := route.Group("/v1/provider/")
	v1.Use(middleware.DynamicAuthMiddleware)
	v1.Use(middleware.OnlyProviderMiddleware)

	v1.GET("orders", providerCtrl.GetLockPaymentOrders)
	v1.POST("orders/:id/accept", providerCtrl.AcceptOrder)
	v1.POST("orders/:id/decline", providerCtrl.DeclineOrder)
	v1.POST("orders/:id/fulfill", providerCtrl.FulfillOrder)
	v1.POST("orders/:id/cancel", providerCtrl.CancelOrder)
	v1.GET("rates/:token/:fiat", providerCtrl.GetMarketRate)
	v1.GET("stats", providerCtrl.Stats)
	v1.GET("node-info", providerCtrl.NodeInfo)
}

// cardsRoutes wires the Tapp Card surface — public redirect, full
// cardholder flow (link, top-up, revoke, resync), and admin recovery.
// The merchant-facing debit endpoints live under /v1/sender/me/tap-card
// and are wired in senderRoutes (different middleware stack).
func cardsRoutes(route *gin.Engine) {
	cardsCtrl := cards.NewController()

	// Public: the tap-to-URL redirect. Anyone with a card hits this.
	route.GET("/c/:token", cardsCtrl.Resolve)

	// Cardholder (PWA): JWT-authenticated via /v1/auth/google.
	cardholder := route.Group("/v1/cards/")
	cardholder.Use(middleware.JWTMiddleware)
	cardholder.POST("link/claim", cardsCtrl.Claim)
	cardholder.POST("link/complete", cardsCtrl.LinkComplete)
	cardholder.GET("me", cardsCtrl.Me)
	cardholder.POST("top-up", cardsCtrl.TopUp)
	cardholder.POST("revoke", cardsCtrl.Revoke)
	cardholder.POST("me/resync", cardsCtrl.Resync)
	cardholder.POST("me/resync/complete", cardsCtrl.ResyncComplete)
	cardholder.POST("me/step-up/parse", cardsCtrl.StepUpParse)
	cardholder.POST("me/step-up/grant", cardsCtrl.StepUpGrant)

	// Admin: shared-secret-gated.
	admin := route.Group("/v1/cards/")
	admin.Use(cards.AdminTokenMiddleware)
	admin.POST("issue-batch", cardsCtrl.IssueBatch)

	// Admin recovery (iOS no-Web-NFC escape hatch).
	adminCards := route.Group("/v1/admin/cards/")
	adminCards.Use(cards.AdminTokenMiddleware)
	adminCards.POST(":id/recovery", cardsCtrl.AdminRecovery)
}
