// Package admin holds operator-facing endpoints for inspecting and
// nudging Route A orders. All routes here are gated by the
// `X-Admin-Token` header via cards.AdminTokenMiddleware (the same
// shared-secret check used for card admin endpoints — to be replaced
// with proper RBAC + audit trail before this moves beyond ops).
//
// Phase 1 ships read-only endpoints (GET event timeline). Phase 6
// will add write endpoints (force-state, retry, refund) which all
// log to route_a_events with actor=operator.
//
// See docs/route-a-hardening.md.
package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/ent/routeaevent"
	"github.com/usezoracle/rails-sui/ent/routeaorder"
	"github.com/usezoracle/rails-sui/storage"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// RouteAController exposes operator endpoints for Route A orders.
type RouteAController struct{}

// NewRouteAController constructs the controller.
func NewRouteAController() *RouteAController {
	return &RouteAController{}
}

// orderEventDTO is the per-row payload returned to the operator —
// thin wrapper over the ent row so we control the JSON shape.
type orderEventDTO struct {
	ID            int            `json:"id"`
	Step          string         `json:"step"`
	Status        string         `json:"status"`
	Actor         string         `json:"actor"`
	At            string         `json:"at"`
	DurationMS    *int64         `json:"duration_ms,omitempty"`
	Payload       map[string]any `json:"payload,omitempty"`
	ErrorMsg      string         `json:"error_msg,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
}

// orderEventsDTO is the full response: current row state + the
// chronological audit trail. Designed so an operator can answer
// "what's the order doing right now?" and "what did it do to get
// there?" with one request.
type orderEventsDTO struct {
	OrderID          int             `json:"order_id"`
	PaymentOrderID   string          `json:"payment_order_id,omitempty"`
	Mode             string          `json:"mode"`
	BridgeStatus     string          `json:"bridge_status"`
	BridgeTxSui      string          `json:"bridge_tx_sui,omitempty"`
	BridgeTxDest     string          `json:"bridge_tx_dest,omitempty"`
	LiFiTool         string          `json:"lifi_tool,omitempty"`
	GatewayOrderID   string          `json:"gateway_order_id,omitempty"`
	SettlementStatus string          `json:"settlement_status,omitempty"`
	FailureReason    string          `json:"failure_reason,omitempty"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
	Events           []orderEventDTO `json:"events"`
}

// GetOrderEvents returns the full audit trail for a Route A order.
//
//	GET /v1/admin/route-a/orders/:id/events
//
// :id is the route_a_orders.id (integer). Returns 404 if the order
// doesn't exist; events array is empty for never-instrumented older
// orders (those predate Phase 1 — read on-chain history instead).
func (ctrl *RouteAController) GetOrderEvents(ctx *gin.Context) {
	idStr := ctx.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error",
			"order id must be an integer", nil)
		return
	}

	order, err := storage.Client.RouteAOrder.
		Query().
		Where(routeaorder.IDEQ(id)).
		WithPaymentOrder().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error",
				"route_a_order not found", nil)
			return
		}
		logger.Errorf("admin GetOrderEvents: load order %d: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"failed to load order", nil)
		return
	}

	events, err := storage.Client.RouteAEvent.
		Query().
		Where(routeaevent.HasRouteAOrderWith(routeaorder.IDEQ(id))).
		Order(ent.Asc(routeaevent.FieldAt), ent.Asc(routeaevent.FieldID)).
		All(ctx)
	if err != nil {
		logger.Errorf("admin GetOrderEvents: load events for order %d: %v", id, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error",
			"failed to load events", nil)
		return
	}

	dto := orderEventsDTO{
		OrderID:          order.ID,
		Mode:             string(order.Mode),
		BridgeStatus:     string(order.BridgeStatus),
		BridgeTxSui:      order.BridgeTxSui,
		BridgeTxDest:     order.BridgeTxDest,
		LiFiTool:         order.LifiTool,
		GatewayOrderID:   order.GatewayOrderID,
		SettlementStatus: order.SettlementStatus,
		FailureReason:    order.FailureReason,
		CreatedAt:        order.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:        order.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Events:           make([]orderEventDTO, 0, len(events)),
	}
	if order.Edges.PaymentOrder != nil {
		dto.PaymentOrderID = order.Edges.PaymentOrder.ID.String()
	}
	for _, e := range events {
		ev := orderEventDTO{
			ID:            e.ID,
			Step:          string(e.Step),
			Status:        string(e.Status),
			Actor:         string(e.Actor),
			At:            e.At.Format("2006-01-02T15:04:05.000Z07:00"),
			Payload:       e.Payload,
			CorrelationID: e.CorrelationID,
		}
		if e.DurationMs != nil {
			ev.DurationMS = e.DurationMs
		}
		ev.ErrorMsg = e.ErrorMsg
		dto.Events = append(dto.Events, ev)
	}

	u.APIResponse(ctx, http.StatusOK, "success", "ok", dto)
}

