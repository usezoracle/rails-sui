package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	suisigner "github.com/block-vision/sui-go-sdk/signer"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	fastshot "github.com/opus-domini/fast-shot"
	"github.com/shopspring/decimal"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/identityverificationrequest"
	"github.com/usezoracle/rails-sui/ent/institution"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/ent/token"
	svc "github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/services/lifi"
	orderSvc "github.com/usezoracle/rails-sui/services/order"
	"github.com/usezoracle/rails-sui/services/settlement"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"

	"github.com/gin-gonic/gin"
)

var cryptoConf = config.CryptoConfig()
var serverConf = config.ServerConfig()
var identityConf = config.IdentityConfig()
var orderConf = config.OrderConfig()

// Controller is the default controller for other endpoints
type Controller struct {
	orderService          types.OrderService
	priorityQueueService  *svc.PriorityQueueService
	receiveAddressService *svc.ReceiveAddressService
}

// NewController creates a new instance of AuthController with injected services
func NewController() *Controller {
	return &Controller{
		orderService:          orderSvc.NewOrderSui(),
		priorityQueueService:  svc.NewPriorityQueueService(),
		receiveAddressService: svc.NewReceiveAddressService(),
	}
}

// GetFiatCurrencies controller fetches the supported fiat currencies
func (ctrl *Controller) GetFiatCurrencies(ctx *gin.Context) {
	// fetch stored fiat currencies.
	fiatcurrencies, err := storage.Client.FiatCurrency.
		Query().
		Where(fiatcurrency.IsEnabledEQ(true)).
		All(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to fetch FiatCurrencies", err.Error())
		return
	}

	currencies := make([]types.SupportedCurrencies, 0, len(fiatcurrencies))
	for _, currency := range fiatcurrencies {
		currencies = append(currencies, types.SupportedCurrencies{
			Code:       currency.Code,
			Name:       currency.Name,
			ShortName:  currency.ShortName,
			Decimals:   int8(currency.Decimals),
			Symbol:     currency.Symbol,
			MarketRate: currency.MarketRate,
		})
	}

	u.APIResponse(ctx, http.StatusOK, "success", "OK", currencies)
}

// GetInstitutionsByCurrency controller fetches the supported institutions for a given currency
func (ctrl *Controller) GetInstitutionsByCurrency(ctx *gin.Context) {
	// Get currency code from the URL
	currencyCode := ctx.Param("currency_code")

	// Query institutions from the local database (seeded via SQL migrations)
	institutions, err := storage.Client.Institution.
		Query().
		Where(institution.HasFiatCurrencyWith(
			fiatcurrency.CodeEQ(currencyCode),
		)).
		All(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to fetch institutions", nil)
		return
	}

	response := make([]types.SupportedInstitutions, 0, len(institutions))
	for _, institution := range institutions {
		response = append(response, types.SupportedInstitutions{
			Code: institution.Code,
			Name: institution.Name,
			Type: institution.Type,
		})
	}

	u.APIResponse(ctx, http.StatusOK, "success", "OK", response)
}

// GetTokenRate controller fetches the current rate of the cryptocurrency token against the fiat currency
func (ctrl *Controller) GetTokenRate(ctx *gin.Context) {
	// Parse path parameters
	token, err := storage.Client.Token.
		Query().
		Where(
			token.SymbolEQ(strings.ToUpper(ctx.Param("token"))),
			token.IsEnabledEQ(true),
		).
		WithNetwork().
		First(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch token rate", nil)
		return
	}

	if token == nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Token is not supported", nil)
		return
	}

	currency, err := storage.Client.FiatCurrency.
		Query().
		Where(
			fiatcurrency.IsEnabledEQ(true),
			fiatcurrency.CodeEQ(strings.ToUpper(ctx.Param("fiat"))),
		).
		Only(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Fiat currency is not supported", nil)
		return
	}

	tokenAmount, err := decimal.NewFromString(ctx.Param("amount"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid amount", nil)
		return
	}

	rateResponse := currency.MarketRate
	routeAQuoted := false
	if strings.EqualFold(currency.Code, "NGN") &&
		token.Edges.Network != nil &&
		strings.HasPrefix(token.Edges.Network.Identifier, "sui-") {
		if len(orderConf.SuiAggregatorPrivateKey) != 32 {
			u.APIResponse(ctx, http.StatusServiceUnavailable, "error",
				"Route A rate requires SUI_AGGREGATOR_PRIVATE_KEY to be configured", nil)
			return
		}
		settlementTTL := time.Duration(orderConf.SettlementPubkeyTTLSeconds) * time.Second
		settlementClient := settlement.New(orderConf.SettlementAPIURL, settlementTTL)
		lifiClient := lifi.New(orderConf.LiFiAPIKey)
		aggSigner := suisigner.NewSigner(orderConf.SuiAggregatorPrivateKey)
		composite, _, qerr := svc.QuoteSuiTokenAmountForFiat(
			ctx,
			lifiClient,
			settlementClient,
			orderConf,
			aggSigner.Address,
			tokenAmount,
			currency.MarketRate,
			token.ContractAddress,
			int32(token.Decimals),
		)
		if qerr != nil {
			logger.Errorf("GetTokenRate.route_a_quote token=%s fiat=%s: %v", token.Symbol, tokenAmount, qerr)
			u.APIResponse(ctx, http.StatusBadGateway, "error",
				"Couldn't quote Route A settlement rate", nil)
			return
		}
		rateResponse = composite.Rate
		routeAQuoted = true
	}

	// get providerID from query params
	providerID := ctx.Query("provider_id")
	if routeAQuoted {
		u.APIResponse(ctx, http.StatusOK, "success", "Rate fetched successfully", rateResponse)
		return
	}
	if providerID != "" {
		// get the provider from the bucket
		provider, err := storage.Client.ProviderProfile.
			Query().
			Where(providerprofile.IDEQ(providerID)).
			Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				u.APIResponse(ctx, http.StatusBadRequest, "error", "Provider not found", nil)
				return
			} else {
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch provider profile", nil)
				return
			}
		}

		rateResponse, err = ctrl.priorityQueueService.GetProviderRate(ctx, provider, token.Symbol)
		if err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch provider rate", nil)
			return
		}

	} else {
		// Get redis keys for provision buckets
		keys, _, err := storage.RedisClient.Scan(ctx, uint64(0), "bucket_"+currency.Code+"_*_*", 100).Result()
		if err != nil {
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch rates", nil)
			return
		}

		highestMaxAmount := decimal.NewFromInt(0)

		// Scan through the buckets to find a matching rate
		for _, key := range keys {
			bucketData := strings.Split(key, "_")
			minAmount, _ := decimal.NewFromString(bucketData[2])
			maxAmount, _ := decimal.NewFromString(bucketData[3])

			for index := 0; ; index++ {
				// Get the topmost provider in the priority queue of the bucket
				providerData, err := storage.RedisClient.LIndex(ctx, key, int64(index)).Result()
				if err != nil {
					break
				}

				if strings.Split(providerData, ":")[1] == token.Symbol {
					// Get fiat equivalent of the token amount
					rate, _ := decimal.NewFromString(strings.Split(providerData, ":")[2])
					fiatAmount := tokenAmount.Mul(rate)

					// Check if fiat amount is within the bucket range and set the rate
					if fiatAmount.GreaterThanOrEqual(minAmount) && fiatAmount.LessThanOrEqual(maxAmount) {
						rateResponse = rate
						break
					} else if maxAmount.GreaterThan(highestMaxAmount) {
						// Get the highest max amount
						highestMaxAmount = maxAmount
						rateResponse = rate
					}
				}
			}
		}
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Rate fetched successfully", rateResponse)
}

