package history

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

func TestSaveReopenAndPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")
	s, err := Open(path, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	reqOld := &shared.Request{ID: "req_old", StartedAt: old, Method: "GET", URL: "http://old.test/"}
	if err := s.Save(reqOld, nil, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, id := range []shared.RequestID{"req_a", "req_b", "req_c"} {
		req := &shared.Request{ID: id, StartedAt: now, Method: "POST", URL: "http://example.test/" + string(id), Status: 200}
		if err := s.Save(req, []byte("req-"+string(id)), []byte("resp-"+string(id))); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, rb, sb, err := s.Get("req_c")
	if err != nil || got == nil {
		t.Fatalf("get req_c: %v %#v", err, got)
	}
	if string(rb) != "req-req_c" || string(sb) != "resp-req_c" {
		t.Fatalf("bodies %q %q", rb, sb)
	}
	missing, _, _, err := s.Get("req_old")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatal("expired row should have been pruned on open")
	}
	rows, err := s.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	s.MaxRows = 1
	if _, err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	if s.Count() != 1 {
		t.Fatalf("count after max-rows prune = %d", s.Count())
	}
}