// forceStateBody is the required POST body for ForceState.
type forceStateBody struct {
	State         string `json:"state" binding:"required"`
	Justification string `json:"justification" binding:"required"`
}

// ForceState manually transitions an order's bridge_status to a chosen
// value. Operator-only — every call writes a `manual_override` audit
// row with the operator's justification so the timeline reflects it.
//
//	POST /v1/admin/route-a/orders/:id/force-state
//	body: {"state": "<new>", "justification": "<why>"}
//
// Use for break-glass scenarios — e.g., bouncing a stuck `failed` row
// back to `bridged` after a manual late-bridge recovery, or marking
// `refunded` after an out-of-band sweep. NEVER use to skip steps the
// pipeline can complete on its own.
func (ctrl *RouteAController) ForceState(ctx *gin.Context) {
	idStr := ctx.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "order id must be an integer", nil)
		return
	}
	var body forceStateBody
	if err := ctx.ShouldBindJSON(&body); err != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", "missing state or justification", nil)
		return
	}
	newState, validErr := parseBridgeStatus(body.State)
	if validErr != nil {
		u.APIResponse(ctx, http.StatusBadRequest, "error", validErr.Error(), nil)
		return
	}

	order, err := storage.Client.RouteAOrder.Query().
		Where(routeaorder.IDEQ(id)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			u.APIResponse(ctx, http.StatusNotFound, "error", "route_a_order not found", nil)
			return
		}
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to load order", nil)
		return
	}
	prev := string(order.BridgeStatus)

	if _, err := order.Update().
		SetBridgeStatus(newState).
		SetFailureReason("manual override: " + body.Justification).
		Save(ctx); err != nil {
		logger.Errorf("admin ForceState: persist %d → %s: %v", id, newState, err)
		u.APIResponse(ctx, http.StatusInternalServerError, "error", "failed to persist new state", nil)
		return
	}

	// Audit row. Use services.LogOnce via a tiny adapter to avoid a
	// services-package import cycle from controllers — just write the
	// row directly with the canonical helper signature.
	writeManualOverrideEvent(ctx, id, prev, string(newState), body.Justification)

	u.APIResponse(ctx, http.StatusOK, "success", "state updated", gin.H{
		"order_id":      id,
		"previous":      prev,
		"current":       string(newState),
		"justification": body.Justification,
	})
}

// parseBridgeStatus validates a string is one of the allowed enum
// values. Rejects unknown values so a typo can't put an order into a
// state the dispatcher doesn't recognize.
func parseBridgeStatus(s string) (routeaorder.BridgeStatus, error) {
	switch routeaorder.BridgeStatus(s) {
	case routeaorder.BridgeStatusPending,
		routeaorder.BridgeStatusAwaitingFunds,
		routeaorder.BridgeStatusBridging,
		routeaorder.BridgeStatusBridgeUncertain,
		routeaorder.BridgeStatusBridged,
		routeaorder.BridgeStatusDispatching,
		routeaorder.BridgeStatusSettled,
		routeaorder.BridgeStatusFailed,
		routeaorder.BridgeStatusRefunded:
		return routeaorder.BridgeStatus(s), nil
	}
	return "", fmt.Errorf("invalid bridge_status %q", s)
}

// writeManualOverrideEvent appends the audit row. Imports routeaevent
// directly (rather than the services package) so this controller has
// no cycle risk with services/route_a_events.go.
func writeManualOverrideEvent(ctx *gin.Context, orderID int, prev, next, justification string) {
	if _, err := storage.Client.RouteAEvent.
		Create().
		SetStep(routeaevent.StepManualOverride).
		SetStatus(routeaevent.StatusSucceeded).
		SetActor(routeaevent.ActorOperator).
		SetAt(time.Now()).
		SetPayload(map[string]any{
			"previous_state": prev,
			"new_state":      next,
			"justification":  justification,
			"remote_addr":    ctx.ClientIP(),
		}).
		SetRouteAOrderID(orderID).
		Save(ctx); err != nil {
		logger.Errorf("admin: write manual_override event for order %d: %v", orderID, err)
	}
}