// GetAggregatorPublicKey controller expose Aggregator Public Key
func (ctrl *Controller) GetAggregatorPublicKey(ctx *gin.Context) {
	u.APIResponse(ctx, http.StatusOK, "success", "OK", cryptoConf.AggregatorPublicKey)
}

type sponsorTransactionRequest struct {
	TxBytes string `json:"txBytes" binding:"required"`
	Sender  string `json:"sender" binding:"required"`
}

type sponsorTransactionResponse struct {
	SponsoredTxBytes string `json:"sponsoredTxBytes"`
	SponsorSignature string `json:"sponsorSignature"`
}

// SponsorTransaction handles POST /v1/gas-station/sponsor.
func (ctrl *Controller) SponsorTransaction(ctx *gin.Context) {
	var req sponsorTransactionRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	sponsoredTxBytes, sponsorSignature, err := ctrl.orderService.SponsorTransaction(ctx.Request.Context(), req.TxBytes, req.Sender)
	if err != nil {
		logger.Errorf("SponsorTransaction failed: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			fmt.Sprintf("Gas station failed: %v", err), nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Transaction sponsored", sponsorTransactionResponse{
		SponsoredTxBytes: sponsoredTxBytes,
		SponsorSignature: sponsorSignature,
	})
}

// VerifyAccount controller verifies an account of a given institution
func (ctrl *Controller) VerifyAccount(ctx *gin.Context) {
	var payload types.VerifyAccountRequest

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	if payload.AccountIdentifier == "" && payload.AccountIdentifierSnake != "" {
		payload.AccountIdentifier = payload.AccountIdentifierSnake
	}

	if payload.AccountIdentifier == "" {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", []types.ErrorData{{
			Field:   "AccountIdentifier",
			Message: "AccountIdentifier (accountIdentifier or account_identifier) is required",
		}})
		return
	}

	// Try live-verifying with settlement API if configured.
	// SETTLEMENT_API_URL is the versioned root (e.g. "https://api.paycrest.io/v1")
	// per services/settlement convention, but verify-account lives at
	// /v2/verify-account — so we strip the trailing /v1 before composing.
	// Stay aligned with services/settlement/client.go FetchRate's URL build.
	settlementURL := strings.TrimSuffix(serverConf.SettlementAPIURL, "/v1")
	if settlementURL != "" {
		pcPayload := map[string]string{
			"institution":       payload.Institution,
			"accountIdentifier": payload.AccountIdentifier,
		}
		res, err := fastshot.NewClient(settlementURL).
			Config().SetTimeout(15*time.Second).
			Header().Add("Content-Type", "application/json").
			Build().POST("/v2/verify-account").
			Body().AsJSON(pcPayload).
			Send()
		if err == nil {
			defer res.RawResponse.Body.Close()
			if res.StatusCode() == http.StatusOK {
				var pcResp struct {
					Status  string `json:"status"`
					Message string `json:"message"`
					Data    string `json:"data"`
				}
				if decodeErr := json.NewDecoder(res.RawResponse.Body).Decode(&pcResp); decodeErr == nil && pcResp.Status == "success" {
					u.APIResponse(ctx, http.StatusOK, "success", "Account name was fetched successfully", pcResp.Data)
					return
				}
				logger.Warnf("settlement verify-account returned 200 but unexpected body — falling back")
			} else {
				logger.Warnf("settlement verify-account http %d — falling back", res.StatusCode())
			}
		} else {
			logger.Warnf("settlement verify-account transport error: %v — falling back", err)
		}
	}

	// Fallback to local database and provider profiles
	institution, err := storage.Client.Institution.
		Query().
		Where(institution.CodeEQ(payload.Institution)).
		WithFiatCurrency().
		Only(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", []types.ErrorData{{
			Field:   "Institution",
			Message: "Institution is not supported",
		}})
		return
	}

	// TODO: Remove this after testing non-NGN institutions
	if institution.Edges.FiatCurrency.Code != "NGN" {
		u.APIResponse(ctx, http.StatusOK, "success", "Account name was fetched successfully", "OK")
		return
	}

	providers, err := storage.Client.ProviderProfile.
		Query().
		Where(
			providerprofile.HasCurrencyWith(
				fiatcurrency.CodeEQ(institution.Edges.FiatCurrency.Code),
			),
			providerprofile.HostIdentifierNotNil(),
			providerprofile.IsActiveEQ(true),
			providerprofile.IsAvailableEQ(true),
		).
		All(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to verify account", err.Error())
		return
	}

	var res fastshot.Response
	var data map[string]interface{}
	for _, provider := range providers {
		res, err = fastshot.NewClient(provider.HostIdentifier).
			Config().SetTimeout(30 * time.Second).
			Build().POST("/verify_account").
			Body().AsJSON(payload).
			Send()
		if err != nil {
			continue
		}

		data, err = u.ParseJSONResponse(res.RawResponse)
		if err != nil {
			continue
		}
	}

	if err != nil {
		logger.Errorf("error: %v %v", err, data)
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "Failed to verify account", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Account name was fetched successfully", data["data"].(string))
}

