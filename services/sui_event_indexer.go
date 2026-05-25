// sui_event_indexer.go runs the Sui-native event indexer: a long-lived
// WebSocket subscription against sui_subscribeEvent, filtered by MoveEventType
// to our deployed Gateway package's four events
// (OrderCreated / OrderSettled / OrderRefunded / SenderFeeTransferred).
//
// On each event:
//   1. Decode the ParsedJson payload into the matching types.Sui*Event struct.
//   2. Dispatch to the matching handler (UpdateOrderStatusSettled etc.)
//      which translates the on-chain state change into DB state transitions.
//
// The handlers themselves preserve the upstream business logic (LockPaymentOrder
// lifecycle, matching engine triggers) — see indexer.go for the surface; the
// handler bodies are next-chunk work.
//
// Lifecycle: Start() launches the subscriber goroutines + handler dispatcher,
// blocks until ctx is cancelled. Run from tasks.go at server boot.
package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/block-vision/sui-go-sdk/models"
	"github.com/block-vision/sui-go-sdk/sui"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/institution"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	networkent "github.com/usezoracle/rails-sui/ent/network"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/provisionbucket"
	tokenent "github.com/usezoracle/rails-sui/ent/token"
	"github.com/usezoracle/rails-sui/ent/transactionlog"
	"github.com/usezoracle/rails-sui/services/contracts"
	orderpkg "github.com/usezoracle/rails-sui/services/order"
	db "github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	"github.com/usezoracle/rails-sui/utils"
	cryptoUtils "github.com/usezoracle/rails-sui/utils/crypto"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// SuiEventIndexer subscribes to our Gateway package's Move events on Sui and
// dispatches them to handlers that update the DB state machine.
type SuiEventIndexer struct {
	wsURL         string
	packageID     string
	networkKey    string // "sui-mainnet" or "sui-testnet", used to scope DB Network lookups
	priorityQueue *PriorityQueueService
	orderSui      *orderpkg.OrderSui // used for Route A self-settle to aggregator
	subscribers   []*subscription
}

// subscription pairs an event-name filter with a typed handler.
type subscription struct {
	eventName string
	handler   func(ctx context.Context, raw models.SuiEventResponse) error
}

// NewSuiEventIndexer builds an indexer pointed at the given Sui WebSocket
// endpoint and Gateway package ID. networkKey identifies which Network row
// the indexer associates created LockPaymentOrders with (e.g. "sui-mainnet").
func NewSuiEventIndexer(wsURL, packageID, networkKey string) *SuiEventIndexer {
	var orderSui *orderpkg.OrderSui
	if svc, ok := orderpkg.NewOrderSui().(*orderpkg.OrderSui); ok {
		orderSui = svc
	}
	idx := &SuiEventIndexer{
		wsURL:         wsURL,
		packageID:     packageID,
		networkKey:    networkKey,
		priorityQueue: NewPriorityQueueService(),
		orderSui:      orderSui,
	}
	idx.subscribers = []*subscription{
		{eventName: contracts.EventOrderCreated, handler: idx.handleOrderCreated},
		{eventName: contracts.EventOrderSettled, handler: idx.handleOrderSettled},
		{eventName: contracts.EventOrderRefunded, handler: idx.handleOrderRefunded},
		// SenderFeeTransferred is informational only — logged, no DB update.
		{eventName: contracts.EventSenderFeeTransferred, handler: idx.handleSenderFeeTransferred},
		// Tap Card: settle the PaymentOrder + fire merchant SSE.
		{eventName: contracts.EventCardDebited, handler: idx.handleCardDebited},
	}
	return idx
}

// Start opens a WebSocket subscription per event type and blocks until ctx
// is cancelled. Each subscription runs in its own goroutine; failures in one
// don't stop the others.
func (s *SuiEventIndexer) Start(ctx context.Context) error {
	if s.packageID == "" {
		return fmt.Errorf("sui event indexer: SUI_GATEWAY_PACKAGE_ID not configured")
	}

	for _, sub := range s.subscribers {
		sub := sub // capture per-goroutine
		go s.runSubscription(ctx, sub)
	}

	<-ctx.Done()
	return ctx.Err()
}

