package tasks

// (import block — shinamiGas added for the gas-fund cron)
import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/google/uuid"
	fastshot "github.com/opus-domini/fast-shot"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/usezoracle/rails-sui/config"
	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/fiatcurrency"
	"github.com/usezoracle/rails-sui/ent/lockorderfulfillment"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/transactionlog"
	"github.com/usezoracle/rails-sui/ent/webhookretryattempt"
	"github.com/usezoracle/rails-sui/services"
	"github.com/usezoracle/rails-sui/services/baas"
	orderpkg "github.com/usezoracle/rails-sui/services/order"
	shinamiGas "github.com/usezoracle/rails-sui/services/shinami_gas"
	"github.com/usezoracle/rails-sui/storage"
	"github.com/usezoracle/rails-sui/types"
	"github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

var orderConf = config.OrderConfig()
var serverConf = config.ServerConfig()

// ReassignPendingOrders reassigns declined order requests to providers
func ReassignPendingOrders() {
	ctx := context.Background()

	// Remove provider id from pending lock orders
	_, err := storage.Client.LockPaymentOrder.
		Update().
		Where(
			lockpaymentorder.StatusEQ(lockpaymentorder.StatusPending),
			lockpaymentorder.Not(lockpaymentorder.HasFulfillments()),
		).
		ClearProvider().
		Save(ctx)
	if err != nil {
		logger.Errorf("ReassignPendingOrders.db: %v", err)
		return
	}

	// Query pending lock orders
	lockOrders, err := storage.Client.LockPaymentOrder.
		Query().
		Where(
			lockpaymentorder.StatusEQ(lockpaymentorder.StatusPending),
			lockpaymentorder.Not(lockpaymentorder.HasFulfillments()),
		).
		WithToken().
		WithProvider().
		WithProvisionBucket(
			func(pbq *ent.ProvisionBucketQuery) {
				pbq.WithCurrency()
			},
		).
		All(ctx)
	if err != nil {
		logger.Errorf("ReassignPendingOrders.db: %v", err)
		return
	}

	// Check if order_request_<order_id> exists in Redis
	for _, order := range lockOrders {
		orderKey := fmt.Sprintf("order_request_%s", order.ID)
		exists, err := storage.RedisClient.Exists(ctx, orderKey).Result()
		if err != nil {
			logger.Errorf("ReassignPendingOrders.redis: %v", err)
			continue
		}

		if exists == 0 {
			// Order request doesn't exist in Redis, reassign the order
			lockPaymentOrder := types.LockPaymentOrderFields{
				ID:                order.ID,
				Token:             order.Edges.Token,
				GatewayID:         order.GatewayID,
				Amount:            order.Amount,
				Rate:              order.Rate,
				BlockNumber:       order.BlockNumber,
				Institution:       order.Institution,
				AccountIdentifier: order.AccountIdentifier,
				AccountName:       order.AccountName,
				Memo:              order.Memo,
				ProvisionBucket:   order.Edges.ProvisionBucket,
			}

			if order.Edges.Provider != nil {
				lockPaymentOrder.ProviderID = order.Edges.Provider.ID
			}

			err := services.NewPriorityQueueService().AssignLockPaymentOrder(ctx, lockPaymentOrder)
			if err != nil {
				logger.Errorf("failed to reassign declined order request: %v", err)
			}
		}
	}
}

