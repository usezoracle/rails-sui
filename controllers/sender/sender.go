package sender

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/storage"

	"github.com/usezoracle/rails-sui/ent/institution"
	"github.com/usezoracle/rails-sui/ent/network"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	providerprofile "github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/ent/senderordertoken"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	tokenEnt "github.com/usezoracle/rails-sui/ent/token"
	"github.com/usezoracle/rails-sui/ent/transactionlog"
	svc "github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/services/lifi"
	"github.com/usezoracle/rails-sui/services/settlement"
	"github.com/usezoracle/rails-sui/types"

	suisigner "github.com/block-vision/sui-go-sdk/signer"
	"github.com/shopspring/decimal"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"

	"github.com/gin-gonic/gin"
)

// SenderController is a controller type for sender endpoints
type SenderController struct {
	receiveAddressService *svc.ReceiveAddressService
}

// NewSenderController creates a new instance of SenderController
func NewSenderController() *SenderController {

	return &SenderController{
		receiveAddressService: svc.NewReceiveAddressService(),
	}
}

var serverConf = config.ServerConfig()
var orderConf = config.OrderConfig()

// InitiatePaymentOrder controller creates a payment order
func (ctrl *SenderController) InitiatePaymentOrder(ctx *gin.Context) {
	var payload types.NewPaymentOrderPayload

	if err := ctx.ShouldBindJSON(&payload); err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	// Get sender profile from the context
	senderCtx, ok := ctx.Get("sender")
	if !ok {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid API key or token", nil)
		return
	}
	sender := senderCtx.(*ent.SenderProfile)

	if !sender.IsActive && !serverConf.Debug {
		u.APIResponse(ctx, http.StatusForbidden, "error", "Your account is not active", nil)
		return
	}

	// Get token from DB
	token, err := storage.Client.Token.
		Query().
		Where(
			tokenEnt.SymbolEQ(payload.Token),
			tokenEnt.HasNetworkWith(network.IdentifierEQ(payload.Network)),
			tokenEnt.IsEnabledEQ(true),
		).
		WithNetwork().
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
			Field:   "Token",
			Message: "Provided token is not supported",
		})
		return
	}

	// Handle sender profile overrides
	senderOrderToken, err := storage.Client.SenderOrderToken.
		Query().
		Where(
			senderordertoken.HasTokenWith(
				tokenEnt.IDEQ(token.ID),
			),
			senderordertoken.HasSenderWith(
				senderprofile.IDEQ(sender.ID),
			),
		).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
			Field:   "Token",
			Message: "Provided token is not configured",
		})
		return
	}

	if senderOrderToken.FeeAddress == "" || senderOrderToken.RefundAddress == "" {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
			Field:   "Token",
			Message: "Fee address or refund address is not configured",
		})
		return
	}

	feePercent := senderOrderToken.FeePercent
	feeAddress := senderOrderToken.FeeAddress
	returnAddress := senderOrderToken.RefundAddress

	if payload.FeeAddress != "" {
		if !sender.IsPartner {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
				Field:   "FeeAddress",
				Message: "FeeAddress is not allowed",
			})
			return
		}

		if payload.FeePercent.IsZero() {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
				Field:   "FeePercent",
				Message: "FeePercent must be greater than zero",
			})
			return
		}

		if !strings.HasPrefix(payload.Network, "tron") {
			if !u.IsValidEthereumAddress(payload.FeeAddress) {
				u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
					Field:   "FeeAddress",
					Message: "Invalid Ethereum address",
				})
				return
			}
		} else {
			if !u.IsValidTronAddress(payload.FeeAddress) {
				u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
					Field:   "FeeAddress",
					Message: "Invalid Tron address",
				})
				return
			}
		}

		feePercent = payload.FeePercent
		feeAddress = payload.FeeAddress
	}

	if payload.ReturnAddress != "" {
		if !strings.HasPrefix(payload.Network, "tron") {
			if !u.IsValidEthereumAddress(payload.ReturnAddress) {
				u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
					Field:   "ReturnAddress",
					Message: "Invalid Ethereum address",
				})
				return
			}
		} else {
			if !u.IsValidTronAddress(payload.ReturnAddress) {
				u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
					Field:   "ReturnAddress",
					Message: "Invalid Tron address",
				})
				return
			}
		}
		returnAddress = payload.ReturnAddress
	}

	if payload.Reference != "" {
		if !regexp.MustCompile(`^[a-zA-Z0-9\-_]+$`).MatchString(payload.Reference) {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
				Field:   "Reference",
				Message: "Reference must be alphanumeric",
			})
			return
		}

		referenceExists, err := storage.Client.PaymentOrder.
			Query().
			Where(
				paymentorder.ReferenceEQ(payload.Reference),
			).
			Exist(ctx)
		if err != nil {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
			return
		}

		if referenceExists {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
				Field:   "Reference",
				Message: "Reference already exists",
			})
			return
		}
	}

	// Validate if institution exists
	institutionExists, err := storage.Client.Institution.
		Query().
		Where(
			institution.CodeEQ(payload.Recipient.Institution),
		).
		Exist(ctx)
	if err != nil {
		logger.Errorf("error validating institution: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to validate institution", nil)
		return
	}
	if !institutionExists {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Failed to validate payload", types.ErrorData{
			Field:   "Recipient",
			Message: "Invalid institution code provided",
		})
		return
	}

	isPrivate := false
	isTokenNetworkPresent := false
	maxOrderAmount := decimal.NewFromInt(0)
	minOrderAmount := decimal.NewFromInt(0)

	if payload.Recipient.ProviderID != "" {
		providerProfile, err := storage.Client.ProviderProfile.
			Query().
			Where(
				providerprofile.IDEQ(payload.Recipient.ProviderID),
			).
			WithOrderTokens().
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

	out:
		for _, orderToken := range providerProfile.Edges.OrderTokens {
			for _, address := range orderToken.Addresses {
				if address.Network == token.Edges.Network.Identifier {
					isTokenNetworkPresent = true
					break out
				}
			}
		}

		if !isTokenNetworkPresent {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "The selected network is not supported by the specified provider", nil)
			return
		}

		maxOrderAmount = providerProfile.Edges.OrderTokens[0].MaxOrderAmount
		minOrderAmount = providerProfile.Edges.OrderTokens[0].MinOrderAmount

		if providerProfile.VisibilityMode == providerprofile.VisibilityModePrivate {
			isPrivate = true
		}
	}

	// Validate amount for private orders
	if isPrivate {
		if payload.Amount.LessThan(minOrderAmount) {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "The amount is below the minimum order amount for the specified provider", nil)
			return
		} else if payload.Amount.GreaterThan(maxOrderAmount) {
			u.APIResponse(ctx, http.StatusBadRequest, "error", "The amount is beyond the maximum order amount for the specified provider", nil)
			return
		}
	}

	// Generate a per-order Sui receive address (Path-2 deposit flow). The
	// service returns the Sui address derived from a fresh Ed25519 keypair
	// + the AES-encrypted seed for safekeeping.
	address, encryptedSeed, err := ctrl.receiveAddressService.CreateSuiReceiveAddress(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		return
	}

	validUntil := time.Now().Add(orderConf.ReceiveAddressValidity)
	// Private orders (memo prefix "P#P") never expire — they may be paid
	// at the sender's leisure.
	if strings.HasPrefix(payload.Recipient.Memo, "P#P") {
		validUntil = time.Time{}
	}

	receiveAddress, err := storage.Client.SuiReceiveAddress.
		Create().
		SetAddress(address).
		SetEncryptedSeed(encryptedSeed).
		SetCoinType(token.ContractAddress).
		SetExpectedAmount(u.ToSubunit(payload.Amount, token.Decimals).Uint64()).
		SetStatus(suireceiveaddress.StatusUnused).
		SetValidUntil(validUntil).
		Save(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		return
	}

	// Create payment order and recipient in a transaction
	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		return
	}

	senderFee := feePercent.Mul(payload.Amount).Div(decimal.NewFromInt(100)).Round(4)
	protocolFee := decimal.NewFromFloat(0)

	// Create transaction Log
	transactionLog, err := tx.TransactionLog.
		Create().
		SetStatus(transactionlog.StatusOrderInitiated).
		SetMetadata(
			map[string]interface{}{
				"ReceiveAddress": receiveAddress.Address,
				"SenderID":       sender.ID.String(),
			},
		).SetNetwork(token.Edges.Network.Identifier).
		Save(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		_ = tx.Rollback()
		return
	}

	// Create payment order
	paymentOrder, err := tx.PaymentOrder.
		Create().
		SetSenderProfile(sender).
		SetAmount(payload.Amount).
		SetAmountPaid(decimal.NewFromInt(0)).
		SetAmountReturned(decimal.NewFromInt(0)).
		SetPercentSettled(decimal.NewFromInt(0)).
		SetNetworkFee(token.Edges.Network.Fee).
		SetProtocolFee(protocolFee).
		SetSenderFee(senderFee).
		SetToken(token).
		SetRate(payload.Rate).
		SetSuiReceiveAddress(receiveAddress).
		SetReceiveAddressText(receiveAddress.Address).
		SetFeePercent(feePercent).
		SetFeeAddress(feeAddress).
		SetReturnAddress(returnAddress).
		SetReference(payload.Reference).
		AddTransactions(transactionLog).
		Save(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		_ = tx.Rollback()
		return
	}

	// Create payment order recipient
	_, err = tx.PaymentOrderRecipient.
		Create().
		SetInstitution(payload.Recipient.Institution).
		SetAccountIdentifier(payload.Recipient.AccountIdentifier).
		SetAccountName(payload.Recipient.AccountName).
		SetProviderID(payload.Recipient.ProviderID).
		SetMemo(payload.Recipient.Memo).
		SetPaymentOrder(paymentOrder).
		Save(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		_ = tx.Rollback()
		return
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate payment order", nil)
		return
	}

	u.APIResponse(ctx, http.StatusCreated, "success", "Payment order initiated successfully",
		&types.ReceiveAddressResponse{
			ID:             paymentOrder.ID,
			Amount:         paymentOrder.Amount,
			Token:          payload.Token,
			Network:        token.Edges.Network.Identifier,
			ReceiveAddress: receiveAddress.Address,
			ValidUntil:     receiveAddress.ValidUntil,
			SenderFee:      senderFee,
			TransactionFee: protocolFee.Add(token.Edges.Network.Fee),
			Reference:      paymentOrder.Reference,
		})
}

