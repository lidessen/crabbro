package proxy

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/lidessen/chiling/internal/shared"
)

// bodyEntry holds a cached request/response body.
type bodyEntry struct {
	key  shared.BodyRef
	data []byte
	size int
}

// BodyCache is a concurrency-safe LRU cache for HTTP request/response bodies.
type BodyCache struct {
	maxTotalBytes int
	maxBodySize   int
	mu            sync.Mutex
	ll            *list.List
	entries       map[shared.BodyRef]*list.Element
	currentBytes  int
	nextID        atomic.Int64
}

// NewBodyCache creates a body cache with the given limits.
func NewBodyCache(maxTotalBytes, maxBodySize int) *BodyCache {
	return &BodyCache{
		maxTotalBytes: maxTotalBytes,
		maxBodySize:   maxBodySize,
		ll:            list.New(),
		entries:       make(map[shared.BodyRef]*list.Element),
	}
}

// Store adds a body to the cache and returns its reference key. The body is
// truncated to maxBodySize. Old entries are evicted (LRU) if total exceeds
// maxTotalBytes.
func (bc *BodyCache) Store(data []byte) shared.BodyRef {
	if len(data) == 0 {
		return ""
	}

	// Truncate if needed.
	if len(data) > bc.maxBodySize {
		data = data[:bc.maxBodySize]
	}

	id := bc.nextID.Add(1)
	key := shared.BodyRef(formatBodyRef(id))

	entry := &bodyEntry{
		key:  key,
		data: data,
		size: len(data),
	}

	bc.mu.Lock()
	defer bc.mu.Unlock()

	// Evict until we have room.
	bc.evict(entry.size)

	elem := bc.ll.PushFront(entry)
	bc.entries[key] = elem
	bc.currentBytes += entry.size

	return key
}

// Get retrieves a body from the cache. Returns nil if the key is not found
// (evicted or never stored).
func (bc *BodyCache) Get(key shared.BodyRef) []byte {
	if key == "" {
		return nil
	}

	bc.mu.Lock()
	defer bc.mu.Unlock()

	elem, ok := bc.entries[key]
	if !ok {
		return nil
	}

	// Move to front (LRU).
	bc.ll.MoveToFront(elem)
	return elem.Value.(*bodyEntry).data
}

// evict removes the oldest entries until at least neededBytes are available.
// Must be called with bc.mu held.
func (bc *BodyCache) evict(neededBytes int) {
	for bc.currentBytes+neededBytes > bc.maxTotalBytes && bc.ll.Len() > 0 {
		elem := bc.ll.Back()
		if elem == nil {
			break
		}
		entry := elem.Value.(*bodyEntry)
		bc.currentBytes -= entry.size
		delete(bc.entries, entry.key)
		bc.ll.Remove(elem)
	}
}

// formatBodyRef formats a numeric ID into a body reference key.
func formatBodyRef(id int64) string {
	const prefix = "body_"
	s := make([]byte, 0, 10)
	s = append(s, prefix...)
	n := id
	for n > 0 {
		digit := n % 36
		if digit < 10 {
			s = append(s, byte('0'+digit))
		} else {
			s = append(s, byte('a'+digit-10))
		}
		n /= 36
	}
	// Reverse the numeric part.
	for i, j := len(prefix), len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return string(s)
}
