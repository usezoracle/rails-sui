package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/usezoracle/rails-sui/ent"
	"github.com/usezoracle/rails-sui/services/livefeed"
	u "github.com/usezoracle/rails-sui/utils"
	"github.com/usezoracle/rails-sui/utils/logger"
)

// Events holds the HTTP connection open as a text/event-stream and forwards
// every live order update scoped to the authed provider — assignment, fiat
// payout status, settlement. The dashboard subscribes here for real-time
// updates instead of polling. Closes on client disconnect or server shutdown.
//
//	GET /v1/provider/events
func (ctrl *ProviderController) Events(ctx *gin.Context) {
	providerCtx, ok := ctx.Get("provider")
	if !ok {
		u.APIResponse(ctx, http.StatusUnauthorized, "error", "Invalid API key or token", nil)
		return
	}
	provider := providerCtx.(*ent.ProviderProfile)

	// SSE headers — set before first flush.
	ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	ctx.Writer.Header().Set("Cache-Control", "no-cache")
	ctx.Writer.Header().Set("Connection", "keep-alive")
	ctx.Writer.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	ctx.Writer.WriteHeader(http.StatusOK)

	flusher, isFlusher := ctx.Writer.(http.Flusher)
	if !isFlusher {
		_, _ = io.WriteString(ctx.Writer, "event: error\ndata: streaming not supported\n\n")
		return
	}

	lastEventID := ctx.GetHeader("Last-Event-ID")
	events, replay, unsubscribe := livefeed.Default().Subscribe(provider.ID, lastEventID)
	defer unsubscribe()

	// First byte so proxies flush headers and don't time out the handshake.
	_, _ = io.WriteString(ctx.Writer, ": connected\n\n")
	flusher.Flush()

	for _, ev := range replay {
		writeProviderSSE(ctx.Writer, ev)
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
			writeProviderSSE(ctx.Writer, ev)
			flusher.Flush()
		}
	}
}

func writeProviderSSE(w io.Writer, ev livefeed.Event) {
	data, err := json.Marshal(ev.Payload)
	if err != nil {
		logger.Errorf("provider SSE marshal: %v", err)
		return
	}
	_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, data)
}