// ReassignUnfulfilledLockOrders reassigns lockOrder unfulfilled within a time frame.
func ReassignUnfulfilledLockOrders() {
	ctx := context.Background()

	// Unassign unfulfilled lock orders. Never touch an order whose platform
	// payout is in-flight or already paid (pending/success) — that order is
	// owned by the execute/settle flow and reassigning it could double-pay.
	// Node-operated orders keep fiat_payout_status=none, so they're unaffected.
	_, err := storage.Client.LockPaymentOrder.
		Update().
		Where(
			lockpaymentorder.FiatPayoutStatusNotIn(lockpaymentorder.FiatPayoutStatusPending, lockpaymentorder.FiatPayoutStatusSuccess),
			lockpaymentorder.Or(
				lockpaymentorder.And(
					lockpaymentorder.StatusEQ(lockpaymentorder.StatusProcessing),
					lockpaymentorder.UpdatedAtLTE(time.Now().Add(-orderConf.OrderFulfillmentValidity*time.Minute)),
				),
				lockpaymentorder.StatusEQ(lockpaymentorder.StatusCancelled),
			),
			lockpaymentorder.Or(
				lockpaymentorder.Not(lockpaymentorder.HasFulfillments()),
				lockpaymentorder.HasFulfillmentsWith(
					lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusFailed),
					lockorderfulfillment.Not(lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusSuccess)),
					lockorderfulfillment.Not(lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusPending)),
				),
			),
		).
		SetStatus(lockpaymentorder.StatusPending).
		ClearProvider().
		Save(ctx)
	if err != nil {
		logger.Errorf("ReassignUnfulfilledLockOrders: %v", err)
		return
	}

	// Query unfulfilled lock orders.
	lockOrders, err := storage.Client.LockPaymentOrder.
		Query().
		Where(
			lockpaymentorder.FiatPayoutStatusNotIn(lockpaymentorder.FiatPayoutStatusPending, lockpaymentorder.FiatPayoutStatusSuccess),
			lockpaymentorder.Or(
				lockpaymentorder.Not(lockpaymentorder.HasFulfillments()),
				lockpaymentorder.HasFulfillmentsWith(
					lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusFailed),
					lockorderfulfillment.Not(lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusSuccess)),
					lockorderfulfillment.Not(lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusPending)),
				),
			),
			lockpaymentorder.Or(
				lockpaymentorder.StatusEQ(lockpaymentorder.StatusProcessing),
				lockpaymentorder.StatusEQ(lockpaymentorder.StatusCancelled),
			),
			lockpaymentorder.Or(
				lockpaymentorder.Or(
					lockpaymentorder.And(
						lockpaymentorder.StatusEQ(lockpaymentorder.StatusProcessing),
						lockpaymentorder.UpdatedAtLTE(time.Now().Add(-orderConf.OrderFulfillmentValidity*time.Minute)),
					),
					lockpaymentorder.StatusEQ(lockpaymentorder.StatusCancelled),
				),
				lockpaymentorder.HasFulfillmentsWith(
					lockorderfulfillment.CreatedAtLTE(time.Now().Add(-orderConf.OrderFulfillmentValidity*time.Minute)),
				),
			),
		).
		WithToken().
		WithProvider().
		WithProvisionBucket(func(pbq *ent.ProvisionBucketQuery) {
			pbq.WithCurrency()
		}).
		All(ctx)
	if err != nil {
		logger.Errorf("ReassignUnfulfilledLockOrders: %v", err)
		return
	}

	for _, order := range lockOrders {
		lockPaymentOrder := types.LockPaymentOrderFields{
			ID:                order.ID,
			Token:             order.Edges.Token,
			GatewayID:         order.GatewayID,
			Amount:            order.Amount,
			Rate:              order.Rate,
			BlockNumber:       order.BlockNumber,
			Institution:       order.Institution,
			AccountIdentifier: order.AccountIdentifier,
			AccountName:       order.AccountName,
			Memo:              order.Memo,
			ProvisionBucket:   order.Edges.ProvisionBucket,
		}

		if order.Edges.Provider != nil {
			lockPaymentOrder.ProviderID = order.Edges.Provider.ID
		}

		err := services.NewPriorityQueueService().AssignLockPaymentOrder(ctx, lockPaymentOrder)
		if err != nil {
			logger.Errorf("ReassignUnfulfilledLockOrders.AssignLockPaymentOrder: %s => %v", order.GatewayID, err)
		}
	}
}

