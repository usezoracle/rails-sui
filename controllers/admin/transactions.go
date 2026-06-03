package admin

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/lockpaymentorder"
	"github.com/usezoracle/rails-sui/ent/paymentorder"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// TransactionsController exposes the operator transaction-timeline endpoints —
// a single, fully-detailed chronological view of every step an order passed
// through, merged across all subsystems (order status, transaction log, Route A
// bridge events, Route B lock order + fulfillments). Read-only.
type TransactionsController struct{}

// NewTransactionsController constructs the controller.
func NewTransactionsController() *TransactionsController { return &TransactionsController{} }

const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// timelineStep is one fully-detailed lifecycle step from any source. Every
// subsystem contributes its steps so nothing is missed.
type timelineStep struct {
	Source  string         `json:"source"` // payment_order | transaction_log | route_a | lock_order | fulfillment
	Step    string         `json:"step"`
	Status  string         `json:"status"` // ok | failed | pending | info
	Actor   string         `json:"actor,omitempty"`
	At      string         `json:"at"`
	Network string         `json:"network,omitempty"`
	TxHash  string         `json:"tx_hash,omitempty"`
	Error   string         `json:"error,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// transactionDetail is the full lifecycle of one payment order.
type transactionDetail struct {
	OrderID    string         `json:"order_id"`
	Status     string         `json:"status"`
	Route      string         `json:"route"` // A | B
	Amount     string         `json:"amount"`
	AmountPaid string         `json:"amount_paid"`
	Rate       string         `json:"rate"`
	Token      string         `json:"token,omitempty"`
	GatewayID  string         `json:"gateway_id,omitempty"`
	TxHash     string         `json:"tx_hash,omitempty"`
	Recipient  map[string]any `json:"recipient,omitempty"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	StepCount  int            `json:"step_count"`
	Steps      []timelineStep `json:"steps"`
}

// GetTransactions lists payment orders, newest first, paginated. Optional
// ?status= filter (initiated|pending|expired|cancelled|settled|refunded).
//
//	GET /v1/admin/transactions?page=&limit=&status=
func (c *TransactionsController) GetTransactions(ctx *gin.Context) {
	_, offset, limit := u.Paginate(ctx)

	q := storage.Client.PaymentOrder.Query().WithToken().WithRouteAOrder()
	if s := ctx.Query("status"); s != "" {
		q = q.Where(paymentorder.StatusEQ(paymentorder.Status(s)))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		logger.Errorf("admin GetTransactions: count: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to count orders", nil)
		return
	}
	orders, err := q.Order(ent.Desc(paymentorder.FieldCreatedAt)).Offset(offset).Limit(limit).All(ctx)
	if err != nil {
		logger.Errorf("admin GetTransactions: query: %v", err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load orders", nil)
		return
	}

	rows := make([]gin.H, 0, len(orders))
	for _, o := range orders {
		route := "B"
		if o.Edges.RouteAOrder != nil {
			route = "A"
		}
		token := ""
		if o.Edges.Token != nil {
			token = o.Edges.Token.Symbol
		}
		rows = append(rows, gin.H{
			"order_id":   o.ID.String(),
			"status":     string(o.Status),
			"route":      route,
			"amount":     o.Amount.String(),
			"rate":       o.Rate.String(),
			"token":      token,
			"gateway_id": o.GatewayID,
			"created_at": o.CreatedAt.Format(tsLayout),
		})
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", gin.H{
		"total":        total,
		"count":        len(rows),
		"transactions": rows,
	})
}