// GetLockPaymentOrderStatus controller fetches a payment order status by ID
func (ctrl *Controller) GetLockPaymentOrderStatus(ctx *gin.Context) {
	// Get order ID from the URL
	orderID := ctx.Param("id")

	// Define the combined response type that satisfies both types.LockPaymentOrderStatusResponse
	// and the PWA's OrderDetails requirements.
	type CombinedOrderResponse struct {
		OrderID       string                             `json:"orderId"`
		Amount        decimal.Decimal                    `json:"amount"`
		Token         string                             `json:"token"`
		Network       string                             `json:"network"`
		SettlePercent decimal.Decimal                    `json:"settlePercent"`
		Status        lockpaymentorder.Status            `json:"status"`
		TxHash        string                             `json:"txHash"`
		Settlements   []types.LockPaymentOrderSplitOrder `json:"settlements"`
		TxReceipts    []types.LockPaymentOrderTxReceipt  `json:"txReceipts"`
		UpdatedAt     time.Time                          `json:"updatedAt"`

		// PWA OrderDetails fields
		ID              string  `json:"id"`
		MerchantName    string  `json:"merchant_name"`
		MerchantLogoURL string  `json:"merchant_logo_url,omitempty"`
		AmountSubunit   int64   `json:"amount_subunit"`
		NgnRate         float64 `json:"ngn_rate"`
		Reference       string  `json:"reference"`
		ExpiresAt       int64   `json:"expires_at"`
		StepUpRequired  bool    `json:"step_up_required"`

		// On-chain deposit target. The customer's wallet sends
		// `amount_subunit` of `coin_type` to this Sui address; the
		// indexer picks the deposit up and advances the order state.
		ReceiveAddress string `json:"receive_address,omitempty"`
		CoinType       string `json:"coin_type,omitempty"`
	}

	// First, try to fetch related lock payment orders from the database
	orders, err := storage.Client.LockPaymentOrder.
		Query().
		Where(
			lockpaymentorder.GatewayIDEQ(orderID),
		).
		WithToken(func(tq *ent.TokenQuery) {
			tq.WithNetwork()
		}).
		WithTransactions().
		All(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch order status", nil)
		return
	}

	// If LockPaymentOrder records exist, build the response from them (and fetch PaymentOrder for PWA details if possible)
	if len(orders) > 0 {
		var settlements []types.LockPaymentOrderSplitOrder
		var receipts []types.LockPaymentOrderTxReceipt
		var settlePercent decimal.Decimal
		var totalAmount decimal.Decimal

		for _, order := range orders {
			for _, transaction := range order.Edges.Transactions {
				if u.ContainsString([]string{"order_settled", "order_created", "order_refunded"}, transaction.Status.String()) {
					var status lockpaymentorder.Status
					if transaction.Status.String() == "order_created" {
						status = lockpaymentorder.StatusPending
					} else {
						status = lockpaymentorder.Status(strings.TrimPrefix(transaction.Status.String(), "order_"))
					}
					receipts = append(receipts, types.LockPaymentOrderTxReceipt{
						Status:    status,
						TxHash:    transaction.TxHash,
						Timestamp: transaction.CreatedAt,
					})
				}
			}

			settlements = append(settlements, types.LockPaymentOrderSplitOrder{
				SplitOrderID: order.ID,
				Amount:       order.Amount,
				Rate:         order.Rate,
				OrderPercent: order.OrderPercent,
			})

			settlePercent = settlePercent.Add(order.OrderPercent)
			totalAmount = totalAmount.Add(order.Amount)
		}

		// Sort receipts by latest timestamp
		slices.SortStableFunc(receipts, func(a, b types.LockPaymentOrderTxReceipt) int {
			return b.Timestamp.Compare(a.Timestamp)
		})

		status := orders[0].Status
		if status == lockpaymentorder.StatusCancelled {
			status = lockpaymentorder.StatusProcessing
		}

		var merchantName string
		var reference string
		var expiresAt int64
		var ngnRate float64
		var receiveAddress string
		var coinType string

		poID, err := uuid.Parse(orders[0].GatewayID)
		if err == nil {
			po, err := storage.Client.PaymentOrder.
				Query().
				Where(paymentorder.IDEQ(poID)).
				WithRecipient().
				WithSuiReceiveAddress().
				Only(ctx)
			if err == nil {
				if po.Edges.Recipient != nil {
					merchantName = po.Edges.Recipient.AccountName
				}
				reference = po.Reference
				if po.Edges.SuiReceiveAddress != nil {
					expiresAt = po.Edges.SuiReceiveAddress.ValidUntil.UnixMilli()
					receiveAddress = po.Edges.SuiReceiveAddress.Address
					coinType = po.Edges.SuiReceiveAddress.CoinType
				} else {
					expiresAt = po.CreatedAt.Add(1 * time.Hour).UnixMilli()
				}
				ngnRate, _ = po.Rate.Float64()
			}
		}
		if merchantName == "" {
			merchantName = orders[0].AccountName
		}
		if reference == "" {
			reference = orders[0].Memo
		}
		if expiresAt == 0 {
			expiresAt = orders[0].CreatedAt.Add(1 * time.Hour).UnixMilli()
		}
		if ngnRate == 0 {
			ngnRate, _ = orders[0].Rate.Float64()
		}

		txHash := ""
		if len(receipts) > 0 {
			txHash = receipts[0].TxHash
		}

		response := &CombinedOrderResponse{
			OrderID:       orders[0].GatewayID,
			Amount:        totalAmount,
			Token:         orders[0].Edges.Token.Symbol,
			Network:       orders[0].Edges.Token.Edges.Network.Identifier,
			SettlePercent: settlePercent,
			Status:        status,
			TxHash:        txHash,
			Settlements:   settlements,
			TxReceipts:    receipts,
			UpdatedAt:     orders[0].UpdatedAt,

			ID:             orders[0].GatewayID,
			MerchantName:   merchantName,
			AmountSubunit:  u.ToSubunit(totalAmount, orders[0].Edges.Token.Decimals).Int64(),
			NgnRate:        ngnRate,
			Reference:      reference,
			ExpiresAt:      expiresAt,
			StepUpRequired: false,
			ReceiveAddress: receiveAddress,
			CoinType:       coinType,
		}

		u.APIResponse(ctx, http.StatusOK, "success", "Order status fetched successfully", response)
		return
	}

	// Fallback: If no LockPaymentOrder exists, check if a PaymentOrder exists with the matching UUID
	poID, err := uuid.Parse(orderID)
	if err != nil {
		// Not a UUID, return 404
		u.APIResponse(ctx, http.StatusNotFound, "error", "Order not found", nil)
		return
	}

	// Retrieve from payment_orders
	po, err := storage.Client.PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(poID)).
		WithToken(func(tq *ent.TokenQuery) {
			tq.WithNetwork()
		}).
		WithRecipient().
		WithSuiReceiveAddress().
		WithTransactions().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "Order not found", nil)
		} else {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch order details", nil)
		}
		return
	}

	// Map PaymentOrder status to lockpaymentorder.Status
	status := lockpaymentorder.StatusPending
	switch po.Status {
	case paymentorder.StatusInitiated, paymentorder.StatusPending:
		status = lockpaymentorder.StatusPending
	case paymentorder.StatusExpired, paymentorder.StatusCancelled:
		status = lockpaymentorder.StatusCancelled
	case paymentorder.StatusSettled:
		status = lockpaymentorder.StatusSettled
	case paymentorder.StatusRefunded:
		status = lockpaymentorder.StatusRefunded
	}

	merchantName := ""
	if po.Edges.Recipient != nil {
		merchantName = po.Edges.Recipient.AccountName
	}

	expiresAt := po.CreatedAt.Add(1 * time.Hour).UnixMilli()
	var receiveAddress string
	var coinType string
	if po.Edges.SuiReceiveAddress != nil {
		expiresAt = po.Edges.SuiReceiveAddress.ValidUntil.UnixMilli()
		receiveAddress = po.Edges.SuiReceiveAddress.Address
		coinType = po.Edges.SuiReceiveAddress.CoinType
	}

	ngnRate, _ := po.Rate.Float64()

	var receipts []types.LockPaymentOrderTxReceipt
	for _, transaction := range po.Edges.Transactions {
		if u.ContainsString([]string{"order_settled", "order_created", "order_refunded"}, transaction.Status.String()) {
			var txStatus lockpaymentorder.Status
			if transaction.Status.String() == "order_created" {
				txStatus = lockpaymentorder.StatusPending
			} else {
				txStatus = lockpaymentorder.Status(strings.TrimPrefix(transaction.Status.String(), "order_"))
			}
			receipts = append(receipts, types.LockPaymentOrderTxReceipt{
				Status:    txStatus,
				TxHash:    transaction.TxHash,
				Timestamp: transaction.CreatedAt,
			})
		}
	}

	txHash := ""
	if len(receipts) > 0 {
		txHash = receipts[0].TxHash
	}

	response := &CombinedOrderResponse{
		OrderID:       po.ID.String(),
		Amount:        po.Amount,
		Token:         po.Edges.Token.Symbol,
		Network:       po.Edges.Token.Edges.Network.Identifier,
		SettlePercent: po.PercentSettled,
		Status:        status,
		TxHash:        txHash,
		Settlements:   []types.LockPaymentOrderSplitOrder{},
		TxReceipts:    receipts,
		UpdatedAt:     po.UpdatedAt,

		ID:             po.ID.String(),
		MerchantName:   merchantName,
		AmountSubunit:  u.ToSubunit(po.Amount, po.Edges.Token.Decimals).Int64(),
		NgnRate:        ngnRate,
		Reference:      po.Reference,
		ExpiresAt:      expiresAt,
		StepUpRequired: false,
		ReceiveAddress: receiveAddress,
		CoinType:       coinType,
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Order status fetched successfully", response)
}

