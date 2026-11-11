package ring

import (
	"sync"
	"testing"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

func TestNewRequestBufferDefaultCapacity(t *testing.T) {
	rb := NewRequestBuffer(0)
	if rb.Cap() != shared.DefaultRingBufferCapacity {
		t.Fatalf("expected cap %d, got %d", shared.DefaultRingBufferCapacity, rb.Cap())
	}
	if rb.Len() != 0 {
		t.Fatalf("expected len 0, got %d", rb.Len())
	}
}

func TestNewRequestBufferExplicitCapacity(t *testing.T) {
	rb := NewRequestBuffer(50)
	if rb.Cap() != 50 {
		t.Fatalf("expected cap 50, got %d", rb.Cap())
	}
}

func TestAddAssignsID(t *testing.T) {
	rb := NewRequestBuffer(10)
	req := rb.Add("GET", "https", "example.com", "/path", "q=1", "https://example.com/path?q=1", "127.0.0.1:12345")

	if req.ID == "" {
		t.Fatal("expected non-empty request ID")
	}
	if string(req.ID)[:4] != "req_" {
		t.Fatalf("expected ID to start with 'req_', got %q", req.ID)
	}
	if req.Method != "GET" {
		t.Fatalf("expected Method=GET, got %q", req.Method)
	}
	if req.Host != "example.com" {
		t.Fatalf("expected Host=example.com, got %q", req.Host)
	}
	if req.Path != "/path" {
		t.Fatalf("expected Path=/path, got %q", req.Path)
	}
	if req.Query != "q=1" {
		t.Fatalf("expected Query=q=1, got %q", req.Query)
	}
	if req.URL != "https://example.com/path?q=1" {
		t.Fatalf("expected URL=https://example.com/path?q=1, got %q", req.URL)
	}
	if req.ClientAddr != "127.0.0.1:12345" {
		t.Fatalf("expected ClientAddr=127.0.0.1:12345, got %q", req.ClientAddr)
	}
}

func TestAddIncrementsID(t *testing.T) {
	rb := NewRequestBuffer(10)
	req1 := rb.Add("GET", "https", "a.com", "/", "", "https://a.com/", "1")
	req2 := rb.Add("POST", "https", "b.com", "/api", "", "https://b.com/api", "2")

	if req1.ID == req2.ID {
		t.Fatal("expected different IDs for different requests")
	}
}

func TestAddReturnsSamePointer(t *testing.T) {
	rb := NewRequestBuffer(10)
	req1 := rb.Add("GET", "https", "a.com", "/", "", "https://a.com/", "1")
	req2 := rb.GetByID(req1.ID)
	if req2 == nil {
		t.Fatal("expected to find request by ID")
	}
	if req1 != req2 {
		t.Fatal("expected same pointer from Add and GetByID")
	}
}

func TestAddMutation(t *testing.T) {
	rb := NewRequestBuffer(10)
	req := rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")

	// Mutate via pointer.
	req.Status = 200
	req.Method = "POST"

	got := rb.GetByID(req.ID)
	if got == nil {
		t.Fatal("request not found")
	}
	if got.Status != 200 {
		t.Fatalf("expected Status 200, got %d", got.Status)
	}
	if got.Method != "POST" {
		t.Fatalf("expected Method POST, got %q", got.Method)
	}
}

func TestGetByIDFound(t *testing.T) {
	rb := NewRequestBuffer(10)
	req := rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")

	got := rb.GetByID(req.ID)
	if got == nil {
		t.Fatal("expected to find request")
	}
	if got != req {
		t.Fatal("expected same pointer")
	}
}

func TestGetByIDNotFound(t *testing.T) {
	rb := NewRequestBuffer(10)
	got := rb.GetByID("req_nonexistent")
	if got != nil {
		t.Fatal("expected nil for unknown ID")
	}
}

func TestGetByIDAfterOverwrite(t *testing.T) {
	rb := NewRequestBuffer(3)
	var reqs []*shared.Request
	for range 5 {
		reqs = append(reqs, rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1"))
	}
	// Buffer has capacity 3, so reqs[0] and reqs[1] were overwritten.
	if rb.GetByID(reqs[0].ID) != nil {
		t.Fatal("expected reqs[0] to be evicted")
	}
	if rb.GetByID(reqs[1].ID) != nil {
		t.Fatal("expected reqs[1] to be evicted")
	}
	if rb.GetByID(reqs[2].ID) == nil {
		t.Fatal("expected reqs[2] to still be present")
	}
	if rb.GetByID(reqs[3].ID) == nil {
		t.Fatal("expected reqs[3] to still be present")
	}
	if rb.GetByID(reqs[4].ID) == nil {
		t.Fatal("expected reqs[4] to still be present")
	}
}

func TestListAll(t *testing.T) {
	rb := NewRequestBuffer(10)
	for range 5 {
		rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
	}

	list := rb.List(0)
	if len(list) != 5 {
		t.Fatalf("expected 5 requests, got %d", len(list))
	}
}

func TestListWithLimit(t *testing.T) {
	rb := NewRequestBuffer(10)
	for range 10 {
		rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
	}

	list := rb.List(3)
	if len(list) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(list))
	}
}

