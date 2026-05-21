package tasks

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
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/providerordertoken"
	"github.com/usezoracle/rails-sui/ent/providerprofile"
	"github.com/usezoracle/rails-sui/ent/senderprofile"
	"github.com/usezoracle/rails-sui/ent/transactionlog"
	"github.com/usezoracle/rails-sui/ent/webhookretryattempt"
	"github.com/usezoracle/rails-sui/services"
	orderService "github.com/usezoracle/rails-sui/services/order"
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

	// Unassign unfulfilled lock orders.
	_, err := storage.Client.LockPaymentOrder.
		Update().
		Where(
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

	// Query unvalidated lock orders.
	lockOrders, err := storage.Client.LockPaymentOrder.
		Query().
		Where(
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

func FixDatabaseMisHap() error {
	ctx := context.Background()

	// parse string to uuid
	orderUUID, err := uuid.Parse("14baa582-84d9-40bf-96b8-94601d6ffe2b")
	if err != nil {
		logger.Errorf("FixDatabaseMisHap: %v", err)
		return nil
	}

	order, err := storage.Client.PaymentOrder.
		Query().
		Where(
			paymentorder.IDEQ(orderUUID),
			paymentorder.StatusEQ(paymentorder.StatusInitiated),
		).
		WithToken(func(tq *ent.TokenQuery) {
			tq.WithNetwork()
		}).
		WithRecipient().
		Only(ctx)
	if err != nil {
		logger.Errorf("FixDatabaseMisHap: %v", err)
	}

	service := orderService.NewOrderSui()
	err = service.CreateOrder(ctx, order.ID)
	if err != nil {
		logger.Errorf("FixDatabaseMisHap: %v", err)
	}

	return nil
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

// fetchExternalRate fetches the external rate for a fiat currency
func fetchExternalRate(currency string) (decimal.Decimal, error) {
	currency = strings.ToUpper(currency)
	supportedCurrencies := []string{"KES", "NGN", "GHS", "TZS", "UGX", "XOF"}
	isSupported := false
	for _, supported := range supportedCurrencies {
		if currency == supported {
			isSupported = true
			break
		}
	}
	if !isSupported {
		return decimal.Zero, fmt.Errorf("ComputeMarketRate: currency not supported")
	}

	// Fetch rates from third-party APIs
	var price decimal.Decimal
	if currency == "NGN" {
		res, err := fastshot.NewClient("https://www.quidax.com").
			Config().SetTimeout(30*time.Second).
			Build().GET(fmt.Sprintf("/api/v1/markets/tickers/usdt%s", strings.ToLower(currency))).
			Retry().Set(3, 5*time.Second).
			Send()
		if err != nil {
			return decimal.Zero, fmt.Errorf("ComputeMarketRate: %w", err)
		}

		data, err := utils.ParseJSONResponse(res.RawResponse)
		if err != nil {
			return decimal.Zero, fmt.Errorf("ComputeMarketRate: %w %v", err, data)
		}

		price, err = decimal.NewFromString(data["data"].(map[string]interface{})["ticker"].(map[string]interface{})["buy"].(string))
		if err != nil {
			return decimal.Zero, fmt.Errorf("ComputeMarketRate: %w", err)
		}
	} else {
		res, err := fastshot.NewClient("https://p2p.binance.com").
			Config().SetTimeout(30*time.Second).
			Header().Add("Content-Type", "application/json").
			Build().POST("/bapi/c2c/v2/friendly/c2c/adv/search").
			Retry().Set(3, 5*time.Second).
			Body().AsJSON(map[string]interface{}{
			"asset":     "USDT",
			"fiat":      currency,
			"tradeType": "SELL",
			"page":      1,
			"rows":      20,
		}).
			Send()
		if err != nil {
			return decimal.Zero, fmt.Errorf("ComputeMarketRate: %w", err)
		}

		resData, err := utils.ParseJSONResponse(res.RawResponse)
		if err != nil {
			return decimal.Zero, fmt.Errorf("ComputeMarketRate: %w", err)
		}

		// Access the data array
		data, ok := resData["data"].([]interface{})
		if !ok || len(data) == 0 {
			return decimal.Zero, fmt.Errorf("ComputeMarketRate: No data in the response")
		}

		// Loop through the data array and extract prices
		var prices []decimal.Decimal
		for _, item := range data {
			adv, ok := item.(map[string]interface{})["adv"].(map[string]interface{})
			if !ok {
				continue
			}

			price, err := decimal.NewFromString(adv["price"].(string))
			if err != nil {
				continue
			}

			prices = append(prices, price)
		}

		// Calculate and return the median
		price = utils.Median(prices)
	}

	return price, nil
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
		// Fetch external rate
		externalRate, err := fetchExternalRate(currency.Code)
		if err != nil {
			continue
		}

		// Fetch rates from token configs with fixed conversion rate
		tokenConfigs, err := storage.Client.ProviderOrderToken.
			Query().
			Where(
				providerordertoken.SymbolIn("USDT", "USDC"),
				providerordertoken.ConversionRateTypeEQ(providerordertoken.ConversionRateTypeFixed),
			).
			Select(providerordertoken.FieldFixedConversionRate).
			All(ctx)
		if err != nil {
			continue
		}

		var rates []decimal.Decimal
		for _, tokenConfig := range tokenConfigs {
			rates = append(rates, tokenConfig.FixedConversionRate)
		}

		// Calculate median
		median := utils.Median(rates)

		// Check the median rate against the external rate to ensure it's not too far off
		percentDeviation := utils.AbsPercentageDeviation(externalRate, median)
		if percentDeviation.GreaterThan(orderConf.PercentDeviationFromExternalRate) {
			median = externalRate
		}

		// Update currency with median rate
		_, err = storage.Client.FiatCurrency.
			UpdateOneID(currency.ID).
			SetMarketRate(median).
			Save(ctx)
		if err != nil {
			continue
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
	// polling jobs (IndexBlockchainEvents, IndexLinkedAddresses) which were
	// removed during the Sui port. The indexer runs as a goroutine for the
	// lifetime of the process; cancel-on-signal would be a server-shutdown
	// concern handled by main.
	suiNetwork := "sui-mainnet"
	if serverConf.Environment != "production" {
		suiNetwork = "sui-testnet"
	}
	suiIndexer := services.NewSuiEventIndexer(orderConf.SuiRpcURL, orderConf.SuiGatewayPackageID, suiNetwork)
	go func() {
		if err := suiIndexer.Start(context.Background()); err != nil && err != context.Canceled {
			logger.Errorf("StartCronJobs: sui event indexer exited: %v", err)
		}
	}()

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
	depositWatcher := services.NewSuiDepositWatcher()
	if _, err := scheduler.Cron("*/1 * * * *").Do(func() {
		if err := depositWatcher.CheckDeposits(context.Background()); err != nil {
			logger.Errorf("StartCronJobs: sui deposit watcher: %v", err)
		}
	}); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	// Route A dispatcher — every minute, advances RouteAOrder rows through
	// pending → bridging → bridged → dispatching → settled via LiFi. See
	// docs/route-a-spec.md.
	routeAD := services.NewRouteADispatcher()
	if _, err := scheduler.Cron("*/1 * * * *").Do(func() {
		if err := routeAD.Tick(context.Background()); err != nil {
			logger.Errorf("StartCronJobs: route-a dispatcher: %v", err)
		}
	}); err != nil {
		logger.Errorf("StartCronJobs: %v", err)
	}

	scheduler.StartAsync()
}