// RequestIDVerification controller requests identity verification details
func (ctrl *Controller) RequestIDVerification(ctx *gin.Context) {
	var payload types.NewIDVerificationRequest

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	// Validate wallet signature
	signature, err := hex.DecodeString(payload.Signature)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid signature", "Signature is not in the correct format")
		return
	}

	if len(signature) != 65 {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid signature", "Signature length is not correct")
		return
	}

	if signature[64] != 27 && signature[64] != 28 {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid signature", "Invalid recovery ID")
		return
	}
	signature[64] -= 27

	// Verify wallet signature
	message := fmt.Sprintf("I accept the KYC Policy and hereby request an identity verification check for %s with nonce %s", payload.WalletAddress, payload.Nonce)

	prefix := "\x19Ethereum Signed Message:\n" + fmt.Sprint(len(message))
	hash := crypto.Keccak256Hash([]byte(prefix + message))

	sigPublicKeyECDSA, err := crypto.SigToPub(hash.Bytes(), signature)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid signature", nil)
		return
	}

	recoveredAddress := crypto.PubkeyToAddress(*sigPublicKeyECDSA)
	if !strings.EqualFold(recoveredAddress.Hex(), payload.WalletAddress) {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid signature", nil)
		return
	}

	// Check if there is an existing verification request
	ivr, err := storage.Client.IdentityVerificationRequest.
		Query().
		Where(
			identityverificationrequest.WalletAddressEQ(payload.WalletAddress),
		).
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to request identity verification", nil)
			return
		}
	}

	timestamp := time.Now()

	if ivr != nil {
		if ivr.WalletSignature == payload.Signature {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "Signature already used for identity verification", nil)
			return
		}

		expiryPeriod := 15 * time.Minute

		if ivr.Status == identityverificationrequest.StatusFailed || (ivr.Status == identityverificationrequest.StatusPending && ivr.LastURLCreatedAt.Add(expiryPeriod).Before(timestamp)) {
			// Request is expired, delete db entry
			_, err := storage.Client.IdentityVerificationRequest.
				Delete().
				Where(
					identityverificationrequest.WalletAddressEQ(payload.WalletAddress),
				).
				Exec(ctx)
			if err != nil {
				logger.Errorf("error: %v", err)
				u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to request identity verification", nil)
				return
			}

		} else if ivr.Status == identityverificationrequest.StatusPending && (ivr.LastURLCreatedAt.Add(expiryPeriod).Equal(timestamp) || ivr.LastURLCreatedAt.Add(expiryPeriod).After(timestamp)) {
			// Update the wallet signature in db
			_, err = ivr.
				Update().
				SetWalletSignature(payload.Signature).
				Save(ctx)
			if err != nil {
				logger.Errorf("error: %v", err)
				u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to request identity verification", nil)
				return
			}

			u.APIResponse(ctx, http.StatusOK, "success", "This account has a pending identity verification request", &types.NewIDVerificationResponse{
				URL:       ivr.VerificationURL,
				ExpiresAt: ivr.LastURLCreatedAt,
			})
			return
		}

		if ivr.Status == "success" {
			u.APIResponse(ctx, http.StatusBadRequest, "success", "Failed to request identity verification", "This account has already been successfully verified")
			return
		}
	}

	// Generate Smile Identity signature
	h := hmac.New(sha256.New, []byte(identityConf.SmileIdentityApiKey))
	h.Write([]byte(timestamp.Format(time.RFC3339Nano)))
	h.Write([]byte(identityConf.SmileIdentityPartnerId))
	h.Write([]byte("sid_request"))

	// Initiate KYC verification
	res, err := fastshot.NewClient(identityConf.SmileIdentityBaseUrl).
		Config().SetTimeout(30 * time.Second).
		Build().POST("/v1/smile_links").
		Body().AsJSON(map[string]interface{}{
		"partner_id":   identityConf.SmileIdentityPartnerId,
		"signature":    base64.StdEncoding.EncodeToString(h.Sum(nil)),
		"timestamp":    timestamp,
		"name":         "Aggregator KYC",
		"company_name": "Rails",
		"id_types": []map[string]interface{}{
			// Nigeria
			{
				"country":             "NG",
				"id_type":             "PASSPORT",
				"verification_method": "doc_verification",
			},
			{
				"country":             "NG",
				"id_type":             "DRIVERS_LICENSE",
				"verification_method": "doc_verification",
			},
			{
				"country":             "NG",
				"id_type":             "V_NIN",
				"verification_method": "biometric_kyc",
			},
			{
				"country":             "NG",
				"id_type":             "VOTER_ID",
				"verification_method": "biometric_kyc",
			},
			{
				"country":             "NG",
				"id_type":             "RESIDENT_ID",
				"verification_method": "doc_verification",
			},
			{
				"country":             "NG",
				"id_type":             "IDENTITY_CARD",
				"verification_method": "doc_verification",
			},

			// Kenya
			{
				"country":             "KE",
				"id_type":             "PASSPORT",
				"verification_method": "enhanced_document_verification",
			},
			{
				"country":             "KE",
				"id_type":             "DRIVERS_LICENSE",
				"verification_method": "doc_verification",
			},
			{
				"country":             "KE",
				"id_type":             "ALIEN_CARD",
				"verification_method": "biometric_kyc",
			},
			{
				"country":             "KE",
				"id_type":             "NATIONAL_ID",
				"verification_method": "biometric_kyc",
			},

			// Ghana
			// {
			// 	"country":             "GH",
			// 	"id_type":             "PASSPORT",
			// 	"verification_method": "enhanced_document_verification",
			// },
			// {
			// 	"country":             "GH",
			// 	"id_type":             "VOTER_ID",
			// 	"verification_method": "enhanced_document_verification",
			// },
			// {
			// 	"country":             "GH",
			// 	"id_type":             "NEW_VOTER_ID",
			// 	"verification_method": "biometric_kyc",
			// },
			// {
			// 	"country":             "GH",
			// 	"id_type":             "DRIVERS_LICENSE",
			// 	"verification_method": "doc_verification",
			// },
			// {
			// 	"country":             "GH",
			// 	"id_type":             "SSNIT",
			// 	"verification_method": "biometric_kyc",
			// },

			// South Africa
			// {
			// 	"country":             "ZA",
			// 	"id_type":             "PASSPORT",
			// 	"verification_method": "doc_verification",
			// },
			// {
			// 	"country":             "ZA",
			// 	"id_type":             "DRIVERS_LICENSE",
			// 	"verification_method": "doc_verification",
			// },
			// {
			// 	"country":             "ZA",
			// 	"id_type":             "RESIDENT_ID",
			// 	"verification_method": "doc_verification",
			// },
			// {
			// 	"country":             "ZA",
			// 	"id_type":             "NATIONAL_ID",
			// 	"verification_method": "biometric_kyc",
			// },
		},
		"callback_url":            fmt.Sprintf("%s/v1/kyc/webhook", serverConf.HostDomain),
		"data_privacy_policy_url": "https://github.com/usezoracle/rails-sui",
		"logo_url":                "https://i.postimg.cc/Twrq0gjC/mark-2x-2.png",
		"is_single_use":           true,
		"user_id":                 payload.WalletAddress,
		"expires_at":              timestamp.Add(1 * time.Hour).Format(time.RFC3339Nano),
	}).
		Send()
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "Failed to request identity verification", "Couldn't reach identity provider")
		return
	}

	data, err := u.ParseJSONResponse(res.RawResponse)
	if err != nil {
		logger.Errorf("error: %v %v", err, data)
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "Failed to request identity verification", data)
		return
	}

	// Save the verification details to the database
	ivr, err = storage.Client.IdentityVerificationRequest.
		Create().
		SetWalletAddress(payload.WalletAddress).
		SetWalletSignature(payload.Signature).
		SetPlatform("smile_id").
		SetPlatformRef(data["ref_id"].(string)).
		SetVerificationURL(data["link"].(string)).
		SetLastURLCreatedAt(timestamp.Add(24 * time.Hour)).
		Save(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to request identity verification", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Identity verification requested successfully", &types.NewIDVerificationResponse{
		URL:       ivr.VerificationURL,
		ExpiresAt: ivr.LastURLCreatedAt,
	})
}

