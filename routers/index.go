package routers

import (
	"net/http"

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

	// KYC routes
	v1.POST("kyc", ctrl.RequestIDVerification)
	v1.GET("kyc/:wallet_address", ctrl.GetIDVerificationStatus)
	v1.POST("kyc/webhook", ctrl.KYCWebhook)

}

func authRoutes(route *gin.Engine) {
	authCtrl := accounts.NewAuthController()
	var profileCtrl accounts.ProfileController

	v1 := route.Group("/v1/")
	v1.POST("auth/register", middleware.OnlyWebMiddleware, authCtrl.Register)
	v1.POST("auth/login", middleware.OnlyWebMiddleware, authCtrl.Login)
	v1.POST("auth/google", middleware.OnlyWebMiddleware, authCtrl.GoogleAuth)
	v1.POST("auth/confirm-account", middleware.OnlyWebMiddleware, authCtrl.ConfirmEmail)
	v1.POST("auth/resend-token", middleware.OnlyWebMiddleware, authCtrl.ResendVerificationToken)
	v1.POST("auth/refresh", middleware.OnlyWebMiddleware, authCtrl.RefreshJWT)
	v1.POST("auth/reset-password-token", middleware.OnlyWebMiddleware, authCtrl.ResetPasswordToken)
	v1.PATCH("auth/reset-password", middleware.OnlyWebMiddleware, authCtrl.ResetPassword)
	v1.PATCH("auth/change-password", middleware.JWTMiddleware, authCtrl.ChangePassword)

	v1.GET(
		"settings/provider",
		middleware.OnlyWebMiddleware,
		middleware.JWTMiddleware,
		middleware.OnlyProviderMiddleware,
		profileCtrl.GetProviderProfile,
	)
	v1.PATCH(
		"settings/provider",
		middleware.OnlyWebMiddleware,
		middleware.JWTMiddleware,
		middleware.OnlyProviderMiddleware,
		profileCtrl.UpdateProviderProfile,
	)

	v1.GET(
		"settings/sender",
		middleware.OnlyWebMiddleware,
		middleware.JWTMiddleware,
		middleware.OnlySenderMiddleware,
		profileCtrl.GetSenderProfile,
	)
	v1.PATCH(
		"settings/sender",
		middleware.OnlyWebMiddleware,
		middleware.JWTMiddleware,
		middleware.OnlySenderMiddleware,
		profileCtrl.UpdateSenderProfile,
	)
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
	v1.GET("stats", senderCtrl.Stats)

	// Tapp Merchant — mobile-first merchant API.
	me := v1.Group("me/")
	me.POST("bank-account", senderCtrl.SaveMerchantBankAccount)
	me.GET("bank-account", senderCtrl.GetMerchantBankAccount)
	me.POST("tap", senderCtrl.InitiateTapPayment)
	me.POST("tap-card", senderCtrl.InitiateTapCardPayment)
	me.GET("payments/stream", senderCtrl.StreamPayments)
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

// cardsRoutes wires the Tapp Card surface — currently a PoC subset
// (issue activation URLs + resolve the public /c/:token redirect).
// Post-PoC endpoints (link, resync, debit, …) land in the same
// /v1/cards group with proper auth wired in per docs/tapp-card-spec.md.
func cardsRoutes(route *gin.Engine) {
	cardsCtrl := cards.NewController()

	// Public: the tap-to-URL redirect. Anyone with a card hits this.
	route.GET("/c/:token", cardsCtrl.Resolve)

	// Cardholder: claim flow (JWT-authenticated via /v1/auth/google).
	cardholder := route.Group("/v1/cards/")
	cardholder.Use(middleware.JWTMiddleware)
	cardholder.POST("link/claim", cardsCtrl.Claim)

	// Admin: mint activation tokens for PoC hand-testing.
	admin := route.Group("/v1/cards/")
	admin.Use(cards.AdminTokenMiddleware)
	admin.POST("issue-batch", cardsCtrl.IssueBatch)
}
