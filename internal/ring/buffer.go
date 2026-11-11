// Package ring provides a generic, concurrency-safe ring buffer for storing
// captured HTTP exchanges and other circular-buffer data.
package ring

import (
	"container/ring"
	"sync"
)

// RingBuffer is a generic, concurrency-safe ring buffer.
//
// T must be a pointer type so that the zero value of RingBuffer is usable:
// Push, Len, Do, and Iterate all work on a nil receiver as if the buffer is
// empty and has a default capacity of 64.
type RingBuffer[T any] struct {
	mu    sync.RWMutex
	cap   int
	items int
	ring  *ring.Ring
}

// New creates a RingBuffer with the given capacity. cap must be > 0.
func New[T any](cap int) *RingBuffer[T] {
	if cap < 1 {
		cap = 64
	}
	return &RingBuffer[T]{
		cap:  cap,
		ring: ring.New(cap),
	}
}

// Push appends an item to the buffer. If the buffer is full, the oldest item
// is overwritten.
func (rb *RingBuffer[T]) Push(item T) {
	if rb == nil {
		return
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.ring == nil {
		return
	}

	rb.ring.Value = item
	rb.ring = rb.ring.Next()
	if rb.items < rb.cap {
		rb.items++
	}
}

// Len returns the number of items currently in the buffer (≤ capacity).
func (rb *RingBuffer[T]) Len() int {
	if rb == nil {
		return 0
	}
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.items
}

// Cap returns the capacity of the buffer.
func (rb *RingBuffer[T]) Cap() int {
	if rb == nil {
		return 0
	}
	return rb.cap
}

// Do calls f on each item in the buffer, from oldest to newest. If f returns
// false, iteration stops early.
//
// Do is safe for concurrent use but holds a read lock for the entire iteration;
// keep f fast.
func (rb *RingBuffer[T]) Do(f func(T) bool) {
	if rb == nil {
		return
	}
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.ring == nil || rb.items == 0 {
		return
	}

	// Walk from the oldest item (items behind current write position).
	// The write position (rb.ring) is the next slot to be written to, so the
	// oldest item is rb.cap - rb.items steps back from it.
	cur := rb.ring.Move(-rb.items)
	for range rb.items {
		if cur.Value != nil {
			if !f(cur.Value.(T)) {
				return
			}
		}
		cur = cur.Next()
	}
}

// Iterate returns a channel that yields items from oldest to newest. The
// channel is closed when all items have been sent.
//
// The items are a snapshot taken before Iterate returns, already buffered in
// the channel: a caller that stops reading early leaves nothing behind. An
// earlier version sent from a goroutine that held the read lock, so a range
// that returned on its first match parked that goroutine with the lock held
// for ever, and the next Push waited on it — which stopped the proxy.
func (rb *RingBuffer[T]) Iterate() <-chan T {
	vals := rb.Values()
	ch := make(chan T, len(vals))
	for _, v := range vals {
		ch <- v
	}
	close(ch)
	return ch
}

// Values returns a snapshot of all items as a slice, oldest to newest.
func (rb *RingBuffer[T]) Values() []T {
	if rb == nil {
		return nil
	}
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.ring == nil || rb.items == 0 {
		return nil
	}

	out := make([]T, 0, rb.items)
	start := rb.ring.Move(-rb.items)
	start.Do(func(v any) {
		if v != nil {
			out = append(out, v.(T))
		}
	})
	return out
}

// Get returns the item at index i (0 = oldest), or the zero value and false
// if the index is out of range.
func (rb *RingBuffer[T]) Get(i int) (T, bool) {
	if rb == nil {
		var zero T
		return zero, false
	}
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.ring == nil || i < 0 || i >= rb.items {
		var zero T
		return zero, false
	}

	elem := rb.ring.Move(-rb.items + i)
	if elem.Value == nil {
		var zero T
		return zero, false
	}
	return elem.Value.(T), true
}

// Reset clears the buffer, keeping its capacity.
func (rb *RingBuffer[T]) Reset() {
	if rb == nil {
		return
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.ring == nil {
		return
	}

	rb.ring = ring.New(rb.cap)
	rb.items = 0
}