// runSubscription opens one WebSocket subscription for one event type and
// dispatches each incoming event to the handler. On WebSocket failure, logs
// and exits the goroutine (a supervisor in tasks.go can re-Start the indexer).
func (s *SuiEventIndexer) runSubscription(ctx context.Context, sub *subscription) {
	// The config stores an HTTPS RPC URL, but the block-vision SDK's
	// WebSocket client requires wss://. Convert the scheme.
	wsURL := httpToWsURL(s.wsURL)
	cli := sui.NewSuiWebsocketClient(wsURL)
	ch := make(chan models.SuiEventResponse, 32)

	eventTypeTag := contracts.EventTypeTag(s.packageID, sub.eventName)

	err := cli.SubscribeEvent(ctx, models.SuiXSubscribeEventsRequest{
		SuiEventFilter: map[string]interface{}{
			"MoveEventType": eventTypeTag,
		},
	}, ch)
	if err != nil {
		logger.Errorf("sui event indexer: subscribe %s: %v", eventTypeTag, err)
		return
	}

	logger.Infof("sui event indexer: subscribed to %s", eventTypeTag)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if err := sub.handler(ctx, msg); err != nil {
				logger.Errorf("sui event indexer: handle %s: %v (tx=%s)", sub.eventName, err, msg.Id.TxDigest)
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Event handlers — decode the Move event ParsedJson into typed structs, then
// delegate to the existing Indexer interface methods for DB updates.
// ----------------------------------------------------------------------------

func (s *SuiEventIndexer) handleOrderCreated(ctx context.Context, raw models.SuiEventResponse) error {
	parsed, err := decodeMoveEvent(raw)
	if err != nil {
		return fmt.Errorf("decode OrderCreated: %w", err)
	}

	evt := &types.SuiOrderCreatedEvent{
		OrderID:         getString(parsed, "order_id"),
		Sender:          getString(parsed, "sender"),
		CoinType:        getString(parsed, "coin_type"),
		Amount:          getUint64(parsed, "amount"),
		ProtocolFee:     getUint64(parsed, "protocol_fee"),
		Rate:            getUint64(parsed, "rate"),
		InstitutionCode: getBytes(parsed, "institution_code"),
		MessageHash:     getString(parsed, "message_hash"),
		TxDigest:        raw.Id.TxDigest,
	}

	logger.Infof("sui event indexer: OrderCreated order_id=%s sender=%s amount=%d rate=%d tx=%s",
		evt.OrderID, evt.Sender, evt.Amount, evt.Rate, evt.TxDigest)

	// Idempotency — Move events can be redelivered on reconnect; de-dup by
	// Sui Order object ID OR by Sui tx digest (covers both retries and
	// historical replay).
	exists, err := db.Client.LockPaymentOrder.
		Query().
		Where(
			lockpaymentorder.Or(
				lockpaymentorder.GatewayIDEQ(evt.OrderID),
				lockpaymentorder.TxHashEQ(evt.TxDigest),
			),
		).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("OrderCreated: idempotency check: %w", err)
	}
	if exists {
		return nil
	}

	// Decrypt message_hash early — we need Reference to detect Route A orders
	// before deciding whether to enter the LP matching flow or self-settle.
	recipient, err := s.decryptMessageHash(evt.MessageHash)
	if err != nil {
		// Decryption failure means we can't safely process — log and skip.
		return fmt.Errorf("OrderCreated: decrypt message_hash: %w", err)
	}

	// Route A branch: if the Reference points to a PaymentOrder with a
	// RouteAOrder edge, drain the Gateway escrow to the aggregator wallet so
	// RouteADispatcher can bridge it. Skip LP matching entirely.
	if recipient.Reference != "" {
		isRouteA, err := s.handleRouteAIfApplicable(ctx, evt, recipient.Reference)
		if err != nil {
			logger.Errorf("OrderCreated: route-a branch (ref=%s): %v", recipient.Reference, err)
			// Fall through to Route B as a safety net — better to match an LP than
			// to leave coin stuck in escrow because of a routing bug.
		}
		if isRouteA {
			return nil
		}
	}

	if err := s.createLockPaymentOrderWithRecipient(ctx, evt, recipient); err != nil {
		return err
	}

	// Publish a "deposited" event to any SSE subscriber. We resolve back to
	// the originating PaymentOrder via Reference (set to PaymentOrder.ID
	// for sender-initiated orders — see InitiateTapPayment).
	if recipient.Reference != "" {
		if refUUID, parseErr := uuid.Parse(recipient.Reference); parseErr == nil {
			po, lookupErr := db.Client.PaymentOrder.
				Query().
				Where(paymentorder.IDEQ(refUUID)).
				WithSenderProfile().
				Only(ctx)
			if lookupErr == nil && po.Edges.SenderProfile != nil {
				Bus().Publish(po.Edges.SenderProfile.ID, "payment.deposited", map[string]any{
					"order_id":    po.ID.String(),
					"sui_tx_hash": evt.TxDigest,
				})
			}
		}
	}
	return nil
}

// handleRouteAIfApplicable checks whether the OrderCreated event belongs to
// a Route A order and, if so, self-settles the Gateway escrow to the
// aggregator wallet. Returns (true, nil) when handled as Route A; (false, nil)
// when the order is plain Route B (caller should continue with LP matching);
// or (true, err) when Route A was attempted but the self-settle failed
// (don't fall through to LP matching, the dispatcher will retry).
func (s *SuiEventIndexer) handleRouteAIfApplicable(ctx context.Context, evt *types.SuiOrderCreatedEvent, reference string) (bool, error) {
	refUUID, err := uuid.Parse(reference)
	if err != nil {
		// Reference isn't a UUID — not a Rails-managed PaymentOrder.
		return false, nil
	}

	po, err := db.Client.PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(refUUID)).
		WithRouteAOrder().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("lookup payment order by reference: %w", err)
	}
	if po.Edges.RouteAOrder == nil {
		return false, nil
	}

	if s.orderSui == nil {
		return true, fmt.Errorf("OrderSui not initialised (SUI_AGGREGATOR_PRIVATE_KEY missing?) — cannot self-settle")
	}

	if err := s.orderSui.SelfSettleToAggregator(ctx, evt.OrderID, evt.CoinType); err != nil {
		return true, fmt.Errorf("self-settle to aggregator: %w", err)
	}

	// Stamp the PaymentOrder so we can correlate later and surface state.
	if _, err := po.Update().SetGatewayID(evt.OrderID).SetTxHash(evt.TxDigest).Save(ctx); err != nil {
		logger.Errorf("OrderCreated route-a: persist GatewayID/TxHash on PaymentOrder %s: %v", po.ID, err)
	}

	logger.Infof("sui event indexer: route-a self-settled order=%s payment_order=%s tx=%s",
		evt.OrderID, po.ID, evt.TxDigest)
	return true, nil
}

