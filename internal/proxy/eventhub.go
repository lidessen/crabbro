package proxy

import (
	"sync"

	"github.com/lidessen/chiling/internal/shared"
)

// subscriber represents a single event subscription.
type subscriber struct {
	ch    chan *shared.Event
	done  chan struct{}
	close sync.Once
}

// EventHub manages event subscriptions and broadcasts events to all
// subscribers. It is safe for concurrent use.
type EventHub struct {
	mu          sync.RWMutex
	subscribers []*subscriber
}

// NewEventHub creates a new EventHub.
func NewEventHub() *EventHub {
	return &EventHub{}
}

// Subscribe creates a new event subscription. The returned channel receives
// events as they are emitted. The subscription is automatically removed when
// the channel is closed or the context is cancelled.
func (eh *EventHub) Subscribe() <-chan *shared.Event {
	eh.mu.Lock()
	defer eh.mu.Unlock()

	sub := &subscriber{
		ch:   make(chan *shared.Event, 256),
		done: make(chan struct{}),
	}
	eh.subscribers = append(eh.subscribers, sub)
	return sub.ch
}

// Unsubscribe removes a subscription by closing its channel.
func (eh *EventHub) Unsubscribe(ch <-chan *shared.Event) {
	eh.mu.Lock()
	defer eh.mu.Unlock()

	for i, sub := range eh.subscribers {
		if sub.ch == ch {
			sub.close.Do(func() {
				close(sub.ch)
				close(sub.done)
			})
			eh.subscribers = append(eh.subscribers[:i], eh.subscribers[i+1:]...)
			return
		}
	}
}

// Emit broadcasts an event to all active subscribers. Slow subscribers that
// don't read from their channel may miss events (non-blocking send).
func (eh *EventHub) Emit(event *shared.Event) {
	eh.mu.RLock()
	defer eh.mu.RUnlock()

	for _, sub := range eh.subscribers {
		select {
		case sub.ch <- event:
		default:
			// Subscriber is too slow; drop the event.
		}
	}
}

// Len returns the number of active subscribers.
func (eh *EventHub) Len() int {
	eh.mu.RLock()
	defer eh.mu.RUnlock()
	return len(eh.subscribers)
}
