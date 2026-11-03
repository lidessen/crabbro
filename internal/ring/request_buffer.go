package ring

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lidessen/chiling/internal/shared"
)

// RequestBuffer is a concurrency-safe ring buffer specialized for storing
// captured HTTP request exchanges. It wraps the generic RingBuffer and
// provides request-specific access patterns.
type RequestBuffer struct {
	buf    *RingBuffer[*shared.Request]
	nextID atomic.Int64
	mu     sync.Mutex
}

// NewRequestBuffer creates a request buffer with the given capacity.
func NewRequestBuffer(capacity int) *RequestBuffer {
	if capacity < 1 {
		capacity = shared.DefaultRingBufferCapacity
	}
	return &RequestBuffer{
		buf: New[*shared.Request](capacity),
	}
}

// Add inserts a new request and assigns it a stable ID. The returned request
// pointer is the same as the one stored in the buffer; callers can mutate it
// (under their own synchronization) to update status, response fields, etc.
func (rb *RequestBuffer) Add(method, scheme, host, path, query, url, clientAddr string) *shared.Request {
	id := rb.nextID.Add(1)
	req := &shared.Request{
		ID:         shared.RequestID(formatReqID(id)),
		Method:     method,
		Scheme:     scheme,
		Host:       host,
		Path:       path,
		Query:      query,
		URL:        url,
		ClientAddr: clientAddr,
	}
	rb.buf.Push(req)
	return req
}

// Resolve returns an exact ID match, or a unique prefix match.
func (rb *RequestBuffer) Resolve(id shared.RequestID) *shared.Request {
	if id == "" {
		return nil
	}
	if exact := rb.GetByID(id); exact != nil {
		return exact
	}
	var match *shared.Request
	for _, req := range rb.List(0) {
		if strings.HasPrefix(string(req.ID), string(id)) {
			if match != nil {
				return nil
			}
			match = req
		}
	}
	return match
}

// GetByID returns the request with the given ID, or nil if not found.
func (rb *RequestBuffer) GetByID(id shared.RequestID) *shared.Request {
	for req := range rb.buf.Iterate() {
		if req.ID == id {
			return req
		}
	}
	return nil
}

// List returns all requests from oldest to newest, up to limit (0 = all).
func (rb *RequestBuffer) List(limit int) []*shared.Request {
	all := rb.buf.Values()
	if limit <= 0 || limit >= len(all) {
		return all
	}
	// Return newest `limit` items.
	return all[len(all)-limit:]
}

// ListSince returns requests where startedAt is after the given cursor,
// up to limit (0 = all).
func (rb *RequestBuffer) ListSince(cursor string, limit int) []*shared.Request {
	all := rb.buf.Values()
	if len(all) == 0 {
		return nil
	}

	// Find the first item after the cursor.
	start := 0
	if cursor != "" {
		for i, r := range all {
			if string(r.ID) == cursor {
				start = i + 1
				break
			}
		}
	}

	if start >= len(all) {
		return nil
	}

	if limit > 0 && start+limit < len(all) {
		return all[start : start+limit]
	}
	return all[start:]
}

// Len returns the number of stored requests.
func (rb *RequestBuffer) Len() int {
	return rb.buf.Len()
}

// Cap returns the buffer capacity.
func (rb *RequestBuffer) Cap() int {
	return rb.buf.Cap()
}

// formatReqID formats a numeric ID into the req_XXXXX format.
func formatReqID(id int64) string {
	const prefix = "req_"
	// Simple base-36 encoding for compactness.
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
