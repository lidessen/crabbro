package capture

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
)

func TestDefaultSinkLifecycle(t *testing.T) {
	rb := ring.NewRequestBuffer(8)
	bc := NewBodyCache(1024*1024, 64*1024)
	hub := NewEventHub()
	sess := session.New(32)
	hist, err := history.Open(filepath.Join(t.TempDir(), "history.db"), time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()

	sess.SetObserver(func(a shared.Activity) {
		hub.Emit(&shared.Event{Type: shared.EventActivity, RequestID: shared.RequestID(a.RequestID), Timestamp: a.At, Payload: a})
	})
	recSession := sess.Start("gw-test")

	sink := NewDefaultSink(rb, bc, hub, sess, hist)
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	req := sink.Start(&shared.Request{
		StartedAt: time.Now(),
		Kind:      "gateway",
		Method:    "POST",
		Scheme:    "http",
		Host:      "127.0.0.1:8081",
		Path:      "/v1/messages",
		URL:       "http://127.0.0.1:8081/v1/messages",
		Gateway:   &shared.GatewayTrace{RouteID: "coding-main", ChosenPlan: "glm-coding"},
	})
	if req.ID == "" || req.SessionID != recSession.ID || req.Kind != "gateway" {
		t.Fatalf("started = %+v", req)
	}

	req.RequestBodyRef = sink.StoreBody([]byte(`{"hello":"world"}`))
	req.ResponseBodyRef = sink.StoreBody([]byte(`{"ok":true}`))
	req.Status = 200
	sink.Emit(shared.EventResponseCompleted, req.ID, map[string]any{"status": 200})
	sink.Complete(req)

	got, reqBody, respBody, err := hist.Get(req.ID)
	if err != nil || got == nil || got.Gateway == nil || got.Gateway.ChosenPlan != "glm-coding" {
		t.Fatalf("history Get = %+v, err=%v", got, err)
	}
	if string(reqBody) != `{"hello":"world"}` || string(respBody) != `{"ok":true}` {
		t.Fatalf("bodies = %q / %q", reqBody, respBody)
	}
}