// GetIDVerificationStatus controller fetches the status of an identity verification request
func (ctrl *Controller) GetIDVerificationStatus(ctx *gin.Context) {
	// Get wallet address from the URL
	walletAddress := ctx.Param("wallet_address")

	// Fetch related identity verification request from the database
	ivr, err := storage.Client.IdentityVerificationRequest.
		Query().
		Where(
			identityverificationrequest.WalletAddressEQ(walletAddress),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// Check the platform's status endpoint
			u.APIResponse(ctx, http.StatusNotFound, "error", "No verification request found for this wallet address", nil)
			return
		}
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch identity verification status", nil)
		return
	}

	// Check if the status is pending and fetch the status from the platform
	// if ivr.Status == identityverificationrequest.StatusPending {
	// 	status, err := getSmileLinkStatus(ivr.PlatformRef)
	// 	if err != nil {
	// 		logger.Errorf("error: %v", err)
	// 		u.APIResponse(ctx, http.StatusServiceUnavailable, "error", "Failed to fetch identity verification status", nil)
	// 		return
	// 	}

	// 	response.Status = status

	// 	if status != "pending" {
	// 		// Update the verification status in the database if it's not pending
	// 		_, err := storage.Client.IdentityVerificationRequest.
	// 			Update().
	// 			Where(
	// 				identityverificationrequest.WalletAddressEQ(walletAddress),
	// 			).
	// 			SetStatus(identityverificationrequest.Status(response.Status)).
	// 			Save(ctx)
	// 		if err != nil {
	// 			logger.Errorf("error: %v", err)
	// 			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to update identity verification status", nil)
	// 			return
	// 		}
	// 	}
	// }

	var status string

	// Check if the verification URL has expired
	if ivr.LastURLCreatedAt.Add(1 * time.Hour).Before(time.Now()) {
		status = "expired"
	} else {
		status = ivr.Status.String()
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Identity verification status fetched successfully", &types.IDVerificationStatusResponse{
		Status: status,
		URL:    ivr.VerificationURL,
	})
}