// createLockPaymentOrder is the Sui-native equivalent of the upstream
// CreateLockPaymentOrder (services/indexer.go:860 in the original). It
// preserves the business logic — token lookup → message_hash decryption for
// recipient → institution + currency lookup → ProvisionBucket selection →
// LockPaymentOrder creation → matching-engine trigger — adapted for Sui:
//   - Token.ContractAddress is the Move type string (e.g. "0x...::usdc::USDC")
//   - GatewayID is the Sui Order object ID (already hex, no formatting needed)
//   - TxHash is the Sui transaction digest (base58)
//   - BlockNumber is unused on Sui (checkpoint-based finality)
func (s *SuiEventIndexer) createLockPaymentOrderWithRecipient(ctx context.Context, evt *types.SuiOrderCreatedEvent, recipient *types.PaymentOrderRecipient) error {
	// Find the Network row this event came from.
	network, err := db.Client.Network.
		Query().
		Where(networkent.IdentifierEQ(s.networkKey)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("OrderCreated: load network %s: %w", s.networkKey, err)
	}

	// Token: match by ContractAddress = the Move coin type string.
	token, err := db.Client.Token.
		Query().
		Where(
			tokenent.ContractAddressEQ(evt.CoinType),
			tokenent.HasNetworkWith(networkent.IDEQ(network.ID)),
		).
		WithNetwork().
		Only(ctx)
	if err != nil {
		return fmt.Errorf("OrderCreated: load token %s on %s: %w", evt.CoinType, s.networkKey, err)
	}

	// Institution + currency lookups.
	inst, err := db.Client.Institution.
		Query().
		Where(institution.CodeEQ(recipient.Institution)).
		WithFiatCurrency().
		Only(ctx)
	if err != nil {
		return fmt.Errorf("OrderCreated: load institution %s: %w", recipient.Institution, err)
	}
	if inst.Edges.FiatCurrency == nil {
		return fmt.Errorf("OrderCreated: institution %s has no fiat currency", recipient.Institution)
	}

	currency, err := db.Client.FiatCurrency.
		Query().
		Where(
			fiatcurrency.IsEnabledEQ(true),
			fiatcurrency.CodeEQ(inst.Edges.FiatCurrency.Code),
		).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("OrderCreated: load currency %s: %w", inst.Edges.FiatCurrency.Code, err)
	}

	// Amounts. Sui u64 → decimal scaled by token decimals (USDC = 6).
	amountInDecimals := utils.FromSubunit(new(big.Int).SetUint64(evt.Amount), token.Decimals)
	rate := decimal.NewFromBigInt(new(big.Int).SetUint64(evt.Rate), 0)
	fiatAmount := amountInDecimals.Mul(rate)

	// ProvisionBucket: tied to fiat amount + currency.
	provisionBucket, err := db.Client.ProvisionBucket.
		Query().
		Where(
			provisionbucket.MaxAmountGTE(fiatAmount),
			provisionbucket.MinAmountLTE(fiatAmount),
			provisionbucket.HasCurrencyWith(fiatcurrency.IDEQ(currency.ID)),
		).
		WithCurrency().
		Only(ctx)
	if err != nil {
		// No bucket = no LP can serve this amount. Order remains in escrow
		// until the aggregator decides to refund. Log loudly, do not crash.
		logger.Errorf("OrderCreated: no provision bucket for amount=%s currency=%s — order %s will not be matched", fiatAmount, currency.Code, evt.OrderID)
	}

	// Build the LockPaymentOrderFields for the priority queue.
	fields := types.LockPaymentOrderFields{
		Token:             token,
		Network:           network,
		GatewayID:         evt.OrderID,
		Amount:            amountInDecimals,
		Rate:              rate,
		BlockNumber:       0, // Sui doesn't use block numbers; checkpoint info isn't on this hot path.
		TxHash:            evt.TxDigest,
		Institution:       recipient.Institution,
		AccountIdentifier: recipient.AccountIdentifier,
		AccountName:       recipient.AccountName,
		ProviderID:        recipient.ProviderID,
		Memo:              recipient.Memo,
		ProvisionBucket:   provisionBucket,
	}

	// Hand off to the matching engine. AssignLockPaymentOrder is responsible
	// for both persisting the LockPaymentOrder row AND assigning a provider
	// (round-robin within rate ceiling, KYB-valid, balance ≥ min).
	if err := s.priorityQueue.AssignLockPaymentOrder(ctx, fields); err != nil {
		return fmt.Errorf("OrderCreated: assign to matching engine: %w", err)
	}

	logger.Infof("sui event indexer: OrderCreated processed order_id=%s currency=%s amount=%s fiat=%s",
		evt.OrderID, currency.Code, amountInDecimals, fiatAmount)
	return nil
}