// ReassignUnvalidatedLockOrders reassigns or refunds unvalidated lock orders to providers
func ReassignUnvalidatedLockOrders() {
	ctx := context.Background()

	// Query unvalidated lock orders. Exclude orders with an in-flight/paid
	// platform payout so a paid order is never refunded mid-settle.
	lockOrders, err := storage.Client.LockPaymentOrder.
		Query().
		Where(
			lockpaymentorder.FiatPayoutStatusNotIn(lockpaymentorder.FiatPayoutStatusPending, lockpaymentorder.FiatPayoutStatusSuccess),
			lockpaymentorder.Or(
				lockpaymentorder.StatusEQ(lockpaymentorder.StatusFulfilled),
				lockpaymentorder.And(
					lockpaymentorder.StatusEQ(lockpaymentorder.StatusCancelled),
					lockpaymentorder.HasFulfillmentsWith(
						lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusPending),
					),
				),
			),
			lockpaymentorder.Or(
				lockpaymentorder.HasFulfillmentsWith(
					lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusFailed),
					lockorderfulfillment.Not(lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusSuccess)),
					lockorderfulfillment.Not(lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusPending)),
				),
				lockpaymentorder.And(
					lockpaymentorder.HasFulfillmentsWith(
						lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusPending),
					),
					lockpaymentorder.HasFulfillmentsWith(
						lockorderfulfillment.UpdatedAtLTE(time.Now().Add(-orderConf.OrderFulfillmentValidity*time.Minute)),
						lockorderfulfillment.Not(lockorderfulfillment.UpdatedAtGT(time.Now().Add(-orderConf.OrderFulfillmentValidity*time.Minute))),
					),
				),
				lockpaymentorder.HasFulfillmentsWith(
					lockorderfulfillment.ValidationStatusEQ(lockorderfulfillment.ValidationStatusSuccess),
				),
			),
		).
		WithToken(func(tq *ent.TokenQuery) {
			tq.WithNetwork()
		}).
		WithProvider(func(pq *ent.ProviderProfileQuery) {
			pq.WithAPIKey()
		}).
		WithFulfillments().
		WithProvisionBucket(func(pb *ent.ProvisionBucketQuery) {
			pb.WithCurrency()
		}).
		All(ctx)
	if err != nil {
		logger.Errorf("ReassignUnvalidatedLockOrders.db: %v", err)
		return
	}

	for _, order := range lockOrders {
		for _, fulfillment := range order.Edges.Fulfillments {
			if fulfillment.ValidationStatus == lockorderfulfillment.ValidationStatusPending {
				// TODO: use auth
				// // Compute HMAC
				// decodedSecret, err := base64.StdEncoding.DecodeString(order.Edges.Provider.Edges.APIKey.Secret)
				// if err != nil {
				// 	logger.Errorf("ReassignUnvalidatedLockOrders: %v", err)
				// 	return
				// }
				// decryptedSecret, err := cryptoUtils.DecryptPlain(decodedSecret)
				// if err != nil {
				// 	logger.Errorf("ReassignUnvalidatedLockOrders: %v", err)
				// 	return
				// }

				// payload := map[string]interface{}{}

				// signature := tokenUtils.GenerateHMACSignature(payload, string(decryptedSecret))

				// Send GET request to the provider's node
				res, err := fastshot.NewClient(order.Edges.Provider.HostIdentifier).
					Config().SetTimeout(30 * time.Second).
					// Header().Add("X-Request-Signature", signature).
					Build().GET(fmt.Sprintf("/tx_status/%s/%s", fulfillment.Psp, fulfillment.TxID)).
					Send()
				if err != nil {
					logger.Errorf("ReassignUnvalidatedLockOrders: %v", err)
					continue
				}

				data, err := utils.ParseJSONResponse(res.RawResponse)
				if err != nil {
					logger.Errorf("ReassignUnvalidatedLockOrders: %v %v", err, data)
					continue
				}

				status := data["data"].(map[string]interface{})["status"].(string)

				if status == "failed" {
					_, err = storage.Client.LockOrderFulfillment.
						UpdateOneID(fulfillment.ID).
						SetValidationStatus(lockorderfulfillment.ValidationStatusFailed).
						SetValidationError(data["data"].(map[string]interface{})["error"].(string)).
						Save(ctx)
					if err != nil {
						logger.Errorf("ReassignUnvalidatedLockOrders.UpdateFulfillmentStatusFailed: %v", err)
						continue
					}

					_, err = order.Update().
						SetStatus(lockpaymentorder.StatusFulfilled).
						Save(ctx)
					if err != nil {
						logger.Errorf("ReassignUnvalidatedLockOrders.UpdateOrderStatusFulfilled: %v", err)
						continue
					}

				} else if status == "success" {
					_, err = storage.Client.LockOrderFulfillment.
						UpdateOneID(fulfillment.ID).
						SetValidationStatus(lockorderfulfillment.ValidationStatusSuccess).
						Save(ctx)
					if err != nil {
						logger.Errorf("ReassignUnvalidatedLockOrders.UpdateFulfillmentStatusSuccess: %v", err)
						continue
					}

					transactionLog, err := storage.Client.TransactionLog.
						Create().
						SetStatus(transactionlog.StatusOrderValidated).
						SetNetwork(order.Edges.Token.Edges.Network.Identifier).
						SetMetadata(map[string]interface{}{
							"TransactionID": fulfillment.TxID,
							"PSP":           fulfillment.Psp,
						}).
						Save(ctx)
					if err != nil {
						logger.Errorf("ReassignUnvalidatedLockOrders.CreateTransactionLog: %v", err)
						continue
					}

					_, err = storage.Client.LockPaymentOrder.
						UpdateOneID(order.ID).
						SetStatus(lockpaymentorder.StatusValidated).
						AddTransactions(transactionLog).
						Save(ctx)
					if err != nil {
						logger.Errorf("ReassignUnvalidatedLockOrders.UpdateOrderStatusValidated: %v", err)
						continue
					}
				}

			} else if fulfillment.ValidationStatus == lockorderfulfillment.ValidationStatusFailed {
				if order.Edges.Provider.VisibilityMode != providerprofile.VisibilityModePrivate {
					lockPaymentOrder := types.LockPaymentOrderFields{
						ID:                order.ID,
						Token:             order.Edges.Token,
						GatewayID:         order.GatewayID,
						Amount:            order.Amount,
						Rate:              order.Rate,
						BlockNumber:       order.BlockNumber,
						Institution:       order.Institution,
						AccountIdentifier: order.AccountIdentifier,
						AccountName:       order.AccountName,
						ProviderID:        "",
						Memo:              order.Memo,
						ProvisionBucket:   order.Edges.ProvisionBucket,
					}

					err := services.NewPriorityQueueService().AssignLockPaymentOrder(ctx, lockPaymentOrder)
					if err != nil {
						logger.Errorf("ReassignUnvalidatedLockOrders.AssignLockPaymentOrder: %v", err)
					}
				}
			} else if fulfillment.ValidationStatus == lockorderfulfillment.ValidationStatusSuccess {
				transactionLog, err := storage.Client.TransactionLog.
					Create().
					SetStatus(transactionlog.StatusOrderValidated).
					SetNetwork(order.Edges.Token.Edges.Network.Identifier).
					SetMetadata(map[string]interface{}{
						"TransactionID": fulfillment.TxID,
						"PSP":           fulfillment.Psp,
					}).
					Save(ctx)
				if err != nil {
					logger.Errorf("ReassignUnvalidatedLockOrders.CreateTransactionLog: %v", err)
					continue
				}

				_, err = storage.Client.LockPaymentOrder.
					UpdateOneID(order.ID).
					SetStatus(lockpaymentorder.StatusValidated).
					AddTransactions(transactionLog).
					Save(ctx)
				if err != nil {
					logger.Errorf("ReassignUnvalidatedLockOrders.UpdateOrderStatusValidated: %v", err)
					continue
				}
			}
		}
	}
}

