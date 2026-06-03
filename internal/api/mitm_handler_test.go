package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/chiling/internal/fixture"
	"github.com/lidessen/chiling/internal/mitm"
	"github.com/lidessen/chiling/internal/proxy"
	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/rule"
	"github.com/lidessen/chiling/internal/shared"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testMITMServer creates a Server wired with real components and a real
// MITMHandler backed by a dynamically generated root CA.
func testMITMServer(t *testing.T) *Server {
	t.Helper()

	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	mitmHandler := mitm.NewMITMHandler(ca, 10)
	mitmHandler.SetCertInstalled(true)

	rb := ring.NewRequestBuffer(100)
	bodyCache := proxy.NewBodyCache(1<<20, 1<<20)
	eventHub := proxy.NewEventHub()
	fixtureStore, err := fixture.New(t.TempDir())
	if err != nil {
		t.Fatalf("fixture.New: %v", err)
	}
	ruleStore := rule.NewRuleStore()
	ruleEngine := rule.NewRuleEngine()

	s := &Server{
		RingBuffer:   rb,
		BodyCache:    bodyCache,
		EventHub:     eventHub,
		FixtureStore: fixtureStore,
		RuleStore:    ruleStore,
		RuleEngine:   ruleEngine,
		ProxyAddr:    ":8080",
		Version:      "test",
		MITMHandler:  mitmHandler,
		mux:          http.NewServeMux(),
	}
	s.registerRoutes()
	s.handler = wrapMiddleware(s.mux)
	return s
}

// testMITMServerWithoutHandler creates a Server with no MITMHandler set, so
// MITM routes are not registered. Used to verify the 404 path when MITM is
// not configured.
func testMITMServerWithoutHandler(t *testing.T) *Server {
	t.Helper()

	rb := ring.NewRequestBuffer(100)
	bodyCache := proxy.NewBodyCache(1<<20, 1<<20)
	eventHub := proxy.NewEventHub()
	fixtureStore, err := fixture.New(t.TempDir())
	if err != nil {
		t.Fatalf("fixture.New: %v", err)
	}
	ruleStore := rule.NewRuleStore()
	ruleEngine := rule.NewRuleEngine()

	s := &Server{
		RingBuffer:   rb,
		BodyCache:    bodyCache,
		EventHub:     eventHub,
		FixtureStore: fixtureStore,
		RuleStore:    ruleStore,
		RuleEngine:   ruleEngine,
		ProxyAddr:    ":8080",
		Version:      "test",
		mux:          http.NewServeMux(),
	}
	s.registerRoutes()
	s.handler = wrapMiddleware(s.mux)
	return s
}

// ---------------------------------------------------------------------------
// Helper: decode error response
// ---------------------------------------------------------------------------

func decodeError(t *testing.T, body []byte) shared.ErrorResponse {
	t.Helper()
	var e shared.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("unmarshal error response: %v\nbody: %s", err, string(body))
	}
	return e
}

// ---------------------------------------------------------------------------
// handleMITMEnable
// ---------------------------------------------------------------------------

func TestMITMEnable(t *testing.T) {
	s := testMITMServer(t)

	// Subscribe to capture emitted event.
	sub := s.EventHub.Subscribe()
	defer s.EventHub.Unsubscribe(sub)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/enable", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if !body["enabled"] {
		t.Error(`"enabled" = false, want true`)
	}

	if !s.MITMHandler.Enabled() {
		t.Error("MITMHandler.Enabled() = false after enable request")
	}

	// Verify event was emitted.
	select {
	case ev := <-sub:
		if ev.Type != shared.EventMITMEnabled {
			t.Errorf("event type = %q, want %q", ev.Type, shared.EventMITMEnabled)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for EventMITMEnabled")
	}
}

func TestMITMEnable_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/enable", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s → status = %d, want 405", method, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestMITMEnable_NotConfigured(t *testing.T) {
	s := testMITMServer(t)
	// Nil the handler to exercise the handler's nil check (routes are already
	// registered, so they dispatch to the handler body).
	s.MITMHandler = nil

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/enable", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeNotFound)
	}
	if !strings.Contains(errResp.Error.Message, "MITM not configured") {
		t.Errorf("error message = %q, want substring %q", errResp.Error.Message, "MITM not configured")
	}
}