// decryptMessageHash unwraps the base64-encoded RSA-encrypted JSON recipient
// blob from an OrderCreated event. Mirrors the upstream
// getOrderRecipientFromMessageHash (services/indexer.go:1462 in the original).
func (s *SuiEventIndexer) decryptMessageHash(messageHash string) (*types.PaymentOrderRecipient, error) {
	cipher, err := base64.StdEncoding.DecodeString(messageHash)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	plain, err := cryptoUtils.PublicKeyDecryptJSON(cipher, config.CryptoConfig().AggregatorPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("rsa decrypt: %w", err)
	}

	raw, err := json.Marshal(plain)
	if err != nil {
		return nil, err
	}

	var recipient *types.PaymentOrderRecipient
	if err := json.Unmarshal(raw, &recipient); err != nil {
		return nil, fmt.Errorf("recipient unmarshal: %w", err)
	}
	return recipient, nil
}

func (s *SuiEventIndexer) handleOrderSettled(ctx context.Context, raw models.SuiEventResponse) error {
	parsed, err := decodeMoveEvent(raw)
	if err != nil {
		return fmt.Errorf("decode OrderSettled: %w", err)
	}

	evt := &types.SuiOrderSettledEvent{
		SplitOrderID:      getBytes(parsed, "split_order_id"),
		OrderID:           getString(parsed, "order_id"),
		LiquidityProvider: getString(parsed, "liquidity_provider"),
		SettlePercent:     getUint64(parsed, "settle_percent"),
		AmountReleased:    getUint64(parsed, "amount_released"),
		TxDigest:          raw.Id.TxDigest,
	}

	logger.Infof("sui event indexer: OrderSettled order_id=%s lp=%s percent=%d amount=%d tx=%s",
		evt.OrderID, evt.LiquidityProvider, evt.SettlePercent, evt.AmountReleased, evt.TxDigest)

	return s.updateOrderStatusSettled(ctx, evt)
}