// ReassignStaleOrderRequest reassigns expired order requests to providers
func ReassignStaleOrderRequest(ctx context.Context, orderRequestChan <-chan *redis.Message) {
	for msg := range orderRequestChan {
		key := strings.Split(msg.Payload, "_")
		orderID := key[len(key)-1]

		orderUUID, err := uuid.Parse(orderID)
		if err != nil {
			logger.Errorf("ReassignStaleOrderRequest: %v", err)
			continue
		}

		// Get the order from the database
		order, err := storage.Client.LockPaymentOrder.
			Query().
			Where(
				lockpaymentorder.IDEQ(orderUUID),
			).
			WithProvisionBucket().
			Only(ctx)
		if err != nil {
			logger.Errorf("ReassignStaleOrderRequest: %v", err)
			continue
		}

		orderFields := types.LockPaymentOrderFields{
			ID:                order.ID,
			GatewayID:         order.GatewayID,
			Amount:            order.Amount,
			Rate:              order.Rate,
			BlockNumber:       order.BlockNumber,
			Institution:       order.Institution,
			AccountIdentifier: order.AccountIdentifier,
			AccountName:       order.AccountName,
			Memo:              order.Memo,
			ProvisionBucket:   order.Edges.ProvisionBucket,
		}

		// Assign the order to a provider
		err = services.NewPriorityQueueService().AssignLockPaymentOrder(ctx, orderFields)
		if err != nil {
			logger.Errorf("ReassignStaleOrderRequest.AssignLockPaymentOrder: %v", err)
		}
	}
}

// SubscribeToRedisKeyspaceEvents subscribes to redis keyspace events according to redis.conf settings
func SubscribeToRedisKeyspaceEvents() {
	ctx := context.Background()

	// Handle expired or deleted order request key events
	orderRequest := storage.RedisClient.PSubscribe(
		ctx,
		"__keyevent@0__:expired:order_request_*",
		"__keyevent@0__:del:order_request_*",
	)
	orderRequestChan := orderRequest.Channel()

	go ReassignStaleOrderRequest(ctx, orderRequestChan)
}

// supportedRateCurrencies is the set of fiats we compute a live market rate for.
var supportedRateCurrencies = map[string]bool{
	"KES": true, "NGN": true, "GHS": true, "TZS": true, "UGX": true, "XOF": true,
}

const rateSourceTimeout = 15 * time.Second