func TestMITMEnable_RoutesNotRegistered(t *testing.T) {
	// When MITMHandler is nil at construction time, routes are never registered.
	s := testMITMServerWithoutHandler(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/enable", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// handleMITMDisable
// ---------------------------------------------------------------------------

func TestMITMDisable(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.SetEnabled(true)

	sub := s.EventHub.Subscribe()
	defer s.EventHub.Unsubscribe(sub)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/disable", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if body["enabled"] {
		t.Error(`"enabled" = true, want false`)
	}

	if s.MITMHandler.Enabled() {
		t.Error("MITMHandler.Enabled() = true after disable request")
	}

	select {
	case ev := <-sub:
		if ev.Type != shared.EventMITMDisabled {
			t.Errorf("event type = %q, want %q", ev.Type, shared.EventMITMDisabled)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for EventMITMDisabled")
	}
}

func TestMITMDisable_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/disable", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s → status = %d, want 405", method, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestMITMDisable_NotConfigured(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler = nil

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/disable", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeNotFound)
	}
}

// ---------------------------------------------------------------------------
// handleMITMStatus
// ---------------------------------------------------------------------------

func TestMITMStatus(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.SetEnabled(true)
	s.MITMHandler.SetCertInstalled(true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/status", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var status shared.MITMStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if !status.Enabled {
		t.Error("Enabled = false, want true")
	}
	if !status.CertInstalled {
		t.Error("CertInstalled = false, want true")
	}
}

func TestMITMStatus_Disabled(t *testing.T) {
	s := testMITMServer(t)
	// MITM starts disabled by default.

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/status", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var status shared.MITMStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if status.Enabled {
		t.Error("Enabled = true, want false (default)")
	}
}

func TestMITMStatus_WithPatterns(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.Matcher.AddAllow("*.example.com")
	s.MITMHandler.Matcher.AddDeny("evil.com")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/status", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var status shared.MITMStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(status.AllowPatterns) != 1 || status.AllowPatterns[0] != "*.example.com" {
		t.Errorf("AllowPatterns = %v, want [\"*.example.com\"]", status.AllowPatterns)
	}
	if len(status.DenyPatterns) != 1 || status.DenyPatterns[0] != "evil.com" {
		t.Errorf("DenyPatterns = %v, want [\"evil.com\"]", status.DenyPatterns)
	}
}

func TestMITMStatus_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/status", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s → status = %d, want 405", method, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestMITMStatus_NotConfigured(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler = nil

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/status", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeNotFound)
	}
}

// ---------------------------------------------------------------------------
// handleMITMCert
// ---------------------------------------------------------------------------

func TestMITMCert(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/cert", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Check headers.
	ct := resp.Header.Get("Content-Type")
	if ct != "application/x-pem-file" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/x-pem-file")
	}
	cd := resp.Header.Get("Content-Disposition")
	if cd != `attachment; filename=rootCA.pem` {
		t.Errorf("Content-Disposition = %q, want %q", cd, "attachment; filename=rootCA.pem")
	}

	// Body should be non-empty PEM.
	body := w.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("cert body is empty")
	}
	if !strings.HasPrefix(string(body), "-----BEGIN CERTIFICATE-----") {
		t.Error("cert body does not start with PEM certificate header")
	}
	if !strings.Contains(string(body), "-----END CERTIFICATE-----") {
		t.Error("cert body does not contain PEM certificate footer")
	}
}

func TestMITMCert_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/cert", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s → status = %d, want 405", method, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestMITMCert_NotConfigured(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler = nil

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/cert", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeNotFound)
	}
}

// ---------------------------------------------------------------------------
// handleMITMAllow
// ---------------------------------------------------------------------------

func TestMITMAllow_Get_Empty(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/allow", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	patterns, ok := body["patterns"]
	if !ok {
		t.Fatal(`response missing "patterns" key`)
	}
	if len(patterns) != 0 {
		t.Errorf("patterns = %v, want empty slice", patterns)
	}
}

func TestMITMAllow_Get_WithPatterns(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.Matcher.AddAllow("example.com")
	s.MITMHandler.Matcher.AddAllow("*.test.com")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/allow", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	patterns := body["patterns"]
	if len(patterns) != 2 {
		t.Fatalf("patterns = %v, want 2 entries", patterns)
	}
}

func TestMITMAllow_Post(t *testing.T) {
	s := testMITMServer(t)

	body := `{"pattern":"*.allowed.com"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/allow", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rbody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if rbody["pattern"] != "*.allowed.com" {
		t.Errorf("pattern = %q, want %q", rbody["pattern"], "*.allowed.com")
	}
	if rbody["action"] != "added" {
		t.Errorf("action = %q, want %q", rbody["action"], "added")
	}

	// Verify the matcher actually has the pattern.
	patterns := s.MITMHandler.Matcher.AllowPatterns()
	found := false
	for _, p := range patterns {
		if p == "*.allowed.com" {
			found = true
			break
		}
	}
	if !found {
		t.Error("*.allowed.com not found in allow patterns after POST")
	}
}

func TestMITMAllow_Post_EmptyPattern(t *testing.T) {
	s := testMITMServer(t)

	body := `{"pattern":""}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/allow", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeBadRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeBadRequest)
	}
}