// InitiateRouteAOrder creates a Route A payment order (Sui USDC bridged to
// Base via LiFi, then dispatched to fiat via settlement). Path-2 (receive-address) deposit
// flow only in v1 — the user sends Sui USDC from any wallet/exchange to the
// returned receive address; the deposit watcher detects it, forwards via
// OrderSui.CreateOrder (embedding the PaymentOrder UUID in message_hash);
// the indexer's handleOrderCreated sees the RouteAOrder edge, self-settles
// Gateway escrow to the aggregator wallet; RouteADispatcher then bridges
// via LiFi and dispatches per mode.
//
// Payload extends NewPaymentOrderPayload with a `mode` field
// ("lp" | "treasury", default "treasury").
func (ctrl *SenderController) InitiateRouteAOrder(ctx *gin.Context) {
	var payload struct {
		types.NewPaymentOrderPayload
		Mode string `json:"mode"` // "lp" | "treasury"
	}
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	mode := strings.ToLower(strings.TrimSpace(payload.Mode))
	if mode == "" {
		mode = "treasury"
	}
	if mode != "lp" && mode != "treasury" {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"mode must be one of: lp, treasury", nil)
		return
	}

	senderVal, _ := ctx.Get("sender")
	sender, ok := senderVal.(*ent.SenderProfile)
	if !ok || sender == nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Sender not authenticated", nil)
		return
	}

	token, err := storage.Client.Token.
		Query().
		Where(
			tokenEnt.SymbolEQ(strings.ToUpper(payload.Token)),
			tokenEnt.IsEnabledEQ(true),
		).
		WithNetwork().
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Token is not supported", nil)
		return
	}

	// Auto-override the user-supplied rate with our resolved off-ramp
	// rate. Orders submitted above the LP ceiling auto-refund after
	// refundTimeoutMinutes (typically 2 min) — quoting from the
	// aggregator at order creation and locking that rate avoids the
	// dead-tx loop. v1 hardcodes (network="base", currency="NGN") since
	// Route A always bridges to Base + only NGN institutions are wired.
	// Derive both from destination chain + recipient.institution in v1.5.
	//
	// USDC source: direct the aggregator NGN/USDC quote (LiFi USDC→USDC is
	// effectively passthrough, ~0 slippage).
	//
	// Native SUI source: the aggregator has no SUI/NGN venue, so we compose
	// the rate from (LiFi SUI→USDC) × (the aggregator USDC→NGN). See
	// services/route_a_quote.go.
	settlementTTL := time.Duration(orderConf.SettlementPubkeyTTLSeconds) * time.Second
	settlementClient := settlement.New(orderConf.SettlementAPIURL, settlementTTL)

	var resolvedRate decimal.Decimal
	var providerIDs []string

	if len(orderConf.SuiAggregatorPrivateKey) != 32 {
		u.APIResponse(ctx, http.StatusServiceUnavailable, "error",
			"Route A orders require SUI_AGGREGATOR_PRIVATE_KEY to be configured", nil)
		return
	}
	lifiClient := lifi.New(orderConf.LiFiAPIKey)
	aggSigner := suisigner.NewSigner(orderConf.SuiAggregatorPrivateKey)
	composite, qerr := svc.QuoteSuiTokenToNgn(ctx, lifiClient, settlementClient,
		orderConf, aggSigner.Address, payload.Amount, token.ContractAddress, int32(token.Decimals))
	if qerr != nil {
		logger.Errorf("InitiateRouteAOrder.composite_rate token=%s: %v", token.Symbol, qerr)
		u.APIResponse(ctx, http.StatusBadGateway, "error",
			fmt.Sprintf("Couldn't compose %s→NGN rate (bridge + settlement fees included) — try again in a moment", token.Symbol), nil)
		return
	}
	resolvedRate = composite.Rate
	providerIDs = composite.ProviderIDs
	logger.Infof("route-a: %s composite rate amount=%s usdc_equiv=%s ngn_per_token=%s ngn_per_usdc=%s providers=%v",
		token.Symbol, payload.Amount, composite.UsdcEquivalent, composite.Rate, composite.UsdcToNgnRate, composite.ProviderIDs)
	if !payload.Rate.Equal(resolvedRate) {
		logger.Infof("route-a: rate override token=%s requested=%s resolved=%s providers=%v",
			payload.Token, payload.Rate.String(), resolvedRate.String(), providerIDs)
	}
	payload.Rate = resolvedRate

	address, encryptedSeed, err := ctrl.receiveAddressService.CreateSuiReceiveAddress(ctx)
	if err != nil {
		logger.Errorf("InitiateRouteAOrder.receive_address: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	receiveAddress, err := storage.Client.SuiReceiveAddress.
		Create().
		SetAddress(address).
		SetEncryptedSeed(encryptedSeed).
		SetCoinType(token.ContractAddress).
		SetExpectedAmount(u.ToSubunit(payload.Amount, token.Decimals).Uint64()).
		SetStatus(suireceiveaddress.StatusUnused).
		SetValidUntil(time.Now().Add(orderConf.ReceiveAddressValidity)).
		Save(ctx)
	if err != nil {
		logger.Errorf("InitiateRouteAOrder.receive_address.persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	paymentOrder, err := tx.PaymentOrder.
		Create().
		SetSenderProfile(sender).
		SetAmount(payload.Amount).
		SetAmountPaid(decimal.NewFromInt(0)).
		SetAmountReturned(decimal.NewFromInt(0)).
		SetPercentSettled(decimal.NewFromInt(0)).
		SetNetworkFee(token.Edges.Network.Fee).
		SetProtocolFee(decimal.NewFromInt(0)).
		SetSenderFee(decimal.NewFromInt(0)).
		SetToken(token).
		SetRate(payload.Rate).
		SetSuiReceiveAddress(receiveAddress).
		SetReceiveAddressText(receiveAddress.Address).
		SetFeePercent(decimal.NewFromInt(0)).
		SetReturnAddress(payload.ReturnAddress).
		SetReference(payload.Reference).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		logger.Errorf("InitiateRouteAOrder.payment_order: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	if _, err := tx.PaymentOrderRecipient.
		Create().
		SetInstitution(payload.Recipient.Institution).
		SetAccountIdentifier(payload.Recipient.AccountIdentifier).
		SetAccountName(payload.Recipient.AccountName).
		SetProviderID(payload.Recipient.ProviderID).
		SetMemo(payload.Recipient.Memo).
		SetPaymentOrder(paymentOrder).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		logger.Errorf("InitiateRouteAOrder.recipient: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	if _, err := tx.RouteAOrder.
		Create().
		SetMode(routeaorder.Mode(mode)).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetPaymentOrder(paymentOrder).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		logger.Errorf("InitiateRouteAOrder.route_a_order: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Errorf("InitiateRouteAOrder.commit: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to initiate route-a order", nil)
		return
	}

	u.APIResponse(ctx, http.StatusCreated, "success", "Route A payment order initiated", map[string]interface{}{
		"id":              paymentOrder.ID,
		"mode":            mode,
		"amount":          paymentOrder.Amount,
		"rate":            paymentOrder.Rate,
		"coin_type":       token.ContractAddress,
		"receive_address": receiveAddress.Address,
		"valid_until":     receiveAddress.ValidUntil,
		"reference":       paymentOrder.Reference,
	})
}

// GetPaymentOrderByID controller fetches a payment order by ID
func (ctrl *SenderController) GetPaymentOrderByID(ctx *gin.Context) {
	// Get order ID from the URL
	orderID := ctx.Param("id")
	isUUID := true

	// Convert order ID to UUID
	id, err := uuid.Parse(orderID)
	if err != nil {
		isUUID = false
	}

	// Get sender profile from the context
	senderCtx, ok := ctx.Get("sender")
	if !ok {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid API key or token", nil)
		return
	}
	sender := senderCtx.(*ent.SenderProfile)

	// Fetch payment order from the database
	paymentOrderQuery := storage.Client.PaymentOrder.Query()

	if isUUID {
		paymentOrderQuery = paymentOrderQuery.Where(paymentorder.IDEQ(id))
	} else {
		paymentOrderQuery = paymentOrderQuery.Where(paymentorder.ReferenceEQ(orderID))
	}

	paymentOrder, err := paymentOrderQuery.
		Where(paymentorder.HasSenderProfileWith(senderprofile.IDEQ(sender.ID))).
		WithRecipient().
		WithToken(func(tq *ent.TokenQuery) {
			tq.WithNetwork()
		}).
		WithTransactions().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error",
				"Payment order not found", nil)
		} else {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to fetch payment order", nil)
		}
		return
	}

	var transactions []types.TransactionLog
	for _, transaction := range paymentOrder.Edges.Transactions {
		transactions = append(transactions, types.TransactionLog{
			ID:        transaction.ID,
			GatewayId: transaction.GatewayID,
			Status:    transaction.Status,
			TxHash:    transaction.TxHash,
			CreatedAt: transaction.CreatedAt,
		})

	}

	institution, err := storage.Client.Institution.
		Query().
		Where(institution.CodeEQ(paymentOrder.Edges.Recipient.Institution)).
		WithFiatCurrency().
		Only(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch payment order", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "The order has been successfully retrieved", &types.PaymentOrderResponse{
		ID:             paymentOrder.ID,
		Amount:         paymentOrder.Amount,
		AmountPaid:     paymentOrder.AmountPaid,
		AmountReturned: paymentOrder.AmountReturned,
		Token:          paymentOrder.Edges.Token.Symbol,
		SenderFee:      paymentOrder.SenderFee,
		TransactionFee: paymentOrder.NetworkFee.Add(paymentOrder.ProtocolFee),
		Rate:           paymentOrder.Rate,
		Network:        paymentOrder.Edges.Token.Edges.Network.Identifier,
		Recipient: types.PaymentOrderRecipient{
			Currency:          institution.Edges.FiatCurrency.Code,
			Institution:       institution.Name,
			AccountIdentifier: paymentOrder.Edges.Recipient.AccountIdentifier,
			AccountName:       paymentOrder.Edges.Recipient.AccountName,
			ProviderID:        paymentOrder.Edges.Recipient.ProviderID,
			Memo:              paymentOrder.Edges.Recipient.Memo,
		},
		Transactions:   transactions,
		FromAddress:    paymentOrder.FromAddress,
		ReturnAddress:  paymentOrder.ReturnAddress,
		ReceiveAddress: paymentOrder.ReceiveAddressText,
		FeeAddress:     paymentOrder.FeeAddress,
		Reference:      paymentOrder.Reference,
		GatewayID:      paymentOrder.GatewayID,
		CreatedAt:      paymentOrder.CreatedAt,
		UpdatedAt:      paymentOrder.UpdatedAt,
		TxHash:         paymentOrder.TxHash,
		Status:         paymentOrder.Status,
	})
}

// GetPaymentOrders controller fetches all payment orders
func (ctrl *SenderController) GetPaymentOrders(ctx *gin.Context) {
	// Get sender profile from the context
	senderCtx, ok := ctx.Get("sender")
	if !ok {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid API key or token", nil)
		return
	}
	sender := senderCtx.(*ent.SenderProfile)

	// Get ordering query param
	ordering := ctx.Query("ordering")
	order := ent.Desc(paymentorder.FieldCreatedAt)
	if ordering == "asc" {
		order = ent.Asc(paymentorder.FieldCreatedAt)
	}

	// Get page and pageSize query params
	page, offset, pageSize := u.Paginate(ctx)

	paymentOrderQuery := storage.Client.PaymentOrder.Query()

	// Filter by sender
	paymentOrderQuery = paymentOrderQuery.Where(
		paymentorder.HasSenderProfileWith(senderprofile.IDEQ(sender.ID)),
	)

	// Filter by status
	statusQueryParam := ctx.Query("status")
	statusMap := map[string]paymentorder.Status{
		"initiated": paymentorder.StatusInitiated,
		"pending":   paymentorder.StatusPending,
		"expired":   paymentorder.StatusExpired,
		"settled":   paymentorder.StatusSettled,
		"refunded":  paymentorder.StatusRefunded,
	}

	if status, ok := statusMap[statusQueryParam]; ok {
		paymentOrderQuery = paymentOrderQuery.Where(
			paymentorder.StatusEQ(status),
		)
	}

	// Filter by token
	tokenQueryParam := ctx.Query("token")

	if tokenQueryParam != "" {
		tokenExists, err := storage.Client.Token.
			Query().
			Where(
				tokenEnt.SymbolEQ(tokenQueryParam),
			).
			Exist(ctx)
		if err != nil {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to fetch payment orders", nil)
			return
		}

		if tokenExists {
			paymentOrderQuery = paymentOrderQuery.Where(
				paymentorder.HasTokenWith(
					tokenEnt.SymbolEQ(tokenQueryParam),
				),
			)
		}
	}

	// Filter by network
	networkQueryParam := ctx.Query("network")

	if networkQueryParam != "" {
		networkExists, err := storage.Client.Network.
			Query().
			Where(
				network.IdentifierEQ(networkQueryParam),
			).
			Exist(ctx)
		if err != nil {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error",
				"Failed to fetch payment orders", nil)
			return
		}

		if networkExists {
			paymentOrderQuery = paymentOrderQuery.Where(
				paymentorder.HasTokenWith(
					tokenEnt.HasNetworkWith(
						network.IdentifierEQ(networkQueryParam),
					),
				),
			)
		}
	}

	count, err := paymentOrderQuery.Count(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch payment orders", nil)
		return
	}

	// Fetch payment orders
	paymentOrders, err := paymentOrderQuery.
		WithRecipient().
		WithToken(func(tq *ent.TokenQuery) {
			tq.WithNetwork()
		}).
		Limit(pageSize).
		Offset(offset).
		Order(order).
		All(ctx)
	if err != nil {
		logger.Errorf("error: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to fetch payment orders", nil)
		return
	}

	var orders []types.PaymentOrderResponse

	for _, paymentOrder := range paymentOrders {
		institution, err := storage.Client.Institution.
			Query().
			Where(institution.CodeEQ(paymentOrder.Edges.Recipient.Institution)).
			WithFiatCurrency().
			Only(ctx)
		if err != nil {
			logger.Errorf("error: %v", err)
			u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch payment orders", nil)
			return
		}

		orders = append(orders, types.PaymentOrderResponse{
			ID:             paymentOrder.ID,
			Amount:         paymentOrder.Amount,
			AmountPaid:     paymentOrder.AmountPaid,
			AmountReturned: paymentOrder.AmountReturned,
			Token:          paymentOrder.Edges.Token.Symbol,
			SenderFee:      paymentOrder.SenderFee,
			TransactionFee: paymentOrder.NetworkFee.Add(paymentOrder.ProtocolFee),
			Rate:           paymentOrder.Rate,
			Network:        paymentOrder.Edges.Token.Edges.Network.Identifier,
			Recipient: types.PaymentOrderRecipient{
				Currency:          institution.Edges.FiatCurrency.Code,
				Institution:       institution.Name,
				AccountIdentifier: paymentOrder.Edges.Recipient.AccountIdentifier,
				AccountName:       paymentOrder.Edges.Recipient.AccountName,
				ProviderID:        paymentOrder.Edges.Recipient.ProviderID,
				Memo:              paymentOrder.Edges.Recipient.Memo,
			},
			FromAddress:    paymentOrder.FromAddress,
			ReturnAddress:  paymentOrder.ReturnAddress,
			ReceiveAddress: paymentOrder.ReceiveAddressText,
			FeeAddress:     paymentOrder.FeeAddress,
			Reference:      paymentOrder.Reference,
			GatewayID:      paymentOrder.GatewayID,
			CreatedAt:      paymentOrder.CreatedAt,
			UpdatedAt:      paymentOrder.UpdatedAt,
			TxHash:         paymentOrder.TxHash,
			Status:         paymentOrder.Status,
		})
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Payment orders retrieved successfully", types.SenderPaymentOrderList{
		Page:         page,
		PageSize:     pageSize,
		TotalRecords: count,
		Orders:       orders,
	})
}