// fetchExternalRate returns the live USDT/<fiat> market price aggregated across
// several independent sources — the aggregator's rates API, Binance P2P, and (for NGN)
// Quidax. It takes the MEDIAN of whatever sources respond, so a source being
// down, geo-restricted (Binance P2P is region-gated), or returning an outlier
// never breaks the rate. It errors only when EVERY source fails. There is no
// seeded/fixed fallback — the rate is always live or nothing.
func fetchExternalRate(currency string) (decimal.Decimal, error) {
	currency = strings.ToUpper(currency)
	if !supportedRateCurrencies[currency] {
		return decimal.Zero, fmt.Errorf("fetchExternalRate: currency %s not supported", currency)
	}

	var rates []decimal.Decimal

	// Source 1 — the aggregator aggregator rates API (region-agnostic; itself a
	// multi-source median, so the most reliable single source).
	if r, err := fetchAggregatorRate(currency); err != nil {
		logger.Warnf("fetchExternalRate: aggregator %s: %v", currency, err)
	} else if r.IsPositive() {
		rates = append(rates, r)
	}

	// Source 2 — Binance P2P SELL-ad median (available where Binance P2P is not
	// geo-restricted; empty data there is a soft miss, not a failure).
	if r, err := fetchBinanceP2PRate(currency); err != nil {
		logger.Warnf("fetchExternalRate: binance %s: %v", currency, err)
	} else if r.IsPositive() {
		rates = append(rates, r)
	}

	// Source 3 — Quidax USDT/<fiat> ticker (NGN market).
	if currency == "NGN" {
		if r, err := fetchQuidaxRate(currency); err != nil {
			logger.Warnf("fetchExternalRate: quidax %s: %v", currency, err)
		} else if r.IsPositive() {
			rates = append(rates, r)
		}
	}

	if len(rates) == 0 {
		return decimal.Zero, fmt.Errorf("fetchExternalRate: all sources failed for %s", currency)
	}
	return utils.Median(rates), nil
}

// fetchAggregatorRate reads USDT/<fiat> from the aggregator's public rates API, e.g.
// GET https://api.paycrest.io/v1/rates/USDT/1/NGN -> {"data":"1380"}.
func fetchAggregatorRate(currency string) (decimal.Decimal, error) {
	res, err := fastshot.NewClient("https://api.paycrest.io").
		Config().SetTimeout(rateSourceTimeout).
		Build().GET(fmt.Sprintf("/v1/rates/USDT/1/%s", currency)).
		Retry().Set(2, 3*time.Second).
		Send()
	if err != nil {
		return decimal.Zero, err
	}
	data, err := utils.ParseJSONResponse(res.RawResponse)
	if err != nil {
		return decimal.Zero, err
	}
	raw, ok := data["data"].(string)
	if !ok {
		return decimal.Zero, fmt.Errorf("aggregator: unexpected response shape: %v", data["data"])
	}
	return decimal.NewFromString(raw)
}

// fetchQuidaxRate reads the USDT/<fiat> buy ticker from Quidax. Note the API
// lives on app.quidax.io — the old www.quidax.com host now blackholes requests.
func fetchQuidaxRate(currency string) (decimal.Decimal, error) {
	res, err := fastshot.NewClient("https://app.quidax.io").
		Config().SetTimeout(rateSourceTimeout).
		Build().GET(fmt.Sprintf("/api/v1/markets/tickers/usdt%s", strings.ToLower(currency))).
		Retry().Set(2, 3*time.Second).
		Send()
	if err != nil {
		return decimal.Zero, err
	}
	data, err := utils.ParseJSONResponse(res.RawResponse)
	if err != nil {
		return decimal.Zero, err
	}
	d, ok := data["data"].(map[string]interface{})
	if !ok {
		return decimal.Zero, fmt.Errorf("quidax: unexpected response shape")
	}
	ticker, ok := d["ticker"].(map[string]interface{})
	if !ok {
		return decimal.Zero, fmt.Errorf("quidax: missing ticker")
	}
	buy, ok := ticker["buy"].(string)
	if !ok {
		return decimal.Zero, fmt.Errorf("quidax: missing buy price")
	}
	return decimal.NewFromString(buy)
}

// fetchBinanceP2PRate returns the median of the top USDT/<fiat> SELL ads on
// Binance P2P. The endpoint is public but region-gated: restricted IPs get an
// empty list (success:true, data:[]) — treated here as a soft miss.
func fetchBinanceP2PRate(currency string) (decimal.Decimal, error) {
	res, err := fastshot.NewClient("https://p2p.binance.com").
		Config().SetTimeout(rateSourceTimeout).
		Header().Add("Content-Type", "application/json").
		Build().POST("/bapi/c2c/v2/friendly/c2c/adv/search").
		Retry().Set(2, 3*time.Second).
		Body().AsJSON(map[string]interface{}{
		"asset":             "USDT",
		"fiat":              currency,
		"tradeType":         "SELL",
		"page":              1,
		"rows":              20,
		"payTypes":          []string{},
		"countries":         []string{},
		"proMerchantAds":    false,
		"shieldMerchantAds": false,
		"publisherType":     nil,
	}).
		Send()
	if err != nil {
		return decimal.Zero, err
	}
	resData, err := utils.ParseJSONResponse(res.RawResponse)
	if err != nil {
		return decimal.Zero, err
	}
	data, ok := resData["data"].([]interface{})
	if !ok || len(data) == 0 {
		return decimal.Zero, fmt.Errorf("binance: no ads (region-gated or none available)")
	}
	var prices []decimal.Decimal
	for _, item := range data {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		adv, ok := m["adv"].(map[string]interface{})
		if !ok {
			continue
		}
		priceStr, ok := adv["price"].(string)
		if !ok {
			continue
		}
		if price, err := decimal.NewFromString(priceStr); err == nil {
			prices = append(prices, price)
		}
	}
	if len(prices) == 0 {
		return decimal.Zero, fmt.Errorf("binance: no parseable prices")
	}
	return utils.Median(prices), nil
}