func (s *SuiEventIndexer) handleOrderRefunded(ctx context.Context, raw models.SuiEventResponse) error {
	parsed, err := decodeMoveEvent(raw)
	if err != nil {
		return fmt.Errorf("decode OrderRefunded: %w", err)
	}

	evt := &types.SuiOrderRefundedEvent{
		Fee:            getUint64(parsed, "fee"),
		OrderID:        getString(parsed, "order_id"),
		AmountRefunded: getUint64(parsed, "amount_refunded"),
		TxDigest:       raw.Id.TxDigest,
	}

	logger.Infof("sui event indexer: OrderRefunded order_id=%s fee=%d amount=%d tx=%s",
		evt.OrderID, evt.Fee, evt.AmountRefunded, evt.TxDigest)

	return s.updateOrderStatusRefunded(ctx, evt)
}

// ----------------------------------------------------------------------------
// DB state-transition functions — preserve the upstream pattern from
// /Users/mac/protocol/services/indexer.go (UpdateOrderStatusSettled +
// UpdateOrderStatusRefunded). Adapted for Sui semantics: gateway_id is the
// Sui Order object ID (already 0x-prefixed hex), tx_hash is the Sui digest
// (base58), no block_number (Sui uses checkpoints which we treat as opaque).
// ----------------------------------------------------------------------------