// GetTransactionTimeline returns every lifecycle step of one order, fully
// detailed and chronological — across order status, transaction log, Route A
// events, and Route B lock order + fulfillments.
//
//	GET /v1/admin/transactions/:id   (id = payment_order UUID)
func (c *TransactionsController) GetTransactionTimeline(ctx *gin.Context) {
	id, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "order id must be a uuid", nil)
		return
	}

	o, err := storage.Client.PaymentOrder.Query().
		Where(paymentorder.IDEQ(id)).
		WithToken().
		WithRecipient().
		WithRouteAOrder().
		WithTransactions().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "payment order not found", nil)
			return
		}
		logger.Errorf("admin GetTransactionTimeline: load %s: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load order", nil)
		return
	}

	steps := make([]timelineStep, 0, 16)

	// 1. Order-level current milestone.
	steps = append(steps, timelineStep{
		Source: "payment_order",
		Step:   "order_" + string(o.Status),
		Status: statusKind(string(o.Status)),
		At:     o.UpdatedAt.Format(tsLayout),
		Detail: map[string]any{
			"amount":          o.Amount.String(),
			"amount_paid":     o.AmountPaid.String(),
			"percent_settled": o.PercentSettled.String(),
		},
	})

	// 2. Transaction-log steps (the canonical lifecycle points).
	for _, tl := range o.Edges.Transactions {
		steps = append(steps, timelineStep{
			Source:  "transaction_log",
			Step:    string(tl.Status),
			Status:  "info",
			At:      tl.CreatedAt.Format(tsLayout),
			Network: tl.Network,
			TxHash:  tl.TxHash,
			Detail:  tl.Metadata,
		})
	}

	// 3. Route A bridge events (fine-grained: quote, bridge submit/poll/done,
	//    dispatch, settlement, manual overrides).
	route := "B"
	if ra := o.Edges.RouteAOrder; ra != nil {
		route = "A"
		evs, err := storage.Client.RouteAEvent.Query().
			Where(routeaevent.HasRouteAOrderWith(routeaorder.IDEQ(ra.ID))).
			Order(ent.Asc(routeaevent.FieldAt)).
			All(ctx)
		if err != nil {
			logger.Errorf("admin GetTransactionTimeline: route_a events %s: %v", id, err)
		}
		for _, e := range evs {
			steps = append(steps, timelineStep{
				Source: "route_a",
				Step:   string(e.Step),
				Status: string(e.Status),
				Actor:  string(e.Actor),
				At:     e.At.Format(tsLayout),
				Error:  e.ErrorMsg,
				Detail: e.Payload,
			})
		}
	}

	// 4. Route B lock order + provider fulfillments (matched by gateway_id).
	if o.GatewayID != "" {
		lpo, err := storage.Client.LockPaymentOrder.Query().
			Where(lockpaymentorder.GatewayIDEQ(o.GatewayID)).
			WithFulfillments().
			WithProvider().
			First(ctx)
		if err == nil && lpo != nil {
			route = "B"
			provider := ""
			if lpo.Edges.Provider != nil {
				provider = lpo.Edges.Provider.TradingName
			}
			steps = append(steps, timelineStep{
				Source: "lock_order",
				Step:   "lock_" + string(lpo.Status),
				Status: statusKind(string(lpo.Status)),
				At:     lpo.UpdatedAt.Format(tsLayout),
				TxHash: lpo.TxHash,
				Detail: map[string]any{
					"provider":    provider,
					"institution": lpo.Institution,
					"account":     lpo.AccountIdentifier,
				},
			})
			for _, f := range lpo.Edges.Fulfillments {
				steps = append(steps, timelineStep{
					Source: "fulfillment",
					Step:   "fulfillment_" + string(f.ValidationStatus),
					Status: statusKind(string(f.ValidationStatus)),
					At:     f.CreatedAt.Format(tsLayout),
					TxHash: f.TxID,
					Error:  f.ValidationError,
					Detail: map[string]any{"psp": f.Psp},
				})
			}
		}
	}

	// Chronological order (RFC3339-style strings sort lexicographically by time).
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].At < steps[j].At })

	detail := transactionDetail{
		OrderID:    o.ID.String(),
		Status:     string(o.Status),
		Route:      route,
		Amount:     o.Amount.String(),
		AmountPaid: o.AmountPaid.String(),
		Rate:       o.Rate.String(),
		GatewayID:  o.GatewayID,
		TxHash:     o.TxHash,
		CreatedAt:  o.CreatedAt.Format(tsLayout),
		UpdatedAt:  o.UpdatedAt.Format(tsLayout),
		StepCount:  len(steps),
		Steps:      steps,
	}
	if o.Edges.Token != nil {
		detail.Token = o.Edges.Token.Symbol
	}
	if r := o.Edges.Recipient; r != nil {
		detail.Recipient = map[string]any{
			"institution":        r.Institution,
			"account_identifier": r.AccountIdentifier,
			"account_name":       r.AccountName,
		}
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", detail)
}

// statusKind maps a status/validation string to a coarse kind for UI coloring.
func statusKind(s string) string {
	switch s {
	case "settled", "fulfilled", "validated", "succeeded", "success", "bridged":
		return "ok"
	case "failed", "cancelled", "expired", "refunded":
		return "failed"
	case "pending", "processing", "initiated", "bridging", "dispatching", "awaiting_funds", "bridge_uncertain":
		return "pending"
	}
	return "info"
}
