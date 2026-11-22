package capture

import (
	"sync"

	"github.com/lidessen/crabbro/internal/shared"
)

const eventRingCap = 512

// subscriber represents a single event subscription.
type subscriber struct {
	ch    chan *shared.Event
	close sync.Once
}

// EventHub broadcasts events and retains a short ring so a client can resume
// after a dropped stream. It is safe for concurrent use.
type EventHub struct {
	mu          sync.Mutex
	next        int64
	ring        []*shared.Event
	subscribers []*subscriber
}

// NewEventHub creates a new EventHub.
func NewEventHub() *EventHub {
	return &EventHub{}
}

// Subscribe creates a live-only subscription.
func (eh *EventHub) Subscribe() <-chan *shared.Event {
	return eh.SubscribeAfter(-1)
}

// SubscribeAfter replays retained events with ID > after when after >= 0,
// then delivers live events. A negative after is live-only.
func (eh *EventHub) SubscribeAfter(after int64) <-chan *shared.Event {
	eh.mu.Lock()
	defer eh.mu.Unlock()

	sub := &subscriber{ch: make(chan *shared.Event, eventRingCap)}
	if after >= 0 {
		for _, ev := range eh.sinceLocked(after) {
			sub.ch <- ev
		}
	}
	eh.subscribers = append(eh.subscribers, sub)
	return sub.ch
}

// Unsubscribe removes a subscription and closes its channel.
func (eh *EventHub) Unsubscribe(ch <-chan *shared.Event) {
	eh.mu.Lock()
	defer eh.mu.Unlock()

	for i, sub := range eh.subscribers {
		if sub.ch == ch {
			sub.close.Do(func() { close(sub.ch) })
			eh.subscribers = append(eh.subscribers[:i], eh.subscribers[i+1:]...)
			return
		}
	}
}

// Emit assigns a monotonic ID, stores the event, and broadcasts it.
func (eh *EventHub) Emit(event *shared.Event) {
	if event == nil {
		return
	}
	eh.mu.Lock()
	defer eh.mu.Unlock()

	eh.next++
	cp := *event
	cp.ID = eh.next
	event.ID = cp.ID
	if eh.ring == nil {
		eh.ring = make([]*shared.Event, eventRingCap)
	}
	stored := cp
	eh.ring[int((eh.next-1)%eventRingCap)] = &stored

	for _, sub := range eh.subscribers {
		select {
		case sub.ch <- &stored:
		default:
		}
	}
}

// Len returns the number of active subscribers.
func (eh *EventHub) Len() int {
	eh.mu.Lock()
	defer eh.mu.Unlock()
	return len(eh.subscribers)
}

func (eh *EventHub) sinceLocked(after int64) []*shared.Event {
	if eh.next == 0 || after >= eh.next {
		return nil
	}
	start := int64(1)
	if eh.next > eventRingCap {
		start = eh.next - eventRingCap + 1
	}
	if after+1 > start {
		start = after + 1
	}
	out := make([]*shared.Event, 0, eh.next-start+1)
	for id := start; id <= eh.next; id++ {
		ev := eh.ring[int((id-1)%int64(eventRingCap))]
		if ev != nil && ev.ID == id {
			out = append(out, ev)
		}
	}
	return out
}