// Stats controller fetches sender stats
func (ctrl *SenderController) Stats(ctx *gin.Context) {
	senderCtx, ok := ctx.Get("sender")
	if !ok {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Sender profile required", nil)
		return
	}
	sender := senderCtx.(*ent.SenderProfile)

	// Optional `?period=today` filter. Default (omitted / "all") returns
	// lifetime totals, preserving the historical API shape.
	var (
		periodSince *time.Time
		now         = time.Now()
	)
	switch strings.ToLower(ctx.Query("period")) {
	case "today":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		periodSince = &start
	case "week":
		start := now.AddDate(0, 0, -7)
		periodSince = &start
	case "month":
		start := now.AddDate(0, -1, 0)
		periodSince = &start
	case "all", "":
		periodSince = nil
	default:
		u.APIResponse(ctx, http.StatusBadRequest, "error", "period must be one of: today, week, month, all", nil)
		return
	}

	// Volume + sender fees over settled orders in the period.
	volQuery := storage.Client.PaymentOrder.Query().Where(
		paymentorder.HasSenderProfileWith(senderprofile.IDEQ(sender.ID)),
		paymentorder.StatusEQ(paymentorder.StatusSettled),
	)
	if periodSince != nil {
		volQuery = volQuery.Where(paymentorder.CreatedAtGTE(*periodSince))
	}

	var w []struct {
		Sum               decimal.Decimal
		SumFieldSenderFee decimal.Decimal
	}
	if err := volQuery.
		Aggregate(
			ent.Sum(paymentorder.FieldAmount),
			ent.As(ent.Sum(paymentorder.FieldSenderFee), "SumFieldSenderFee"),
		).
		Scan(ctx, &w); err != nil {
		logger.Errorf("Stats.volume: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch sender stats", nil)
		return
	}

	// Order count over the same period — across all statuses so an
	// abandoned-cart count doesn't drop to zero on a slow day.
	countQuery := storage.Client.PaymentOrder.Query().Where(
		paymentorder.HasSenderProfileWith(senderprofile.IDEQ(sender.ID)),
	)
	if periodSince != nil {
		countQuery = countQuery.Where(paymentorder.CreatedAtGTE(*periodSince))
	}

	var v []struct{ Count int }
	if err := countQuery.Aggregate(ent.Count()).Scan(ctx, &v); err != nil {
		logger.Errorf("Stats.count: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to fetch sender stats", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Sender stats retrieved successfully", types.SenderStatsResponse{
		TotalOrders:      v[0].Count,
		TotalOrderVolume: w[0].Sum,
		TotalFeeEarnings: w[0].SumFieldSenderFee,
	})
}