func TestMITMAllow_Post_InvalidJSON(t *testing.T) {
	s := testMITMServer(t)

	body := `not-json`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/allow", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeBadRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeBadRequest)
	}
}

func TestMITMAllow_Delete(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.Matcher.AddAllow("example.com")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/allow?pattern=example.com", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rbody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if rbody["pattern"] != "example.com" {
		t.Errorf("pattern = %q, want %q", rbody["pattern"], "example.com")
	}
	if rbody["action"] != "removed" {
		t.Errorf("action = %q, want %q", rbody["action"], "removed")
	}

	// Verify the matcher actually removed it.
	patterns := s.MITMHandler.Matcher.AllowPatterns()
	for _, p := range patterns {
		if p == "example.com" {
			t.Error("example.com still present in allow patterns after DELETE")
		}
	}
}

func TestMITMAllow_Delete_MissingPattern(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/allow", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeBadRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeBadRequest)
	}
}

func TestMITMAllow_Delete_NonExistent(t *testing.T) {
	// Deleting a pattern that does not exist should not error.
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/allow?pattern=nonexistent.com", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMITMAllow_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/allow", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s → status = %d, want 405", method, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestMITMAllow_NotConfigured(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler = nil

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/allow", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s → status = %d, want 404", method, resp.StatusCode)
			}
			resp.Body.Close()

			errResp := decodeError(t, w.Body.Bytes())
			if errResp.Error.Code != shared.ErrCodeNotFound {
				t.Errorf("%s → error code = %q, want %q", method, errResp.Error.Code, shared.ErrCodeNotFound)
			}
		})
	}
}

func TestMITMAllow_AllMethods(t *testing.T) {
	// Verify that GET, POST, DELETE all succeed on a configured server.
	s := testMITMServer(t)

	t.Run("GET", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/mitm/allow", nil)
		s.ServeHTTP(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET → status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("POST", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/mitm/allow",
			strings.NewReader(`{"pattern":"test.com"}`))
		req.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST → status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("DELETE", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/allow?pattern=test.com", nil)
		s.ServeHTTP(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("DELETE → status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})
}

// ---------------------------------------------------------------------------
// handleMITMDeny
// ---------------------------------------------------------------------------

func TestMITMDeny_Get_Empty(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/deny", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	patterns := body["patterns"]
	if len(patterns) != 0 {
		t.Errorf("patterns = %v, want empty slice", patterns)
	}
}

func TestMITMDeny_Get_WithPatterns(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.Matcher.AddDeny("evil.com")
	s.MITMHandler.Matcher.AddDeny("*.bad.net")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/deny", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	patterns := body["patterns"]
	if len(patterns) != 2 {
		t.Fatalf("patterns = %v, want 2 entries", patterns)
	}
}

func TestMITMDeny_Post(t *testing.T) {
	s := testMITMServer(t)

	body := `{"pattern":"blocked.com"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/deny", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rbody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if rbody["pattern"] != "blocked.com" {
		t.Errorf("pattern = %q, want %q", rbody["pattern"], "blocked.com")
	}
	if rbody["action"] != "added" {
		t.Errorf("action = %q, want %q", rbody["action"], "added")
	}

	// Verify the matcher actually has the pattern.
	patterns := s.MITMHandler.Matcher.DenyPatterns()
	found := false
	for _, p := range patterns {
		if p == "blocked.com" {
			found = true
			break
		}
	}
	if !found {
		t.Error("blocked.com not found in deny patterns after POST")
	}

	// Verify it actually denies the domain.
	if s.MITMHandler.Matcher.IsAllowed("blocked.com") {
		t.Error("blocked.com should be denied after adding to deny patterns")
	}
}

func TestMITMDeny_Post_EmptyPattern(t *testing.T) {
	s := testMITMServer(t)

	body := `{"pattern":""}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/deny", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeBadRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeBadRequest)
	}
}

func TestMITMDeny_Post_InvalidJSON(t *testing.T) {
	s := testMITMServer(t)

	body := `not-json`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/deny", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeBadRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeBadRequest)
	}
}

func TestMITMDeny_Delete(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.Matcher.AddDeny("evil.com")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/deny?pattern=evil.com", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rbody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&rbody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()

	if rbody["pattern"] != "evil.com" {
		t.Errorf("pattern = %q, want %q", rbody["pattern"], "evil.com")
	}
	if rbody["action"] != "removed" {
		t.Errorf("action = %q, want %q", rbody["action"], "removed")
	}

	// Verify the matcher actually removed it.
	patterns := s.MITMHandler.Matcher.DenyPatterns()
	for _, p := range patterns {
		if p == "evil.com" {
			t.Error("evil.com still present in deny patterns after DELETE")
		}
	}

	// evil.com should now be allowed (no deny patterns matching it).
	if !s.MITMHandler.Matcher.IsAllowed("evil.com") {
		t.Error("evil.com should be allowed after removal from deny patterns")
	}
}

func TestMITMDeny_Delete_MissingPattern(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/deny", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	errResp := decodeError(t, w.Body.Bytes())
	if errResp.Error.Code != shared.ErrCodeBadRequest {
		t.Errorf("error code = %q, want %q", errResp.Error.Code, shared.ErrCodeBadRequest)
	}
}

func TestMITMDeny_Delete_NonExistent(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/deny?pattern=nonexistent.com", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMITMDeny_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/deny", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s → status = %d, want 405", method, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestMITMDeny_NotConfigured(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler = nil

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/mitm/deny", nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s → status = %d, want 404", method, resp.StatusCode)
			}
			resp.Body.Close()

			errResp := decodeError(t, w.Body.Bytes())
			if errResp.Error.Code != shared.ErrCodeNotFound {
				t.Errorf("%s → error code = %q, want %q", method, errResp.Error.Code, shared.ErrCodeNotFound)
			}
		})
	}
}