// KYCWebhook handles the webhook callback from Smile Identity
func (ctrl *Controller) KYCWebhook(ctx *gin.Context) {
	var payload types.SmileIDWebhookPayload

	// Parse the JSON payload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		logger.Errorf("Failed to parse webhook payload: %v", err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	// Verify the webhook signature
	if !verifySmileIDWebhookSignature(payload, payload.Signature) {
		logger.Errorf("Invalid webhook signature")
		ctx.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
		return
	}

	// Process the webhook
	status := identityverificationrequest.StatusPending

	// Check for success codes
	successCodes := []string{
		"0810", // Document Verified
		"1020", // Exact Match (Basic KYC and Enhanced KYC)
		"1012", // Valid ID / ID Number Validated (Enhanced KYC)
		"0820", // Authenticate User Machine Judgement - PASS
		"0840", // Enroll User PASS - Machine Judgement
	}

	// Check for failed codes
	failedCodes := []string{
		"0811", // No Face Match
		"0812", // Filed Security Features Check
		"0813", // Document Not Verified - Machine Judgement
		"1022", // No Match
		"1023", // No Found
		"1011", // Invalid ID / ID Number Invalid
		"1013", // ID Number Not Found
		"1014", // Unsupported ID Type
		"0821", // Images did not match
		"0911", // No Face Found
		"0912", // Face Not Matching
		"0921", // Face Not Found
		"0922", // Selfie Quality Too Poor
		"0841", // Enroll User FAIL
		"0941", // Face Not Found
		"0942", // Face Poor Quality
	}

	if slices.Contains(successCodes, payload.ResultCode) {
		status = identityverificationrequest.StatusSuccess
	}

	if slices.Contains(failedCodes, payload.ResultCode) {
		status = identityverificationrequest.StatusFailed
	}

	// Update the verification status in the database
	_, err := storage.Client.IdentityVerificationRequest.
		Update().
		Where(
			identityverificationrequest.WalletAddressEQ(payload.PartnerParams.UserID),
			identityverificationrequest.StatusEQ(identityverificationrequest.StatusPending),
		).
		SetStatus(status).
		Save(ctx)
	if err != nil {
		logger.Errorf("Failed to update verification status: %v", err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process webhook"})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"message": "Webhook processed successfully"})
}

// verifyWebhookSignature verifies the signature of a Smile Identity webhook
func verifySmileIDWebhookSignature(payload types.SmileIDWebhookPayload, receivedSignature string) bool {
	// Create HMAC
	// Generate Smile Identity signature
	h := hmac.New(sha256.New, []byte(identityConf.SmileIdentityApiKey))
	h.Write([]byte(payload.Timestamp))
	h.Write([]byte(identityConf.SmileIdentityPartnerId))
	h.Write([]byte("sid_request"))

	// Compare the computed signature with the one in the header
	computedSignature := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return computedSignature == receivedSignature
}

// getSmileLinkStatus fetches the status of a Smile Link
func getSmileLinkStatus(linkID string) (string, error) {
	// Generate signature
	timestamp := time.Now().Format(time.RFC3339Nano)
	h := hmac.New(sha256.New, []byte(identityConf.SmileIdentityApiKey))
	h.Write([]byte(timestamp))
	h.Write([]byte(identityConf.SmileIdentityPartnerId))
	h.Write([]byte("sid_request"))

	// Get Smile Link status
	res, err := fastshot.NewClient(identityConf.SmileIdentityBaseUrl).
		Config().SetTimeout(30 * time.Second).
		Build().POST(fmt.Sprintf("/v1/smile_links/%s", linkID)).
		Body().AsJSON(map[string]interface{}{
		"partner_id": identityConf.SmileIdentityPartnerId,
		"signature":  base64.StdEncoding.EncodeToString(h.Sum(nil)),
		"timestamp":  timestamp,
	}).
		Send()
	if err != nil {
		return "", fmt.Errorf("failed to get Smile Link status: %w", err)
	}

	data, err := u.ParseJSONResponse(res.RawResponse)
	if err != nil {
		return "", fmt.Errorf("failed to parse Smile Link response: %w", err)
	}

	totalJobs := data["total_jobs"].(float64)
	successfulJobs := data["successful_jobs"].(float64)
	failedJobs := data["failed_jobs"].(float64)

	if failedJobs+successfulJobs < totalJobs {
		return "pending", nil
	}

	if totalJobs == successfulJobs && failedJobs == 0 {
		return "success", nil
	}

	if failedJobs > 0 {
		return "failed", nil
	}

	return "", nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Public order endpoints (customer-facing checkout PWA).
// /v1/orders/:id is already public; the two below extend the same path
// with a confirm-after-pay ack and a per-order SSE stream so the
// customer's UI can mirror the merchant's bridge → settle progress
// without exposing the sender's other orders.
// ─────────────────────────────────────────────────────────────────────────────

type confirmOrderPayload struct {
	TxDigest string `json:"txDigest" binding:"required"`
}

// ConfirmOrderPayment is the customer-side "I sent the USDC" ack.
//
// The Sui event indexer is authoritative — it watches the order's
// receive_address and will eventually fire payment.deposited on its own.
// This endpoint exists to (1) shave seconds off the merchant's UI by
// pre-emitting payment.deposited the moment the customer signs, and
// (2) capture tx_hash for support/debug without scanning the chain.
//
// We do NOT validate the digest on-chain here — the indexer does, and a
// fake digest can't move a customer's balance. The status update happens
// only when the indexer confirms the deposit; this endpoint just emits
// an optimistic SSE event the customer's own page subscribes to.
func (ctrl *Controller) ConfirmOrderPayment(ctx *gin.Context) {
	idStr := ctx.Param("id")
	orderID, err := uuid.Parse(idStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid order id", nil)
		return
	}

	var payload confirmOrderPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", u.GetErrorData(err))
		return
	}

	po, err := storage.Client.PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(orderID)).
		WithSenderProfile().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "Order not found", nil)
			return
		}
		logger.Errorf("ConfirmOrderPayment.query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to record confirmation", nil)
		return
	}

	// Idempotent — same digest reposted is a no-op success.
	if po.TxHash == "" {
		if _, err := po.Update().SetTxHash(payload.TxDigest).Save(ctx); err != nil {
			logger.Errorf("ConfirmOrderPayment.update: %v", err)
			// Keep going — we still want to emit the SSE event so the
			// merchant UI advances. The tx_hash is observability nice-to-
			// have; the indexer will fill it in when it confirms.
		}
	}

	// Pre-emit a payment.deposited event so the merchant's SSE (and the
	// customer's new /v1/orders/:id/stream below) advance immediately.
	// The indexer will fire its own payment.deposited later when it sees
	// the on-chain effect; subscribers must tolerate duplicates (the
	// existing useTapBroadcast/PaymentsRealtimeProvider already do —
	// state transitions are idempotent).
	if po.Edges.SenderProfile != nil {
		svc.Bus().Publish(po.Edges.SenderProfile.ID, "payment.deposited", map[string]any{
			"order_id":    po.ID.String(),
			"sui_tx_hash": payload.TxDigest,
		})
	}

	logger.Infof("\n================================================================\n🔔 [ConfirmOrderPayment] Payment confirmation received for Order: %s (txDigest: %s)\n================================================================", po.ID, payload.TxDigest)

	u.APIResponse(ctx, http.StatusOK, "success", "Confirmation recorded", gin.H{
		"id":     po.ID,
		"status": po.Status,
	})
}

