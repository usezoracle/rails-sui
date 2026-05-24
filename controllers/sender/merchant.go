// merchant.go holds the controllers used by the Tapp Merchant mobile
// app — a small surface on top of the existing sender APIs:
//
//   POST /v1/sender/me/bank-account       SaveMerchantBankAccount
//   GET  /v1/sender/me/bank-account       GetMerchantBankAccount
//   POST /v1/sender/me/tap                InitiateTapPayment
//   POST /v1/sender/me/tap-card           InitiateTapCardPayment
//   GET  /v1/sender/me/payments/stream    StreamPayments (SSE)
//
// Auth is the same DynamicAuthMiddleware + OnlySenderMiddleware stack
// the rest of /v1/sender uses; the merchant identity is just a
// SenderProfile with an attached MerchantBankAccount.

package sender

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/institution"
	"github.com/usezoracle/rails-sui/ent/merchantbankaccount"
	"github.com/usezoracle/rails-sui/ent/network"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/suireceiveaddress"
	tokenEnt "github.com/usezoracle/rails-sui/ent/token"
	"github.com/usezoracle/rails-sui/ent/transactionlog"
	svc "github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// -----------------------------------------------------------------------------
// Bank account: save + fetch the merchant's NGN payout account.
// -----------------------------------------------------------------------------

type saveBankAccountPayload struct {
	Currency      string `json:"currency"       binding:"required"` // e.g. "NGN"
	BankCode      string `json:"bank_code"      binding:"required"` // CBN code
	AccountNumber string `json:"account_number" binding:"required"` // 10-digit NUBAN for NGN
	AccountName   string `json:"account_name"   binding:"required"` // resolved name
}

