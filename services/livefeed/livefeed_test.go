package livefeed

import "testing"

func TestPublishSubscribe(t *testing.T) {
	b := &Bus{providers: map[string]*providerBus{}}
	ch, _, unsub := b.Subscribe("prov-1", "")
	defer unsub()

	b.Publish("prov-1", "order.assigned", map[string]any{"orderId": "abc"})
	// a different provider must NOT receive it
	b.Publish("prov-2", "order.assigned", map[string]any{"orderId": "zzz"})

	select {
	case ev := <-ch:
		if ev.Name != "order.assigned" || ev.Payload["orderId"] != "abc" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	default:
		t.Fatal("expected an event for prov-1")
	}
	// no second event (prov-2's went elsewhere)
	select {
	case ev := <-ch:
		t.Fatalf("did not expect a second event: %+v", ev)
	default:
	}
}

func TestLastEventIDReplay(t *testing.T) {
	b := &Bus{providers: map[string]*providerBus{}}
	// publish before anyone subscribes — goes into the ring buffer
	b.Publish("p", "order.payout", map[string]any{"n": 1})
	// capture the id of that event to resume after it
	id := b.providers["p"].recent[0].ID
	b.Publish("p", "order.settled", map[string]any{"n": 2})

	_, replay, unsub := b.Subscribe("p", id)
	defer unsub()
	if len(replay) != 1 || replay[0].Name != "order.settled" {
		t.Fatalf("replay = %+v, want only the settled event after %s", replay, id)
	}
}