// updateOrderStatusSettled marks both the LockPaymentOrder (provider side) and
// PaymentOrder (sender side) as settled and fires the integrator webhook.
// Runs inside a single Ent transaction so the LockPaymentOrder + PaymentOrder
// + TransactionLog updates land atomically.
func (s *SuiEventIndexer) updateOrderStatusSettled(ctx context.Context, evt *types.SuiOrderSettledEvent) error {
	// Sender-side payment order (may not exist for integrator-initiated orders
	// where Rails is the sender of record).
	paymentOrderExists := true
	po, err := db.Client.PaymentOrder.
		Query().
		Where(paymentorder.GatewayIDEQ(evt.OrderID)).
		WithSenderProfile().
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return fmt.Errorf("OrderSettled: load payment order: %w", err)
		}
		paymentOrderExists = false
	}

	tx, err := db.Client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("OrderSettled: begin tx: %w", err)
	}

	// Create / update transaction log, keyed by (gateway_id, status).
	txMeta := map[string]interface{}{
		"OrderID":           evt.OrderID,
		"LiquidityProvider": evt.LiquidityProvider,
		"SettlePercent":     evt.SettlePercent,
		"AmountReleased":    evt.AmountReleased,
		"SplitOrderID":      evt.SplitOrderID,
	}
	updatedRows, err := tx.TransactionLog.
		Update().
		Where(
			transactionlog.StatusEQ(transactionlog.StatusOrderSettled),
			transactionlog.GatewayIDEQ(evt.OrderID),
		).
		SetTxHash(evt.TxDigest).
		SetMetadata(txMeta).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("OrderSettled: update tx log: %w", err)
	}
	var txLog *ent.TransactionLog
	if updatedRows == 0 {
		txLog, err = tx.TransactionLog.
			Create().
			SetStatus(transactionlog.StatusOrderSettled).
			SetGatewayID(evt.OrderID).
			SetTxHash(evt.TxDigest).
			SetMetadata(txMeta).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("OrderSettled: create tx log: %w", err)
		}
	}

	// Provider-side: LockPaymentOrder.status → settled
	lpoUpdate := tx.LockPaymentOrder.
		Update().
		Where(lockpaymentorder.GatewayIDEQ(evt.OrderID)).
		SetTxHash(evt.TxDigest).
		SetStatus(lockpaymentorder.StatusSettled)
	if txLog != nil {
		lpoUpdate = lpoUpdate.AddTransactions(txLog)
	}
	if _, err = lpoUpdate.Save(ctx); err != nil {
		return fmt.Errorf("OrderSettled: update lock payment order: %w", err)
	}

	// Sender-side: PaymentOrder.status → settled (if exists)
	if paymentOrderExists && po.Status != paymentorder.StatusSettled {
		poUpdate := tx.PaymentOrder.
			Update().
			Where(paymentorder.GatewayIDEQ(evt.OrderID)).
			SetTxHash(evt.TxDigest).
			SetStatus(paymentorder.StatusSettled)
		if txLog != nil {
			poUpdate = poUpdate.AddTransactions(txLog)
		}
		if _, err = poUpdate.Save(ctx); err != nil {
			return fmt.Errorf("OrderSettled: update payment order: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("OrderSettled: commit: %w", err)
	}

	// Integrator-facing webhook (only after successful commit so we never
	// notify a state that didn't actually land).
	if paymentOrderExists && po.Status != paymentorder.StatusSettled {
		po.Status = paymentorder.StatusSettled
		po.TxHash = evt.TxDigest
		if err := utils.SendPaymentOrderWebhook(ctx, po); err != nil {
			// Webhook failures are non-fatal; the DB state is correct and
			// retry happens via the WebhookRetryAttempt cron.
			logger.Errorf("OrderSettled: webhook: %v", err)
		}
		// Fan out to the in-process SSE bus — the Tapp Merchant app's
		// broadcast screen subscribes on the sender ID.
		if po.Edges.SenderProfile != nil {
			Bus().Publish(po.Edges.SenderProfile.ID, "payment.settled", map[string]any{
				"order_id":   po.ID.String(),
				"amount":     po.Amount.String(),
				"tx_hash":    evt.TxDigest,
				"settled_at": time.Now().UTC().Format(time.RFC3339),
			})
		}
	}
	return nil
}

// updateOrderStatusRefunded is the symmetric handler for refunds.
func (s *SuiEventIndexer) updateOrderStatusRefunded(ctx context.Context, evt *types.SuiOrderRefundedEvent) error {
	paymentOrderExists := true
	po, err := db.Client.PaymentOrder.
		Query().
		Where(paymentorder.GatewayIDEQ(evt.OrderID)).
		WithSenderProfile().
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return fmt.Errorf("OrderRefunded: load payment order: %w", err)
		}
		paymentOrderExists = false
	}

	tx, err := db.Client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("OrderRefunded: begin tx: %w", err)
	}

	txMeta := map[string]interface{}{
		"OrderID":        evt.OrderID,
		"Fee":            evt.Fee,
		"AmountRefunded": evt.AmountRefunded,
	}
	updatedRows, err := tx.TransactionLog.
		Update().
		Where(
			transactionlog.StatusEQ(transactionlog.StatusOrderRefunded),
			transactionlog.GatewayIDEQ(evt.OrderID),
		).
		SetTxHash(evt.TxDigest).
		SetMetadata(txMeta).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("OrderRefunded: update tx log: %w", err)
	}
	var txLog *ent.TransactionLog
	if updatedRows == 0 {
		txLog, err = tx.TransactionLog.
			Create().
			SetStatus(transactionlog.StatusOrderRefunded).
			SetGatewayID(evt.OrderID).
			SetTxHash(evt.TxDigest).
			SetMetadata(txMeta).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("OrderRefunded: create tx log: %w", err)
		}
	}

	lpoUpdate := tx.LockPaymentOrder.
		Update().
		Where(lockpaymentorder.GatewayIDEQ(evt.OrderID)).
		SetTxHash(evt.TxDigest).
		SetStatus(lockpaymentorder.StatusRefunded)
	if txLog != nil {
		lpoUpdate = lpoUpdate.AddTransactions(txLog)
	}
	if _, err = lpoUpdate.Save(ctx); err != nil {
		return fmt.Errorf("OrderRefunded: update lock payment order: %w", err)
	}

	if paymentOrderExists && po.Status != paymentorder.StatusRefunded {
		poUpdate := tx.PaymentOrder.
			Update().
			Where(paymentorder.GatewayIDEQ(evt.OrderID)).
			SetTxHash(evt.TxDigest).
			SetStatus(paymentorder.StatusRefunded)
		if txLog != nil {
			poUpdate = poUpdate.AddTransactions(txLog)
		}
		if _, err = poUpdate.Save(ctx); err != nil {
			return fmt.Errorf("OrderRefunded: update payment order: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("OrderRefunded: commit: %w", err)
	}

	if paymentOrderExists && po.Status != paymentorder.StatusRefunded {
		po.Status = paymentorder.StatusRefunded
		po.TxHash = evt.TxDigest
		if err := utils.SendPaymentOrderWebhook(ctx, po); err != nil {
			logger.Errorf("OrderRefunded: webhook: %v", err)
		}
		if po.Edges.SenderProfile != nil {
			Bus().Publish(po.Edges.SenderProfile.ID, "payment.refunded", map[string]any{
				"order_id":        po.ID.String(),
				"amount_refunded": evt.AmountRefunded,
				"tx_hash":         evt.TxDigest,
			})
		}
	}
	return nil
}


func (s *SuiEventIndexer) handleSenderFeeTransferred(ctx context.Context, raw models.SuiEventResponse) error {
	parsed, err := decodeMoveEvent(raw)
	if err != nil {
		return fmt.Errorf("decode SenderFeeTransferred: %w", err)
	}
	logger.Infof("sui event indexer: SenderFeeTransferred recipient=%s amount=%d tx=%s",
		getString(parsed, "sender_fee_recipient"),
		getUint64(parsed, "amount"),
		raw.Id.TxDigest,
	)
	return nil
}

// handleCardDebited resolves the PaymentOrder the Tap Card debit
// corresponds to (via `fiat_reference` = PaymentOrder.ID as UTF-8
// bytes), marks it settled, persists the tx digest, and publishes
// `payment.settled` to the merchant SSE bus so the merchant's
// "Payment received" screen lights up.
func (s *SuiEventIndexer) handleCardDebited(ctx context.Context, raw models.SuiEventResponse) error {
	parsed, err := decodeMoveEvent(raw)
	if err != nil {
		return fmt.Errorf("decode CardDebited: %w", err)
	}

	capID := getString(parsed, "cap_id")
	owner := getString(parsed, "owner")
	merchantRecipient := getString(parsed, "merchant_recipient")
	amountSubunit := getUint64(parsed, "amount_subunit")
	fiatRef := getBytes(parsed, "fiat_reference")

	logger.Infof("sui event indexer: CardDebited cap=%s owner=%s recipient=%s amount=%d ref=%s tx=%s",
		capID, owner, merchantRecipient, amountSubunit, string(fiatRef), raw.Id.TxDigest)

	// Resolve PaymentOrder by reference. The merchant_tap controller
	// sets PaymentOrder.Reference = the TappCard's UUID; the
	// fiat_reference passed to the Move call is PaymentOrder.ID
	// (different UUID). The DebitCard call uses po.ID — that's the
	// authoritative correlation key.
	poID, err := uuid.Parse(string(fiatRef))
	if err != nil {
		return fmt.Errorf("CardDebited: fiat_reference is not a UUID (got %q): %w", string(fiatRef), err)
	}

	po, err := db.Client.PaymentOrder.
		Query().
		Where(paymentorder.IDEQ(poID)).
		WithSenderProfile().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// Not ours — could be a debit from a non-Rails initiator.
			// Log and skip.
			logger.Infof("CardDebited: no PaymentOrder for ref %s — skipping", poID)
			return nil
		}
		return fmt.Errorf("CardDebited: load payment order %s: %w", poID, err)
	}

	if po.Status == paymentorder.StatusSettled {
		// Idempotent — event redelivery shouldn't double-emit SSE.
		return nil
	}

	if _, err := po.Update().
		SetStatus(paymentorder.StatusSettled).
		SetTxHash(raw.Id.TxDigest).
		Save(ctx); err != nil {
		return fmt.Errorf("CardDebited: mark settled: %w", err)
	}

	if po.Edges.SenderProfile != nil {
		Bus().Publish(po.Edges.SenderProfile.ID, "payment.settled", map[string]any{
			"order_id":   po.ID.String(),
			"amount":     po.Amount.String(),
			"tx_hash":    raw.Id.TxDigest,
			"source":     "tap_card",
			"settled_at": time.Now().UTC().Format(time.RFC3339),
		})
	}

	return nil
}