// ComputeMarketRate computes the market price for fiat currencies
func ComputeMarketRate() error {
	ctx := context.Background()

	// Fetch all fiat currencies
	currencies, err := storage.Client.FiatCurrency.
		Query().
		Where(fiatcurrency.IsEnabledEQ(true)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("ComputeMarketRate: %w", err)
	}

	for _, currency := range currencies {
		// The market rate is the live, externally-computed price — never seeded
		// and never derived from provider rates. If every source fails we keep
		// the last good value rather than overwrite it with a bad/zero rate.
		rate, err := fetchExternalRate(currency.Code)
		if err != nil {
			logger.Errorf("ComputeMarketRate: %s: %v", currency.Code, err)
			continue
		}
		if !rate.IsPositive() {
			continue
		}

		if _, err := storage.Client.FiatCurrency.
			UpdateOneID(currency.ID).
			SetMarketRate(rate).
			Save(ctx); err != nil {
			logger.Errorf("ComputeMarketRate: update %s: %v", currency.Code, err)
		}
	}

	return nil
}

// Retry failed webhook notifications
func RetryFailedWebhookNotifications() error {
	ctx := context.Background()

	// Fetch failed webhook notifications that are due for retry
	attempts, err := storage.Client.WebhookRetryAttempt.
		Query().
		Where(
			webhookretryattempt.StatusEQ(webhookretryattempt.StatusFailed),
			webhookretryattempt.NextRetryTimeLTE(time.Now()),
		).
		All(ctx)
	if err != nil {
		return fmt.Errorf("RetryFailedWebhookNotifications: %w", err)
	}

	baseDelay := 2 * time.Minute
	maxCumulativeTime := 24 * time.Hour

	for _, attempt := range attempts {
		// Send the webhook notification
		body, err := fastshot.NewClient(attempt.WebhookURL).
			Config().SetTimeout(30*time.Second).
			Header().Add("X-Rails-Signature", attempt.Signature).
			Build().POST("").
			Body().AsJSON(attempt.Payload).
			Send()

		if err != nil || (body.StatusCode() >= 205) {
			// Webhook notification failed
			// Update attempt with next retry time
			attemptNumber := attempt.AttemptNumber + 1
			delay := baseDelay * time.Duration(math.Pow(2, float64(attemptNumber-1)))

			nextRetryTime := time.Now().Add(delay)

			attemptUpdate := attempt.Update()

			attemptUpdate.
				AddAttemptNumber(1).
				SetNextRetryTime(nextRetryTime)

			// Set status to expired if cumulative time is greater than 24 hours
			if nextRetryTime.Sub(attempt.CreatedAt.Add(-baseDelay)) > maxCumulativeTime {
				attemptUpdate.SetStatus(webhookretryattempt.StatusExpired)
				uid, err := uuid.Parse(attempt.Payload["data"].(map[string]interface{})["senderId"].(string))
				if err != nil {
					return fmt.Errorf("RetryFailedWebhookNotifications.FailedExtraction: %w", err)
				}
				profile, err := storage.Client.SenderProfile.
					Query().
					Where(
						senderprofile.IDEQ(uid),
					).
					WithUser().Only(ctx)
				if err != nil {
					return fmt.Errorf("RetryFailedWebhookNotifications.CouldNotFetchProfile: %w", err)
				}

				_, err = services.SendTemplateEmail(types.SendEmailPayload{
					ToAddress: profile.Edges.User.Email,
					DynamicData: map[string]interface{}{
						"first_name": profile.Edges.User.FirstName,
					},
				}, "d-da75eee4966544ad92dcd060421d4e12")

				if err != nil {
					return fmt.Errorf("RetryFailedWebhookNotifications.SendTemplateEmail: %w", err)
				}
			}

			_, err := attemptUpdate.Save(ctx)
			if err != nil {
				return fmt.Errorf("RetryFailedWebhookNotifications: %w", err)
			}

			continue
		}

		// Webhook notification was successful
		_, err = attempt.Update().
			SetStatus(webhookretryattempt.StatusSuccess).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("RetryFailedWebhookNotifications: %w", err)
		}
	}

	return nil
}