// StreamOrderStatus is a per-ORDER SSE stream — unauthenticated, scoped
// to one order ID. The customer-facing checkout PWA subscribes here
// after submitting their on-chain payment so the success screen can
// advance through Rails' bridge → settle pipeline in real time.
//
// Security: we look the order up, get its sender, subscribe to that
// sender's event bus, and filter events server-side to only those whose
// payload.order_id matches the URL param. The customer never sees other
// orders' events even though we're piggy-backing on the sender bus.
//
// Knowing an order's UUID is treated as the auth here — the same way
// the existing public GET /v1/orders/:id does. Order IDs are
// unguessable v4 UUIDs (~122 bits); guessing one is computationally
// infeasible.
func (ctrl *Controller) StreamOrderStatus(ctx *gin.Context) {
	idStr := ctx.Param("id")
	orderID, err := uuid.Parse(idStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid order id", nil)
		return
	}

	po, err := storage.Client.PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(orderID)).
		WithSenderProfile().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "Order not found", nil)
			return
		}
		logger.Errorf("StreamOrderStatus.query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to open stream", nil)
		return
	}
	if po.Edges.SenderProfile == nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Order has no owner", nil)
		return
	}
	orderIDStr := po.ID.String()

	ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	ctx.Writer.Header().Set("Cache-Control", "no-cache")
	ctx.Writer.Header().Set("Connection", "keep-alive")
	ctx.Writer.Header().Set("X-Accel-Buffering", "no")
	ctx.Writer.WriteHeader(http.StatusOK)

	flusher, isFlusher := ctx.Writer.(http.Flusher)
	if !isFlusher {
		_, _ = io.WriteString(ctx.Writer, "event: error\ndata: streaming not supported\n\n")
		return
	}

	lastEventID := ctx.GetHeader("Last-Event-ID")
	events, replay, unsubscribe := svc.Bus().Subscribe(po.Edges.SenderProfile.ID, lastEventID)
	defer unsubscribe()

	_, _ = io.WriteString(ctx.Writer, ": connected\n\n")
	flusher.Flush()

	// Replay matching events from the ring buffer (drops events for
	// other orders on this sender).
	for _, ev := range replay {
		if eventOrderID(ev) == orderIDStr {
			writeOrderSSE(ctx.Writer, ev)
			flusher.Flush()
		}
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	clientGone := ctx.Request.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(ctx.Writer, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return
			}
			if eventOrderID(ev) != orderIDStr {
				// Different order on the same sender — skip.
				continue
			}
			writeOrderSSE(ctx.Writer, ev)
			flusher.Flush()
		}
	}
}

func eventOrderID(ev svc.PaymentEvent) string {
	if v, ok := ev.Payload["order_id"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func writeOrderSSE(w io.Writer, ev svc.PaymentEvent) {
	data, err := json.Marshal(ev.Payload)
	if err != nil {
		logger.Errorf("OrderSSE marshal: %v", err)
		return
	}
	_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, data)
}