func TestMITMDeny_AllMethods(t *testing.T) {
	s := testMITMServer(t)

	t.Run("GET", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/mitm/deny", nil)
		s.ServeHTTP(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET → status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("POST", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/mitm/deny",
			strings.NewReader(`{"pattern":"test.com"}`))
		req.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST → status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})

	t.Run("DELETE", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/v1/mitm/deny?pattern=test.com", nil)
		s.ServeHTTP(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("DELETE → status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	})
}

// ---------------------------------------------------------------------------
// Edge cases: enable/disable state transitions
// ---------------------------------------------------------------------------

func TestMITM_EnableDisable_StateTransitions(t *testing.T) {
	s := testMITMServer(t)

	// Start disabled.
	if s.MITMHandler.Enabled() {
		t.Fatal("expected MITM to start disabled")
	}

	// Enable.
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/enable", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatal("enable request failed")
	}
	w.Result().Body.Close()

	if !s.MITMHandler.Enabled() {
		t.Fatal("expected enabled after first enable")
	}

	// Double-enable should still work.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/mitm/enable", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatal("second enable request failed")
	}
	w.Result().Body.Close()

	if !s.MITMHandler.Enabled() {
		t.Fatal("expected still enabled after second enable")
	}

	// Disable.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/mitm/disable", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatal("disable request failed")
	}
	w.Result().Body.Close()

	if s.MITMHandler.Enabled() {
		t.Fatal("expected disabled after disable")
	}

	// Double-disable should still work.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/mitm/disable", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatal("second disable request failed")
	}
	w.Result().Body.Close()

	if s.MITMHandler.Enabled() {
		t.Fatal("expected still disabled after second disable")
	}
}

// ---------------------------------------------------------------------------
// Edge cases: routes not registered at all
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// cert-installed endpoint
// ---------------------------------------------------------------------------

func TestMITM_CertInstalled_SetsTrue(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/cert-installed", strings.NewReader(`{"installed":true}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if !s.MITMHandler.CertInstalled() {
		t.Error("CertInstalled should be true after POST")
	}
}

func TestMITM_CertInstalled_SetsFalse(t *testing.T) {
	s := testMITMServer(t)
	s.MITMHandler.SetCertInstalled(true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/cert-installed", strings.NewReader(`{"installed":false}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if s.MITMHandler.CertInstalled() {
		t.Error("CertInstalled should be false after POST with installed:false")
	}
}

func TestMITM_CertInstalled_InvalidBody(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/cert-installed", strings.NewReader(`not json`))
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMITM_CertInstalled_WrongMethod(t *testing.T) {
	s := testMITMServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/mitm/cert-installed", nil)
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestMITM_CertInstalled_NoHandler(t *testing.T) {
	s := testMITMServerWithoutHandler(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/mitm/cert-installed", strings.NewReader(`{"installed":true}`))
	s.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMITM_RoutesNotRegistered(t *testing.T) {
	s := testMITMServerWithoutHandler(t)

	paths := []string{
		"/v1/mitm/enable",
		"/v1/mitm/disable",
		"/v1/mitm/status",
		"/v1/mitm/cert",
		"/v1/mitm/cert-installed",
		"/v1/mitm/allow",
		"/v1/mitm/deny",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			s.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s → status = %d, want 404", path, resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}