// StartCronJobs starts cron jobs
func StartCronJobs() {
	scheduler := gocron.NewScheduler(time.UTC)
	priorityQueue := services.NewPriorityQueueService()

	// One-time bootstrap.
	if err := ComputeMarketRate(); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}
	if err := priorityQueue.ProcessBucketQueues(); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Sui chain event indexer — long-lived WebSocket subscription to the
	// Gateway package's four Move events (OrderCreated, OrderSettled,
	// OrderRefunded, SenderFeeTransferred). Replaces the legacy EVM/Tron
	// polling jobs (IndexBlockchainEvents) which were
	// removed during the Sui port. The indexer runs as a goroutine for the
	// lifetime of the process; cancel-on-signal would be a server-shutdown
	// concern handled by main.
	suiNetwork := "sui-mainnet"
	if serverConf.Environment != "production" {
		suiNetwork = "sui-testnet"
	}
	// Skip the indexer entirely if the Move package isn't deployed yet —
	// without a package ID it can't subscribe to anything useful, and
	// the block-vision SDK's WS error path will take down the process
	// when handed an HTTPS URL instead of WSS. Lets local dev boot
	// against a non-deployed Gateway.
	if orderConf.SuiGatewayPackageID != "" && (orderConf.SuiWsURL != "" || orderConf.SuiGrpcURL != "") {
		suiIndexer := services.NewSuiEventIndexer(
			orderConf.SuiWsURL,
			orderConf.SuiGrpcURL,
			orderConf.SuiGrpcToken,
			orderConf.SuiGatewayPackageID,
			suiNetwork,
		)
		go func() {
			if err := suiIndexer.Start(context.Background()); err != nil && err != context.Canceled {
				logger.Errorf("StartCronJobs: sui event indexer exited: %v", err)
			}
		}()
	} else {
		if orderConf.SuiGatewayPackageID == "" {
			logger.Infof("StartCronJobs: SUI_GATEWAY_PACKAGE_ID empty — skipping event indexer")
		} else {
			logger.Infof("StartCronJobs: SUI_WS_URL and SUI_GRPC_URL empty — skipping event indexer")
		}
	}

	// Compute market rate every 30 minutes.
	if _, err := scheduler.Cron("*/30 * * * *").Do(ComputeMarketRate); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Refresh provision bucket priority queues every N minutes.
	if _, err := scheduler.Cron(fmt.Sprintf("*/%d * * * *", orderConf.BucketQueueRebuildInterval)).
		Do(priorityQueue.ProcessBucketQueues); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Retry failed webhook notifications every 59 minutes.
	if _, err := scheduler.Cron("*/59 * * * *").Do(RetryFailedWebhookNotifications); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Reassign unvalidated order requests every 2 minutes.
	if _, err := scheduler.Cron("*/2 * * * *").Do(ReassignUnvalidatedLockOrders); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Sui deposit watcher — every minute, scans active SuiReceiveAddress rows
	// for incoming Coin<USDC> deposits, flips status to 'deposited', then
	// forwards via OrderSui.CreateOrder into the Gateway escrow. Path-2
	// (exchange / external wallet) deposit flow only; Path-1 PTB-direct
	// deposits arrive via the SuiEventIndexer's OrderCreated subscription.
	//
	// Same gate as the indexer above — `sui.NewSuiClient` internally
	// initializes a WebSocket subscriber that calls `log.Fatalf` on
	// an https:// URL (block-vision SDK behavior). Without the
	// Gateway deployed the watcher has nothing to do anyway.
	if orderConf.SuiGatewayPackageID != "" {
		depositWatcher := services.NewSuiDepositWatcher()
		if _, err := scheduler.Cron("*/1 * * * *").Do(func() {
			if err := depositWatcher.CheckDeposits(context.Background()); err != nil {
				logger.Errorf("StartCronJobs: sui deposit watcher: %v", err)
			}
		}); err != nil {
			logger.Errorf("StartCronJobs: %v", err)
		}
	} else {
		logger.Infof("StartCronJobs: SUI_GATEWAY_PACKAGE_ID empty — skipping deposit watcher")
	}

	// Route A dispatcher — every minute, advances RouteAOrder rows through
	// pending → bridging → bridged → dispatching → settled via LiFi +
	// settlement Gateway. See docs/route-a-settlement.md.
	routeAD := services.NewRouteADispatcher()
	if _, err := scheduler.Cron("*/1 * * * *").Do(func() {
		if err := routeAD.Tick(context.Background()); err != nil {
			logger.Errorf("StartCronJobs: route-a dispatcher: %v", err)
		}
	}); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Base aggregator wallet low-balance alert — every 5 minutes. Logs
	// Errorf when ETH balance drops below BASE_NATIVE_LOW_THRESHOLD_WEI
	// so ops can top up before createOrder txs start running out of gas.
	if _, err := scheduler.Cron("*/5 * * * *").Do(func() {
		if err := routeAD.CheckNativeBalance(context.Background()); err != nil {
			logger.Errorf("StartCronJobs: route-a native balance check: %v", err)
		}
	}); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Shinami Gas Station fund balance alert — every 5 min. Every
	// aggregator-initiated Move call (CreateOrder, SettleOrder,
	// RefundOrder, DebitCard) is now sponsored by the Shinami fund
	// tied to SHINAMI_GAS_API_KEY. If the fund runs dry, ALL of those
	// stall silently. Threshold: 1 SUI (1_000_000_000 MIST) — generous
	// for a few hundred txs at typical mainnet gas cost.
	if orderConf.ShinamiGasAPIKey != "" {
		gasClient := shinamiGas.New(orderConf.ShinamiGasAPIKey, orderConf.ShinamiGasBaseURL)
		const lowFundThresholdMist = int64(1_000_000_000) // 1 SUI
		if _, err := scheduler.Cron("*/5 * * * *").Do(func() {
			fund, err := gasClient.GetFund(context.Background())
			if err != nil {
				logger.Errorf("StartCronJobs: shinami gas fund check: %v", err)
				return
			}
			if fund.Balance < lowFundThresholdMist {
				balanceDec := decimal.NewFromInt(fund.Balance).Shift(-9)
				inFlightDec := decimal.NewFromInt(fund.InFlight).Shift(-9)
				logger.Errorf("❌ Shinami gas fund LOW — %s (network=%s) balance=%s SUI, in_flight=%s SUI. Top up at depositAddress=%s. Below threshold ALL aggregator Move calls (CreateOrder, SettleOrder, RefundOrder, DebitCard) will start failing.",
					fund.Name, fund.Network, balanceDec.String(), inFlightDec.String(), fund.DepositAddress)
			}
		}); err != nil {
			logger.Errorf("StartCronJobs: %v", err)
		}
	} else {
		logger.Infof("StartCronJobs: SHINAMI_GAS_API_KEY empty — skipping Shinami fund-balance cron (aggregator Move calls will fail at runtime)")
	}

	// Reconcile in-flight Route B fiat payouts as a backstop to the webhook.
	if _, err := scheduler.Cron("*/2 * * * *").Do(ReconcileFiatPayouts); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	scheduler.StartAsync()
}

