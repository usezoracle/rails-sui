// sender_events.go is the in-process pub/sub used to fan out
// PaymentOrder lifecycle events to subscribers — chiefly the SSE stream
// at GET /v1/sender/me/payments/stream that the Tapp Merchant app
// listens on while a broadcast is active.
//
// Design:
//   - In-process only. v1 runs as a single Rails process per region;
//     when we shard the API tier we'll swap this for Redis pub/sub
//     (same Publish/Subscribe surface).
//   - Per-sender ring buffer of recent events (capacity 64) so a
//     dropped SSE client can resume via `Last-Event-ID`.
//   - Publishers (the Sui event indexer's settled/refunded handlers)
//     call Publish; subscribers receive on a buffered channel that
//     drops the oldest event on overflow rather than blocking the
//     publisher — slow SSE clients can't stall the indexer.

package services

import (
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PaymentEvent is a single status update the SSE channel forwards
// to the merchant app.
type PaymentEvent struct {
	// "payment.deposited" | "payment.processing" | "payment.fulfilled" |
	// "payment.settled" | "payment.refunded". Treated as opaque by the bus.
	Name string `json:"-"`
	// Stable monotonic ID per sender — combination of unix-ms + order
	// short-id. Stable enough for SSE Last-Event-ID resume.
	ID string `json:"-"`
	// Application payload (JSON-serialized into the SSE `data:` line).
	Payload map[string]any `json:"data"`
	// When the event was published, for buffer GC.
	At time.Time `json:"-"`
}

// senderBus holds the per-sender subscriber list + recent-event ring.
type senderBus struct {
	mu      sync.RWMutex
	subs    map[chan PaymentEvent]struct{}
	recent  []PaymentEvent
	maxRing int
}

// SenderEventBus is the package-level singleton publishers and SSE
// subscribers both talk to.
type SenderEventBus struct {
	mu      sync.RWMutex
	senders map[uuid.UUID]*senderBus
}

var defaultBus = &SenderEventBus{
	senders: make(map[uuid.UUID]*senderBus),
}

// Bus returns the process-wide event bus.
func Bus() *SenderEventBus { return defaultBus }

// Publish fans out an event for one sender. Non-blocking: any
// subscriber whose buffered channel is full silently drops the event,
// keeping the publisher (the indexer goroutine) from stalling on a
// slow SSE consumer.
func (b *SenderEventBus) Publish(senderID uuid.UUID, name string, payload map[string]any) {
	bus := b.bus(senderID)

	ev := PaymentEvent{
		Name:    name,
		ID:      strconv.FormatInt(time.Now().UnixNano()/1e6, 10) + "-" + name,
		Payload: payload,
		At:      time.Now(),
	}

	bus.mu.Lock()
	// Append to recent-event ring, trim if past capacity.
	bus.recent = append(bus.recent, ev)
	if len(bus.recent) > bus.maxRing {
		bus.recent = bus.recent[len(bus.recent)-bus.maxRing:]
	}
	subsCopy := make([]chan PaymentEvent, 0, len(bus.subs))
	for ch := range bus.subs {
		subsCopy = append(subsCopy, ch)
	}
	bus.mu.Unlock()

	for _, ch := range subsCopy {
		select {
		case ch <- ev:
		default:
			// Slow consumer — drop the event for this subscriber.
		}
	}
}

// Subscribe registers a new subscriber for one sender. The returned
// channel receives all subsequent events; `replay` contains any events
// from the ring buffer whose ID sorts strictly after lastEventID
// (empty string means "no resume; only future events").
//
// The caller MUST invoke the returned unsubscribe func when done
// (e.g. on SSE disconnect) — leaking subscribers leaks goroutine
// memory in the publisher closure.
func (b *SenderEventBus) Subscribe(senderID uuid.UUID, lastEventID string) (<-chan PaymentEvent, []PaymentEvent, func()) {
	bus := b.bus(senderID)
	ch := make(chan PaymentEvent, 16)

	bus.mu.Lock()
	bus.subs[ch] = struct{}{}
	var replay []PaymentEvent
	if lastEventID != "" {
		for _, ev := range bus.recent {
			if ev.ID > lastEventID {
				replay = append(replay, ev)
			}
		}
	}
	bus.mu.Unlock()

	return ch, replay, func() {
		bus.mu.Lock()
		delete(bus.subs, ch)
		bus.mu.Unlock()
		close(ch)
	}
}

func (b *SenderEventBus) bus(senderID uuid.UUID) *senderBus {
	b.mu.RLock()
	if bus, ok := b.senders[senderID]; ok {
		b.mu.RUnlock()
		return bus
	}
	b.mu.RUnlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	if bus, ok := b.senders[senderID]; ok {
		return bus
	}
	bus := &senderBus{
		subs:    make(map[chan PaymentEvent]struct{}),
		maxRing: 64,
	}
	b.senders[senderID] = bus
	return bus
}