// CancelOrder lets the sender abandon an in-flight PaymentOrder before
// the customer pays. Allowed only on orders the merchant actually owns,
// and only while the order is still in `initiated` or `pending` state —
// once settlement begins it's too late to cancel client-side.
//
// Idempotent: cancelling an already-cancelled / expired / settled order
// is a no-op 200 with the current state surfaced in the body, so the
// merchant app's tear-down path (back button, broadcast timeout) doesn't
// have to special-case 409s.
func (ctrl *SenderController) CancelOrder(ctx *gin.Context) {
	idStr := ctx.Param("id")
	orderID, err := uuid.Parse(idStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "Invalid order id", nil)
		return
	}

	sender, ok := ctx.Get("sender")
	if !ok || sender == nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Sender profile required", nil)
		return
	}
	senderProfile := sender.(*ent.SenderProfile)

	order, err := storage.Client.PaymentOrder.
		Query().
		Where(
			paymentorder.IDEQ(orderID),
			paymentorder.HasSenderProfileWith(senderprofile.IDEQ(senderProfile.ID)),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "Order not found", nil)
			return
		}
		logger.Errorf("CancelOrder.query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to cancel order", nil)
		return
	}

	// Idempotent — already in a terminal state.
	switch order.Status {
	case paymentorder.StatusCancelled, paymentorder.StatusExpired,
		paymentorder.StatusSettled, paymentorder.StatusRefunded:
		u.APIResponse(ctx, http.StatusOK, "success", "Order is already final",
			gin.H{"id": order.ID, "status": order.Status})
		return
	}

	// Active orders can be cancelled.
	updated, err := order.Update().
		SetStatus(paymentorder.StatusCancelled).
		Save(ctx)
	if err != nil {
		logger.Errorf("CancelOrder.update: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "Failed to cancel order", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Order cancelled",
		gin.H{"id": updated.ID, "status": updated.Status})
}