// ReconcileFiatPayouts polls the BaaS rail for the outcome of in-flight Route B
// payouts (fiat_payout_status=pending with a session id) and converges each lock
// order to a terminal status. It is the backstop to the inbound webhook: if a
// callback is missed, this closes the loop. No-op when the rail is unconfigured.
func ReconcileFiatPayouts() error {
	provider := baas.Default()
	if provider == nil {
		return nil
	}
	ctx := context.Background()

	orders, err := storage.Client.LockPaymentOrder.
		Query().
		Where(
			lockpaymentorder.FiatPayoutStatusEQ(lockpaymentorder.FiatPayoutStatusPending),
			lockpaymentorder.FiatPayoutSessionIDNEQ(""),
		).
		All(ctx)
	if err != nil {
		return fmt.Errorf("ReconcileFiatPayouts: query: %w", err)
	}

	for _, o := range orders {
		tr, err := provider.TransferStatus(ctx, o.FiatPayoutSessionID)
		if err != nil {
			logger.Warnf("ReconcileFiatPayouts %s: status: %v", o.ID, err)
			continue
		}
		switch tr.Status {
		case baas.TransferSuccess:
			if err := storage.Client.LockPaymentOrder.UpdateOneID(o.ID).
				SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusSuccess).
				ClearFiatPayoutSessionID().
				ClearFiatPayoutError().
				Exec(ctx); err != nil {
				logger.Errorf("ReconcileFiatPayouts %s: persist success: %v", o.ID, err)
				continue
			}
			// Fiat confirmed → release the LP's USDC (fulfil + settle).
			if err := orderpkg.NewExecuteOrderService().SettleAfterPayout(ctx, o.ID); err != nil {
				logger.Errorf("ReconcileFiatPayouts %s: settle: %v", o.ID, err)
			}
		case baas.TransferFailed:
			if err := storage.Client.LockPaymentOrder.UpdateOneID(o.ID).
				SetFiatPayoutStatus(lockpaymentorder.FiatPayoutStatusFailed).
				SetFiatPayoutError("rail reported: " + tr.RawStatus).
				ClearFiatPayoutSessionID().
				Exec(ctx); err != nil {
				logger.Errorf("ReconcileFiatPayouts %s: persist failed: %v", o.ID, err)
			}
		default:
			// still pending — keep polling next tick
		}
	}
	return nil
}