type bankAccountResponse struct {
	ID            uuid.UUID  `json:"id"`
	Currency      string     `json:"currency"`
	BankCode      string     `json:"bank_code"`
	AccountNumber string     `json:"account_number"`
	AccountName   string     `json:"account_name"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
}

// SaveMerchantBankAccount upserts the merchant's payout account.
// The client is expected to have already called POST /v1/verify-account
// to resolve account_name before saving — this endpoint trusts the
// supplied name but re-validates institution code + format.
func (ctrl *SenderController) SaveMerchantBankAccount(ctx *gin.Context) {
	var payload saveBankAccountPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}

	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	payload.Currency = strings.ToUpper(strings.TrimSpace(payload.Currency))
	if payload.Currency != "NGN" {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Only NGN is supported in v1", types.ErrorData{
				Field: "Currency", Message: "Must be NGN",
			})
		return
	}

	// Validate institution exists and belongs to the declared currency.
	inst, err := storage.Client.Institution.
		Query().
		Where(institution.CodeEQ(payload.BankCode)).
		WithFiatCurrency().
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", types.ErrorData{
				Field: "BankCode", Message: "Unknown bank code",
			})
		return
	}
	if inst.Edges.FiatCurrency == nil || inst.Edges.FiatCurrency.Code != payload.Currency {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", types.ErrorData{
				Field: "BankCode", Message: "Bank does not match the declared currency",
			})
		return
	}

	// NUBAN check for NGN — 10 digits exactly.
	if payload.Currency == "NGN" && !ngnAccountNumberRegex.MatchString(payload.AccountNumber) {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", types.ErrorData{
				Field: "AccountNumber", Message: "NGN accounts must be 10 digits",
			})
		return
	}

	now := time.Now()
	existing, err := storage.Client.MerchantBankAccount.
		Query().
		Where(merchantbankaccount.HasSenderProfileWith(senderprofile.IDEQ(sender.ID))).
		Only(ctx)

	var saved *ent.MerchantBankAccount
	switch {
	case err == nil:
		saved, err = existing.Update().
			SetCurrency(payload.Currency).
			SetBankCode(payload.BankCode).
			SetAccountNumber(payload.AccountNumber).
			SetAccountName(payload.AccountName).
			SetVerifiedAt(now).
			Save(ctx)
	case ent.IsNotFound(err):
		saved, err = storage.Client.MerchantBankAccount.Create().
			SetCurrency(payload.Currency).
			SetBankCode(payload.BankCode).
			SetAccountNumber(payload.AccountNumber).
			SetAccountName(payload.AccountName).
			SetVerifiedAt(now).
			SetSenderProfile(sender).
			Save(ctx)
	default:
		logger.Errorf("SaveMerchantBankAccount: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to save bank account", nil)
		return
	}
	if err != nil {
		logger.Errorf("SaveMerchantBankAccount: persist: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to save bank account", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Bank account saved",
		bankAccountResponseFromEnt(saved))
}

// GetMerchantBankAccount returns the merchant's saved payout account
// (or 404 if not yet set).
func (ctrl *SenderController) GetMerchantBankAccount(ctx *gin.Context) {
	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	row, err := storage.Client.MerchantBankAccount.
		Query().
		Where(merchantbankaccount.HasSenderProfileWith(senderprofile.IDEQ(sender.ID))).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "Bank account not set", nil)
			return
		}
		logger.Errorf("GetMerchantBankAccount: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to fetch bank account", nil)
		return
	}

	u.APIResponse(ctx, http.StatusOK, "success", "Bank account retrieved",
		bankAccountResponseFromEnt(row))
}

// -----------------------------------------------------------------------------
// Tap to take (phone-to-phone): one-shot PaymentOrder creation that auto-
// populates recipient from the saved MerchantBankAccount.
// -----------------------------------------------------------------------------

type tapPaymentPayload struct {
	// Fiat amount the customer should pay (e.g. "2500.00" NGN).
	Amount decimal.Decimal `json:"amount" binding:"required"`
	// Optional 3-letter currency code. Defaults to the merchant's saved
	// bank account currency (NGN in v1).
	Currency string `json:"currency"`
	// Free-text shown alongside the order on the merchant dashboard.
	Memo string `json:"memo"`
}

type tapPaymentResponse struct {
	OrderID     uuid.UUID `json:"order_id"`
	CheckoutURL string    `json:"checkout_url"`
	Amount      string    `json:"amount"`
	Currency    string    `json:"currency"`
	Token       string    `json:"token"`
	Network     string    `json:"network"`
	Rate        string    `json:"rate"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// InitiateTapPayment creates a PaymentOrder for a phone-to-phone tap.
// The merchant supplies only a fiat amount; we pull the recipient from
// the saved MerchantBankAccount, derive the crypto-side amount from the
// fiat currency's current MarketRate, and return a checkout URL the
// merchant phone broadcasts via NFC HCE (Android) or QR (iOS).
func (ctrl *SenderController) InitiateTapPayment(ctx *gin.Context) {
	var payload tapPaymentPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	if !payload.Amount.IsPositive() {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Amount must be greater than zero", nil)
		return
	}

	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	bank, err := storage.Client.MerchantBankAccount.
		Query().
		Where(merchantbankaccount.HasSenderProfileWith(senderprofile.IDEQ(sender.ID))).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusPreconditionFailed, "error",
				"Save a bank account before taking payments", nil)
			return
		}
		logger.Errorf("InitiateTapPayment: bank lookup: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	currencyCode := strings.ToUpper(strings.TrimSpace(payload.Currency))
	if currencyCode == "" {
		currencyCode = bank.Currency
	}
	if currencyCode != bank.Currency {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Currency does not match saved bank account", nil)
		return
	}

	currency, err := storage.Client.FiatCurrency.
		Query().
		Where(
			fiatcurrency.CodeEQ(currencyCode),
			fiatcurrency.IsEnabledEQ(true),
		).
		Only(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Currency is not supported", nil)
		return
	}

	if !currency.MarketRate.IsPositive() {
		u.APIResponse(ctx, http.StatusFailedDependency, "error",
			"Market rate not available — try again shortly", nil)
		return
	}

	// Default token + network for v1: a Sui USDC-class token. We pick the
	// first enabled token whose network identifier begins with "sui-" —
	// keeps mainnet/testnet selection environment-driven via the Token
	// table rather than env shims.
	tok, err := storage.Client.Token.
		Query().
		Where(
			tokenEnt.IsEnabledEQ(true),
			tokenEnt.SymbolEQ("USDC"),
			tokenEnt.HasNetworkWith(network.IdentifierHasPrefix("sui-")),
		).
		WithNetwork().
		First(ctx)
	if err != nil {
		logger.Errorf("InitiateTapPayment: default token lookup: %v", err)
		u.APIResponse(ctx, http.StatusFailedDependency, "error",
			"Default tap token (USDC on Sui) is not configured", nil)
		return
	}

	cryptoAmount := payload.Amount.Div(currency.MarketRate).Round(int32(tok.Decimals))
	if !cryptoAmount.IsPositive() {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Computed crypto amount rounds to zero — increase fiat amount", nil)
		return
	}

	address, encryptedSeed, err := ctrl.receiveAddressService.CreateSuiReceiveAddress(ctx)
	if err != nil {
		logger.Errorf("InitiateTapPayment: receive address: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	orderID := uuid.New()
	validUntil := time.Now().Add(orderConf.ReceiveAddressValidity)

	receiveAddr, err := storage.Client.SuiReceiveAddress.
		Create().
		SetAddress(address).
		SetEncryptedSeed(encryptedSeed).
		SetCoinType(tok.ContractAddress).
		SetExpectedAmount(u.ToSubunit(cryptoAmount, tok.Decimals).Uint64()).
		SetStatus(suireceiveaddress.StatusUnused).
		SetValidUntil(validUntil).
		Save(ctx)
	if err != nil {
		logger.Errorf("InitiateTapPayment: persist receive address: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	tx, err := storage.Client.Tx(ctx)
	if err != nil {
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	txLog, err := tx.TransactionLog.
		Create().
		SetStatus(transactionlog.StatusOrderInitiated).
		SetNetwork(tok.Edges.Network.Identifier).
		SetMetadata(map[string]interface{}{
			"Source":         "tapp_merchant_tap",
			"SenderID":       sender.ID.String(),
			"BankCode":       bank.BankCode,
			"FiatAmount":     payload.Amount.String(),
			"Currency":       currencyCode,
			"ReceiveAddress": receiveAddr.Address,
		}).
		Save(ctx)
	if err != nil {
		logger.Errorf("InitiateTapPayment: tx log: %v", err)
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	po, err := tx.PaymentOrder.
		Create().
		SetID(orderID).
		// Set Reference = order ID so the Sui event indexer can resolve
		// the OrderCreated event back to this row and publish a
		// "payment.deposited" event to the SSE bus.
		SetReference(orderID.String()).
		SetSenderProfile(sender).
		SetAmount(cryptoAmount).
		SetAmountPaid(decimal.NewFromInt(0)).
		SetAmountReturned(decimal.NewFromInt(0)).
		SetPercentSettled(decimal.NewFromInt(0)).
		SetNetworkFee(tok.Edges.Network.Fee).
		SetProtocolFee(decimal.NewFromInt(0)).
		SetSenderFee(decimal.NewFromInt(0)).
		SetToken(tok).
		SetRate(currency.MarketRate).
		SetSuiReceiveAddress(receiveAddr).
		SetReceiveAddressText(receiveAddr.Address).
		SetFeePercent(decimal.NewFromInt(0)).
		SetFeeAddress("").
		SetReturnAddress("").
		AddTransactions(txLog).
		Save(ctx)
	if err != nil {
		logger.Errorf("InitiateTapPayment: payment order: %v", err)
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	if _, err := tx.PaymentOrderRecipient.
		Create().
		SetInstitution(bank.BankCode).
		SetAccountIdentifier(bank.AccountNumber).
		SetAccountName(bank.AccountName).
		SetMemo(payload.Memo).
		SetPaymentOrder(po).
		Save(ctx); err != nil {
		logger.Errorf("InitiateTapPayment: recipient: %v", err)
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	// Enrol the order in Route A (Sui USDC → Base via LiFi → fiat via the
	// settlement aggregator). Without this row the dispatcher never sees
	// the order and the customer's USDC sits at the receive address.
	// Mode = "lp" because the merchant flow always settles to a real bank
	// account; "treasury" is reserved for protocol-internal sweeps.
	if _, err := tx.RouteAOrder.
		Create().
		SetMode(routeaorder.ModeLp).
		SetBridgeStatus(routeaorder.BridgeStatusPending).
		SetPaymentOrder(po).
		Save(ctx); err != nil {
		logger.Errorf("InitiateTapPayment: route_a_order: %v", err)
		_ = tx.Rollback()
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Errorf("InitiateTapPayment: commit: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"Failed to initiate payment", nil)
		return
	}

	u.APIResponse(ctx, http.StatusCreated, "success", "Tap payment initiated",
		tapPaymentResponse{
			OrderID:     po.ID,
			CheckoutURL: u.BuildCheckoutURL(po.ID),
			Amount:      payload.Amount.String(),
			Currency:    currencyCode,
			Token:       tok.Symbol,
			Network:     tok.Edges.Network.Identifier,
			Rate:        currency.MarketRate.String(),
			ExpiresAt:   validUntil,
		})
}

// -----------------------------------------------------------------------------
// Tap Card (NTAG215): debit a previously-linked Tapp Card. v1 stub —
// the card-linking system lives on the checkout web and isn't wired
// yet, so this returns a clear "not recognized" so the mobile app's
// fallback path activates.
// -----------------------------------------------------------------------------

type tapCardPayload struct {
	Amount      decimal.Decimal `json:"amount" binding:"required"`
	Currency    string          `json:"currency"`
	Memo        string          `json:"memo"`
	CardUIDHash string          `json:"card_uid_hash" binding:"required"`
}

// InitiateTapCardPayment is currently a stub. The full implementation
// resolves card_uid_hash → linked TappCard → linked SenderProfile →
// builds and submits a debit PTB against the Move card module. Until
// the card-linking system lands, we surface a 501 so the merchant app
// shows "Card not recognized" rather than silently failing.
func (ctrl *SenderController) InitiateTapCardPayment(ctx *gin.Context) {
	var payload tapCardPayload
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"Failed to validate payload", u.GetErrorData(err))
		return
	}
	u.APIResponse(ctx, http.StatusNotImplemented, "error",
		"Tap Card not yet recognized — link cards via the Zoracle web app first",
		map[string]any{
			"reason":        "card_unrecognized",
			"card_uid_hash": payload.CardUIDHash,
		})
}

// -----------------------------------------------------------------------------
// SSE stream: real-time PaymentOrder status updates for the merchant.
// -----------------------------------------------------------------------------

// StreamPayments holds the HTTP connection open as a text/event-stream
// and forwards every PaymentOrder lifecycle event scoped to the authed
// SenderProfile. Bidirectional close: client disconnect ends the loop;
// server context cancel ends it too.
func (ctrl *SenderController) StreamPayments(ctx *gin.Context) {
	sender, ok := senderFromCtx(ctx)
	if !ok {
		return
	}

	// SSE headers — Set before WriteHeader / before first flush.
	ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	ctx.Writer.Header().Set("Cache-Control", "no-cache")
	ctx.Writer.Header().Set("Connection", "keep-alive")
	ctx.Writer.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	ctx.Writer.WriteHeader(http.StatusOK)

	flusher, isFlusher := ctx.Writer.(http.Flusher)
	if !isFlusher {
		// Should never happen with gin's default writer, but bail gracefully.
		_, _ = io.WriteString(ctx.Writer, "event: error\ndata: streaming not supported\n\n")
		return
	}

	lastEventID := ctx.GetHeader("Last-Event-ID")
	events, replay, unsubscribe := svc.Bus().Subscribe(sender.ID, lastEventID)
	defer unsubscribe()

	// Immediately write a comment line so intermediate proxies see headers
	// + first byte and don't time out the connection on the handshake.
	_, _ = io.WriteString(ctx.Writer, ": connected\n\n")
	flusher.Flush()

	for _, ev := range replay {
		writeSSE(ctx.Writer, ev)
		flusher.Flush()
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
			writeSSE(ctx.Writer, ev)
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, ev svc.PaymentEvent) {
	data, err := json.Marshal(ev.Payload)
	if err != nil {
		logger.Errorf("SSE marshal: %v", err)
		return
	}
	_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, data)
}

// -----------------------------------------------------------------------------
// Shared helpers (file-scoped, so they don't pollute the package API).
// -----------------------------------------------------------------------------

var ngnAccountNumberRegex = regexp.MustCompile(`^[0-9]{10}$`)

func bankAccountResponseFromEnt(row *ent.MerchantBankAccount) bankAccountResponse {
	resp := bankAccountResponse{
		ID:            row.ID,
		Currency:      row.Currency,
		BankCode:      row.BankCode,
		AccountNumber: row.AccountNumber,
		AccountName:   row.AccountName,
	}
	if row.VerifiedAt != nil {
		t := *row.VerifiedAt
		resp.VerifiedAt = &t
	}
	return resp
}

func senderFromCtx(ctx *gin.Context) (*ent.SenderProfile, bool) {
	senderCtx, ok := ctx.Get("sender")
	if !ok || senderCtx == nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Invalid API key or token", nil)
		return nil, false
	}
	sender, ok := senderCtx.(*ent.SenderProfile)
	if !ok || sender == nil {
		u.APIResponse(ctx, http.StatusUnauthorized, "error",
			"Sender not authenticated", nil)
		return nil, false
	}
	return sender, true
}
