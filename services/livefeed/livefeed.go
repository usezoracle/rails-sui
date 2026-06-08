// Package livefeed is the in-process pub/sub that fans out live LP order
// updates (assignment, payout status, settlement) to the provider dashboard's
// SSE stream at GET /v1/provider/events.
//
// It mirrors the sender event bus but is keyed by the provider's string ID and
// is a dependency-free leaf package, so the order service, the indexer, the
// webhook controller, and the reconcile cron can all publish to it without
// import cycles.
//
// Design:
//   - In-process only (single process per region). Swap for Redis pub/sub when
//     the API tier is sharded — same Publish/Subscribe surface.
//   - Per-provider ring buffer (capacity 64) so a reconnecting SSE client can
//     resume via Last-Event-ID.
//   - Non-blocking publish: a slow subscriber drops the oldest event rather
//     than stalling the publisher.
package livefeed

import (
	"strconv"
	"sync"
	"time"
)

// Event is one live update forwarded to the provider dashboard.
type Event struct {
	Name    string         // e.g. "order.assigned" | "order.payout" | "order.settled"
	ID      string         // monotonic-ish id for Last-Event-ID resume
	Payload map[string]any // JSON-serialized into the SSE data: line
	At      time.Time
}

type providerBus struct {
	mu      sync.RWMutex
	subs    map[chan Event]struct{}
	recent  []Event
	maxRing int
}

// Bus is the process-wide provider event bus.
type Bus struct {
	mu        sync.RWMutex
	providers map[string]*providerBus
}

var defaultBus = &Bus{providers: make(map[string]*providerBus)}

// Default returns the process-wide bus.
func Default() *Bus { return defaultBus }

// Publish fans out an event for one provider. Non-blocking.
func (b *Bus) Publish(providerID, name string, payload map[string]any) {
	if providerID == "" {
		return
	}
	pb := b.bus(providerID)

	ev := Event{
		Name:    name,
		ID:      strconv.FormatInt(time.Now().UnixNano()/1e6, 10) + "-" + name,
		Payload: payload,
		At:      time.Now(),
	}

	pb.mu.Lock()
	pb.recent = append(pb.recent, ev)
	if len(pb.recent) > pb.maxRing {
		pb.recent = pb.recent[len(pb.recent)-pb.maxRing:]
	}
	subsCopy := make([]chan Event, 0, len(pb.subs))
	for ch := range pb.subs {
		subsCopy = append(subsCopy, ch)
	}
	pb.mu.Unlock()

	for _, ch := range subsCopy {
		select {
		case ch <- ev:
		default:
			// slow consumer — drop for this subscriber
		}
	}
}

// Subscribe registers a subscriber for one provider. Returns the live channel,
// any buffered events newer than lastEventID, and an unsubscribe func the caller
// MUST invoke on disconnect.
func (b *Bus) Subscribe(providerID, lastEventID string) (<-chan Event, []Event, func()) {
	pb := b.bus(providerID)
	ch := make(chan Event, 16)

	pb.mu.Lock()
	pb.subs[ch] = struct{}{}
	var replay []Event
	if lastEventID != "" {
		for _, ev := range pb.recent {
			if ev.ID > lastEventID {
				replay = append(replay, ev)
			}
		}
	}
	pb.mu.Unlock()

	return ch, replay, func() {
		pb.mu.Lock()
		delete(pb.subs, ch)
		pb.mu.Unlock()
		close(ch)
	}
}

func (b *Bus) bus(providerID string) *providerBus {
	b.mu.RLock()
	if pb, ok := b.providers[providerID]; ok {
		b.mu.RUnlock()
		return pb
	}
	b.mu.RUnlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	if pb, ok := b.providers[providerID]; ok {
		return pb
	}
	pb := &providerBus{subs: make(map[chan Event]struct{}), maxRing: 64}
	b.providers[providerID] = pb
	return pb
}