// ----------------------------------------------------------------------------
// Decode helpers — Sui event ParsedJson comes through as an
// interface{} (typically map[string]interface{}); these are tiny typed
// accessors so the handlers above stay readable.
// ----------------------------------------------------------------------------

func decodeMoveEvent(raw models.SuiEventResponse) (map[string]interface{}, error) {
	// ParsedJson is typed as map[string]interface{} in this SDK version.
	if raw.ParsedJson == nil {
		return nil, fmt.Errorf("event has no ParsedJson payload")
	}
	return raw.ParsedJson, nil
}

// Unused but retained to satisfy the json import for potential future use.
var _ = json.Marshal

func getString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// getUint64 extracts a numeric field. Sui Move u64 values arrive as strings
// (to avoid JSON number-precision issues for large values).
func getUint64(m map[string]interface{}, key string) uint64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case string:
		var out uint64
		_, _ = fmt.Sscanf(x, "%d", &out)
		return out
	case float64:
		return uint64(x)
	case uint64:
		return x
	case int64:
		return uint64(x)
	}
	return 0
}

// getBytes extracts a vector<u8> field. ParsedJson typically delivers these
// as either a hex string or a []interface{} of bytes.
func getBytes(m map[string]interface{}, key string) []byte {
	v, ok := m[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case string:
		// Sui often base64-encodes bytes but Move-formatted events deliver
		// them as their UTF-8 bytes when they look textual (e.g. institution
		// codes like "044"). Try raw bytes first.
		return []byte(x)
	case []interface{}:
		out := make([]byte, 0, len(x))
		for _, b := range x {
			if n, ok := b.(float64); ok {
				out = append(out, byte(n))
			}
		}
		return out
	}
	return nil
}

// httpToWsURL converts an HTTP(S) URL to its WebSocket equivalent.
// https://... → wss://...  and  http://... → ws://...
// If the URL already uses ws:// or wss://, it is returned unchanged.
func httpToWsURL(rawURL string) string {
	if strings.HasPrefix(rawURL, "wss://") || strings.HasPrefix(rawURL, "ws://") {
		return rawURL
	}
	if strings.HasPrefix(rawURL, "https://") {
		return "wss://" + strings.TrimPrefix(rawURL, "https://")
	}
	if strings.HasPrefix(rawURL, "http://") {
		return "ws://" + strings.TrimPrefix(rawURL, "http://")
	}
	// Fallback: prepend wss:// if no scheme at all.
	return "wss://" + rawURL
}