func TestListLimitGreaterThanCount(t *testing.T) {
	rb := NewRequestBuffer(10)
	for range 3 {
		rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
	}

	list := rb.List(100)
	if len(list) != 3 {
		t.Fatalf("expected all 3 requests when limit > count, got %d", len(list))
	}
}

func TestListOrder(t *testing.T) {
	rb := NewRequestBuffer(10)
	var ids []shared.RequestID
	for range 5 {
		req := rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
		ids = append(ids, req.ID)
	}

	list := rb.List(5)
	for i, r := range list {
		if r.ID != ids[i] {
			t.Fatalf("index %d: expected ID %q, got %q", i, ids[i], r.ID)
		}
	}
}

func TestListSinceWithCursor(t *testing.T) {
	rb := NewRequestBuffer(10)
	var ids []shared.RequestID
	for range 5 {
		req := rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
		ids = append(ids, req.ID)
	}

	after := rb.ListSince(string(ids[2]), 0)
	if len(after) != 2 {
		t.Fatalf("expected 2 requests after cursor at index 2, got %d", len(after))
	}
	if after[0].ID != ids[3] {
		t.Fatalf("expected first after cursor to be id[3]=%s, got %s", ids[3], after[0].ID)
	}
}

func TestListSinceWithLimit(t *testing.T) {
	rb := NewRequestBuffer(10)
	var ids []shared.RequestID
	for range 10 {
		req := rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
		ids = append(ids, req.ID)
	}

	after := rb.ListSince(string(ids[3]), 3)
	if len(after) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(after))
	}
	if after[0].ID != ids[4] {
		t.Fatalf("expected first item ids[4]=%s, got %s", ids[4], after[0].ID)
	}
	if after[2].ID != ids[6] {
		t.Fatalf("expected third item ids[6]=%s, got %s", ids[6], after[2].ID)
	}
}

func TestListSinceEmptyCursor(t *testing.T) {
	rb := NewRequestBuffer(10)
	for range 5 {
		rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
	}

	all := rb.ListSince("", 0)
	if len(all) != 5 {
		t.Fatalf("expected all 5 requests with empty cursor, got %d", len(all))
	}
}

func TestListSinceEmptyBuffer(t *testing.T) {
	rb := NewRequestBuffer(10)
	result := rb.ListSince("req_1", 0)
	if result != nil {
		t.Fatalf("expected nil for empty buffer, got %v", result)
	}
}

func TestListSincePastEnd(t *testing.T) {
	rb := NewRequestBuffer(10)
	var ids []shared.RequestID
	for range 3 {
		req := rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
		ids = append(ids, req.ID)
	}

	result := rb.ListSince(string(ids[2]), 0)
	if result != nil {
		t.Fatalf("expected nil when cursor is at last item, got %d items", len(result))
	}
}

func TestListSinceNonexistentCursor(t *testing.T) {
	rb := NewRequestBuffer(10)
	for range 5 {
		rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
	}

	result := rb.ListSince("req_nonexistent", 0)
	if len(result) != 5 {
		t.Fatalf("expected all 5 items when cursor not found, got %d", len(result))
	}
}

func TestConcurrentAddAndList(t *testing.T) {
	rb := NewRequestBuffer(500)
	var wg sync.WaitGroup

	// Concurrent adds.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rb.Add("GET", "https", "example.com", "/", "", "https://example.com/", "1")
			}
		}()
	}

	// Concurrent reads.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				rb.Len()
				rb.List(10)
				rb.ListSince("", 10)
				rb.Cap()
			}
		}()
	}
	wg.Wait()
}

func TestFormatReqID(t *testing.T) {
	tests := []struct {
		id   int64
		want string
	}{
		{1, "req_1"},
		{10, "req_a"},
		{35, "req_z"},
		{36, "req_10"},
		{71, "req_1z"},
		{1295, "req_zz"},
		{1296, "req_100"},
	}
	for _, tt := range tests {
		got := formatReqID(tt.id)
		if got != tt.want {
			t.Errorf("formatReqID(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// A lookup that matches before the newest entry must not leave the buffer
// locked: Add after it has to return.
func TestGetByIDDoesNotBlockLaterAdds(t *testing.T) {
	rb := NewRequestBuffer(8)
	first := rb.Add("GET", "http", "a.test", "/", "", "http://a.test/", "")
	rb.Add("GET", "http", "a.test", "/2", "", "http://a.test/2", "")
	if got := rb.GetByID(first.ID); got == nil {
		t.Fatalf("GetByID(%s) = nil", first.ID)
	}
	done := make(chan struct{})
	go func() {
		rb.Add("GET", "http", "a.test", "/3", "", "http://a.test/3", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Add blocked after GetByID of an older request")
	}
}
