package capture

import (
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

func TestEventHubReplayAndLiveOnly(t *testing.T) {
	h := NewEventHub()
	for i := 0; i < 3; i++ {
		h.Emit(&shared.Event{Type: shared.EventRequestStarted, Timestamp: time.Unix(int64(i), 0).UTC()})
	}
	live := h.Subscribe()
	defer h.Unsubscribe(live)
	select {
	case ev := <-live:
		t.Fatalf("live subscribe replayed %+v", ev)
	default:
	}

	sub := h.SubscribeAfter(1)
	defer h.Unsubscribe(sub)
	var got []int64
	for len(got) < 2 {
		select {
		case ev := <-sub:
			got = append(got, ev.ID)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay")
		}
	}
	if got[0] != 2 || got[1] != 3 {
		t.Fatalf("replay = %v", got)
	}

	h.Emit(&shared.Event{Type: shared.EventScopeChanged, Timestamp: time.Now().UTC()})
	select {
	case ev := <-sub:
		if ev.ID != 4 || ev.Type != shared.EventScopeChanged {
			t.Fatalf("live tail = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
}

func TestEventHubRingDropsOldest(t *testing.T) {
	h := NewEventHub()
	for i := 0; i < eventRingCap+5; i++ {
		h.Emit(&shared.Event{Type: shared.EventRequestStarted})
	}
	sub := h.SubscribeAfter(0)
	defer h.Unsubscribe(sub)
	first := <-sub
	if first.ID != int64(h.next-eventRingCap+1) {
		t.Fatalf("oldest retained id = %d, hub next = %d", first.ID, h.next)
	}
}
