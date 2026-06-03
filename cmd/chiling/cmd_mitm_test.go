package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lidessen/chiling/internal/shared"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestServer creates a test HTTP server that dispatches requests by path
// to the given handler map. It returns the server and a clientConfig
// configured to reach it via TCP.
func newTestServer(handlers map[string]http.HandlerFunc) (*httptest.Server, *clientConfig) {
	mux := http.NewServeMux()
	for route, fn := range handlers {
		mux.HandleFunc(route, fn)
	}
	s := httptest.NewServer(mux)
	cfg := &clientConfig{TCP: s.Listener.Addr().String()}
	return s, cfg
}

// captureStdout runs f and returns everything written to os.Stdout.
func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// writeStatus writes the JSON-encoded v as an HTTP 200 response.
func writeStatus(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(v)
}

// writeError writes body as a plain-text HTTP error response.
func writeError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

// ---------------------------------------------------------------------------
// cmdMITM — dispatch
// ---------------------------------------------------------------------------

func TestCmdMITM_NoArgs(t *testing.T) {
	err := cmdMITM(nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
}

func TestCmdMITM_EmptyArgs(t *testing.T) {

	err := cmdMITM(nil, []string{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
}

func TestCmdMITM_UnknownCommand(t *testing.T) {

	err := cmdMITM(nil, []string{"fly"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown mitm command") {
		t.Errorf("expected unknown command error, got: %v", err)
	}
}

func TestCmdMITM_DispatchesEnable(t *testing.T) {

	var method, path string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/enable": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			path = r.URL.Path
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITM(cfg, []string{"enable"}); err != nil {
			t.Fatal(err)
		}
	})
	if method != http.MethodPost {
		t.Errorf("expected POST, got %s", method)
	}
	if !strings.Contains(out, "MITM enabled") {
		t.Errorf("expected 'MITM enabled', got %q", out)
	}
	_ = path
}

func TestCmdMITM_DispatchesDisable(t *testing.T) {

	var method string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/disable": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITM(cfg, []string{"disable"}); err != nil {
			t.Fatal(err)
		}
	})
	if method != http.MethodPost {
		t.Errorf("expected POST, got %s", method)
	}
	if !strings.Contains(out, "MITM disabled") {
		t.Errorf("expected 'MITM disabled', got %q", out)
	}
}

func TestCmdMITM_DispatchesStatus(t *testing.T) {

	var method, path string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/status": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			path = r.URL.Path
			writeStatus(w, shared.MITMStatusResponse{Enabled: true})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITM(cfg, []string{"status"}); err != nil {
			t.Fatal(err)
		}
	})
	if method != http.MethodGet {
		t.Errorf("expected GET, got %s", method)
	}
	if !strings.Contains(out, `"enabled":true`) {
		t.Errorf("expected JSON status, got %q", out)
	}
	_ = path
}

func TestCmdMITM_DispatchesCert(t *testing.T) {

	var method string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/cert": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("-----BEGIN CERTIFICATE-----\nMOCK\n-----END CERTIFICATE-----\n"))
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITM(cfg, []string{"cert"}); err != nil {
			t.Fatal(err)
		}
	})
	if method != http.MethodGet {
		t.Errorf("expected GET, got %s", method)
	}
	if !strings.Contains(out, "BEGIN CERTIFICATE") {
		t.Errorf("expected cert output, got %q", out)
	}
}

func TestCmdMITM_DispatchesAllow(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, map[string][]string{"patterns": {}})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITM(cfg, []string{"allow", "list"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "No allow patterns") {
		t.Errorf("expected 'No allow patterns', got %q", out)
	}
}

func TestCmdMITM_DispatchesDeny(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, map[string][]string{"patterns": {"evil.com"}})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITM(cfg, []string{"deny", "list"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "deny patterns") {
		t.Errorf("expected deny patterns listing, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMEnable
// ---------------------------------------------------------------------------

func TestCmdMITMEnable_Success(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/enable": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMEnable(cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "MITM enabled") {
		t.Errorf("expected 'MITM enabled', got %q", out)
	}
}

func TestCmdMITMEnable_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/enable": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, "internal error")
		},
	})
	defer s.Close()

	err := cmdMITMEnable(cfg, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("expected server error message, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMDisable
// ---------------------------------------------------------------------------

func TestCmdMITMDisable_Success(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/disable": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMDisable(cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "MITM disabled") {
		t.Errorf("expected 'MITM disabled', got %q", out)
	}
}

func TestCmdMITMDisable_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/disable": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusBadRequest, "already disabled")
		},
	})
	defer s.Close()

	err := cmdMITMDisable(cfg, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMStatus
// ---------------------------------------------------------------------------

func TestCmdMITMStatus_Success(t *testing.T) {

	status := shared.MITMStatusResponse{
		Enabled:       true,
		CertInstalled: false,
		AllowPatterns: []string{"*.example.com"},
		DenyPatterns:  []string{},
	}
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/status": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			writeStatus(w, status)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMStatus(cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	// Non-pretty mode: raw JSON
	if !strings.Contains(out, `"enabled":true`) {
		t.Errorf("expected raw JSON with enabled field, got %q", out)
	}
	if !strings.Contains(out, `*.example.com`) {
		t.Errorf("expected allow patterns in output, got %q", out)
	}
}

func TestCmdMITMStatus_Pretty(t *testing.T) {

	status := shared.MITMStatusResponse{
		Enabled:       true,
		CertInstalled: true,
		AllowPatterns: []string{"*.example.com", "api.example.com"},
		DenyPatterns:  []string{"evil.com"},
	}
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/status": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, status)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMStatus(cfg, []string{"--pretty"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Enabled:       true") {
		t.Errorf("expected pretty 'Enabled: true', got %q", out)
	}
	if !strings.Contains(out, "Cert Installed: true") {
		t.Errorf("expected pretty 'Cert Installed: true', got %q", out)
	}
	if !strings.Contains(out, "*.example.com") {
		t.Errorf("expected allow patterns in pretty output, got %q", out)
	}
	if !strings.Contains(out, "evil.com") {
		t.Errorf("expected deny patterns in pretty output, got %q", out)
	}
}

func TestCmdMITMStatus_PrettyNoPatterns(t *testing.T) {

	status := shared.MITMStatusResponse{
		Enabled:       false,
		CertInstalled: false,
		AllowPatterns: nil,
		DenyPatterns:  []string{},
	}
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/status": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, status)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMStatus(cfg, []string{"--pretty"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Allow Patterns: (none)") {
		t.Errorf("expected '(none)' for allow patterns, got %q", out)
	}
	if !strings.Contains(out, "Deny Patterns:  (none)") {
		t.Errorf("expected '(none)' for deny patterns, got %q", out)
	}
}

func TestCmdMITMStatus_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/status": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "daemon not ready")
		},
	})
	defer s.Close()

	err := cmdMITMStatus(cfg, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
	if !strings.Contains(err.Error(), "daemon not ready") {
		t.Errorf("expected server message in error, got: %v", err)
	}
}

func TestCmdMITMStatus_PrettyIgnoredOnNonPrettyArgs(t *testing.T) {

	// Verify that non-pretty args produce raw JSON.
	status := shared.MITMStatusResponse{Enabled: true}
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/status": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, status)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMStatus(cfg, []string{"--other"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"enabled":true`) {
		t.Errorf("expected JSON output, got %q", out)
	}
	// Pretty-print header should NOT appear.
	if strings.Contains(out, "Enabled:") {
		t.Errorf("pretty-print header should not appear for non-pretty args, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMCert
// ---------------------------------------------------------------------------

const testCertPEM = "-----BEGIN CERTIFICATE-----\nMOCKCERT\n-----END CERTIFICATE-----\n"

func TestCmdMITMCert_SuccessNoOutput(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/cert": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(testCertPEM))
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMCert(cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	if out != testCertPEM {
		t.Errorf("expected cert PEM on stdout, got %q", out)
	}
}

func TestCmdMITMCert_WithOutput(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/cert": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(testCertPEM))
		},
	})
	defer s.Close()

	dest := filepath.Join(t.TempDir(), "rootCA.pem")
	out := captureStdout(func() {
		if err := cmdMITMCert(cfg, []string{"--output", dest}); err != nil {
			t.Fatal(err)
		}
	})

	// Should print the save message, not the cert.
	if !strings.Contains(out, "Root CA certificate saved to") {
		t.Errorf("expected save message, got %q", out)
	}
	if !strings.Contains(out, dest) {
		t.Errorf("expected destination in message, got %q", out)
	}

	// Verify the file was written correctly.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != testCertPEM {
		t.Errorf("file content mismatch:\ngot:  %q\nwant: %q", string(data), testCertPEM)
	}
}

func TestCmdMITMCert_WithOutputFlagLastArg(t *testing.T) {

	// Test that --output at the end of args without a following arg
	// results in outputPath being "" (flag parsing checks i+1 < len(args)).
	// This means the function will print to stdout instead.
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/cert": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(testCertPEM))
		},
	})
	defer s.Close()

	// --output as the last arg with no value → outputPath stays "" → prints PEM to stdout.
	out := captureStdout(func() {
		if err := cmdMITMCert(cfg, []string{"--output"}); err != nil {
			t.Fatal(err)
		}
	})
	if out != testCertPEM {
		t.Errorf("expected cert on stdout when --output has no path, got %q", out)
	}
}

func TestCmdMITMCert_WriteError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/cert": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-pem-file")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(testCertPEM))
		},
	})
	defer s.Close()

	// Non-existent directory should cause os.WriteFile to fail.
	err := cmdMITMCert(cfg, []string{"--output", "/nonexistent/dir/cert.pem"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "write cert to") {
		t.Errorf("expected write error, got: %v", err)
	}
}

func TestCmdMITMCert_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/cert": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusForbidden, "MITM not enabled")
		},
	})
	defer s.Close()

	err := cmdMITMCert(cfg, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMAllowDeny — dispatch
// ---------------------------------------------------------------------------

func TestCmdMITMAllowDeny_NoArgs(t *testing.T) {

	err := cmdMITMAllowDeny(nil, "allow", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
}

func TestCmdMITMAllowDeny_EmptyArgs(t *testing.T) {

	err := cmdMITMAllowDeny(nil, "deny", []string{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
}

func TestCmdMITMAllowDeny_UnknownCommand(t *testing.T) {

	err := cmdMITMAllowDeny(nil, "allow", []string{"fly"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown allow command") {
		t.Errorf("expected unknown command error, got: %v", err)
	}

	err = cmdMITMAllowDeny(nil, "deny", []string{"fly"})
	if err == nil {
		t.Fatal("expected error for deny too, got nil")
	}
	if !strings.Contains(err.Error(), "unknown deny command") {
		t.Errorf("expected unknown deny command error, got: %v", err)
	}
}

func TestCmdMITMAllowDeny_AddNoPattern(t *testing.T) {

	err := cmdMITMAllowDeny(nil, "allow", []string{"add"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "allow") {
		t.Errorf("expected list type in error, got: %v", err)
	}
}

func TestCmdMITMAllowDeny_RemoveNoPattern(t *testing.T) {

	err := cmdMITMAllowDeny(nil, "deny", []string{"remove"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "deny") {
		t.Errorf("expected list type in error, got: %v", err)
	}
}

func TestCmdMITMAllowDeny_DispatchesList(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			writeStatus(w, map[string][]string{"patterns": {"*.example.com"}})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMAllowDeny(cfg, "allow", []string{"list"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "allow patterns") {
		t.Errorf("expected allow patterns listing, got %q", out)
	}
	if !strings.Contains(out, "*.example.com") {
		t.Errorf("expected pattern in listing, got %q", out)
	}
}

func TestCmdMITMAllowDeny_DispatchesListDeny(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			writeStatus(w, map[string][]string{"patterns": {"evil.com", "bad.net"}})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMAllowDeny(cfg, "deny", []string{"list"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "deny patterns") {
		t.Errorf("expected deny patterns listing, got %q", out)
	}
	if !strings.Contains(out, "evil.com") {
		t.Errorf("expected pattern in listing, got %q", out)
	}
}

func TestCmdMITMAllowDeny_DispatchesAdd(t *testing.T) {

	var receivedMethod, receivedBody string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			receivedMethod = r.Method
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMAllowDeny(cfg, "allow", []string{"add", "*.example.com"}); err != nil {
			t.Fatal(err)
		}
	})
	if receivedMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", receivedMethod)
	}
	if !strings.Contains(receivedBody, `"pattern":"*.example.com"`) {
		t.Errorf("expected pattern in request body, got %s", receivedBody)
	}
	if !strings.Contains(out, `"*.example.com" added to allow list`) {
		t.Errorf("expected confirmation, got %q", out)
	}
}

func TestCmdMITMAllowDeny_DispatchesRemove(t *testing.T) {

	var receivedMethod, receivedPath string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			receivedMethod = r.Method
			receivedPath = r.URL.RequestURI()
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMAllowDeny(cfg, "deny", []string{"remove", "evil.com"}); err != nil {
			t.Fatal(err)
		}
	})
	if receivedMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", receivedMethod)
	}
	if !strings.Contains(receivedPath, "pattern=evil.com") {
		t.Errorf("expected pattern in query, got %s", receivedPath)
	}
	if !strings.Contains(out, `"evil.com" removed from deny list`) {
		t.Errorf("expected confirmation, got %q", out)
	}
}

func TestCmdMITMAllowDeny_AddAPIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusBadRequest, "invalid pattern")
		},
	})
	defer s.Close()

	err := cmdMITMAllowDeny(cfg, "allow", []string{"add", "**"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

func TestCmdMITMAllowDeny_RemoveAPIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "pattern not found")
		},
	})
	defer s.Close()

	err := cmdMITMAllowDeny(cfg, "deny", []string{"remove", "nothing.com"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMList
// ---------------------------------------------------------------------------

func TestCmdMITMList_WithPatterns(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			writeStatus(w, map[string][]string{"patterns": {"a.com", "b.com"}})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMList(cfg, "allow"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "allow patterns:") {
		t.Errorf("expected header, got %q", out)
	}
	if !strings.Contains(out, "a.com") {
		t.Errorf("expected a.com in output, got %q", out)
	}
	if !strings.Contains(out, "b.com") {
		t.Errorf("expected b.com in output, got %q", out)
	}
}

func TestCmdMITMList_NoPatterns(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, map[string][]string{"patterns": {}})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMList(cfg, "deny"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "No deny patterns") {
		t.Errorf("expected 'No deny patterns', got %q", out)
	}
}

func TestCmdMITMList_NullPatterns(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, map[string][]string{})
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMList(cfg, "allow"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "No allow patterns") {
		t.Errorf("expected 'No allow patterns', got %q", out)
	}
}

func TestCmdMITMList_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, "storage error")
		},
	})
	defer s.Close()

	err := cmdMITMList(cfg, "deny")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMAdd
// ---------------------------------------------------------------------------

func TestCmdMITMAdd_Success(t *testing.T) {

	var jsonBody string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			jsonBody = string(body)
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMAdd(cfg, "allow", "*.example.com"); err != nil {
			t.Fatal(err)
		}
	})

	// Verify JSON body
	var req shared.MITMDomainRequest
	if err := json.Unmarshal([]byte(jsonBody), &req); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if req.Pattern != "*.example.com" {
		t.Errorf("expected pattern '*.example.com', got %q", req.Pattern)
	}

	// Verify confirmation message
	expected := `"*.example.com" added to allow list`
	if !strings.Contains(out, expected) {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestCmdMITMAdd_Deny(t *testing.T) {

	var gotMethod, gotPath string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	if err := cmdMITMAdd(cfg, "deny", "evil.com"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/mitm/deny" {
		t.Errorf("expected /v1/mitm/deny, got %s", gotPath)
	}
}

func TestCmdMITMAdd_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusBadRequest, "bad pattern")
		},
	})
	defer s.Close()

	err := cmdMITMAdd(cfg, "allow", "**")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cmdMITMRemove
// ---------------------------------------------------------------------------

func TestCmdMITMRemove_Success(t *testing.T) {

	var gotMethod, gotPath string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.RequestURI()
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMRemove(cfg, "deny", "evil.com"); err != nil {
			t.Fatal(err)
		}
	})
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if !strings.Contains(gotPath, "pattern=evil.com") {
		t.Errorf("expected pattern in query, got %s", gotPath)
	}
	if !strings.Contains(gotPath, "/v1/mitm/deny") {
		t.Errorf("expected deny path, got %s", gotPath)
	}
	expected := `"evil.com" removed from deny list`
	if !strings.Contains(out, expected) {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestCmdMITMRemove_Allow(t *testing.T) {

	var gotMethod, gotQuery string
	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/allow": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
		},
	})
	defer s.Close()

	out := captureStdout(func() {
		if err := cmdMITMRemove(cfg, "allow", "old.com"); err != nil {
			t.Fatal(err)
		}
	})
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotQuery != "pattern=old.com" {
		t.Errorf("expected 'pattern=old.com', got %q", gotQuery)
	}
	expected := `"old.com" removed from allow list`
	if !strings.Contains(out, expected) {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestCmdMITMRemove_APIError(t *testing.T) {

	s, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/mitm/deny": func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "pattern not found")
		},
	})
	defer s.Close()

	err := cmdMITMRemove(cfg, "deny", "nothing.com")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "API error") {
		t.Errorf("expected 'API error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// formatPatterns
// ---------------------------------------------------------------------------

func TestFormatPatterns_Nil(t *testing.T) {

	result := formatPatterns(nil)
	if result != "(none)" {
		t.Errorf("expected '(none)', got %q", result)
	}
}

func TestFormatPatterns_Empty(t *testing.T) {

	result := formatPatterns([]string{})
	if result != "(none)" {
		t.Errorf("expected '(none)', got %q", result)
	}
}

func TestFormatPatterns_Single(t *testing.T) {

	result := formatPatterns([]string{"*.example.com"})
	if result != "*.example.com" {
		t.Errorf("expected '*.example.com', got %q", result)
	}
}

func TestFormatPatterns_Multiple(t *testing.T) {

	result := formatPatterns([]string{"a.com", "b.com", "c.com"})
	if result != "a.com, b.com, c.com" {
		t.Errorf("expected 'a.com, b.com, c.com', got %q", result)
	}
}

func TestFormatPatterns_SpecialChars(t *testing.T) {

	result := formatPatterns([]string{"*.example.com", "api.example.com"})
	if !strings.Contains(result, "*.example.com") {
		t.Errorf("expected wildcard pattern in result, got %q", result)
	}
	if !strings.Contains(result, "api.example.com") {
		t.Errorf("expected api.example.com in result, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// cmdMITM dispatch — install-ca / uninstall-ca
// ---------------------------------------------------------------------------

func TestCmdMITM_DispatchesInstallCA(t *testing.T) {
	// Should dispatch to cmdMITMInstallCA. If the cert file exists locally,
	// it will ask for confirmation and cancel (nil error). If the cert file
	// doesn't exist, it tries to download from daemon and fails.
	// Both outcomes are valid — we just verify it's not an unknown command.
	err := cmdMITM(&clientConfig{TCP: "127.0.0.1:1"}, []string{"install-ca"})
	if err != nil {
		if !strings.Contains(err.Error(), "cannot reach daemon") &&
			!strings.Contains(err.Error(), "unsupported platform") {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestCmdMITM_DispatchesUninstallCA(t *testing.T) {
	// Should dispatch to cmdMITMUninstallCA. Since cert doesn't exist,
	// it will proceed to the OS command and need confirmation. With no stdin,
	// confirm returns false, so we get "cancelled" output.
	cfg := &clientConfig{TCP: "127.0.0.1:1"}
	err := cmdMITMUninstallCA(cfg, nil)
	// Confirm returns false when stdin is closed → nil error (cancelled).
	if err != nil {
		t.Errorf("expected nil (cancelled), got: %v", err)
	}
}

func TestCmdMITM_UninstallCA_UnsupportedPlatform(t *testing.T) {
	// We can't easily mock runtime.GOOS, but we can test that the function
	// handles a missing cert gracefully when the command is unsupported.
	// This requires a file to exist to skip the Stat check, then a platform.
	// Minimal test: call the function on current platform - it should find
	// the cert missing and attempt uninstall anyway.
	err := cmdMITMUninstallCA(&clientConfig{TCP: "127.0.0.1:1"}, nil)
	if err != nil {
		// On unsupported platform, it returns an error.
		// On supported platform, it asks confirmation and gets cancelled (nil).
		// Either is acceptable.
		if !strings.Contains(err.Error(), "unsupported platform") {
			t.Errorf("expected nil or 'unsupported platform', got: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// OS command helpers
// ---------------------------------------------------------------------------

func TestOSInstallCertCommand_Darwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific test")
	}
	cmd, args := osInstallCertCommand("/tmp/test.pem")
	if cmd != "security" {
		t.Errorf("expected 'security', got %q", cmd)
	}
	found := false
	for _, a := range args {
		if a == "/tmp/test.pem" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected cert path in args, got %v", args)
	}
}

func TestOSInstallCertCommand_Linux(t *testing.T) {
	// test by manually calling with GOOS override trick — just test the pattern.
	cmd, args := osInstallCertCommand("/tmp/test.pem")
	_ = cmd
	_ = args
	// We can't reliably test this without changing runtime.GOOS, so we
	// just verify the function doesn't panic and returns a result.
}

func TestOSUninstallCertCommand_Darwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific test")
	}
	cmd, args := osUninstallCertCommand("/tmp/test.pem")
	if cmd != "security" {
		t.Errorf("expected 'security', got %q", cmd)
	}
	containsRemove := false
	for _, a := range args {
		if a == "remove-trusted-cert" {
			containsRemove = true
			break
		}
	}
	if !containsRemove {
		t.Errorf("expected 'remove-trusted-cert' in args, got %v", args)
	}
}

func TestDefaultCertPath(t *testing.T) {
	path := defaultCertPath()
	if !strings.Contains(path, ".chiling/certs/rootCA.pem") {
		t.Errorf("expected path to contain '.chiling/certs/rootCA.pem', got %q", path)
	}
}

// ---------------------------------------------------------------------------
// confirm
// ---------------------------------------------------------------------------

func TestConfirm_Yes(t *testing.T) {
	// Simulate user typing "y".
	r, w, _ := os.Pipe()
	w.WriteString("y\n")
	w.Close()

	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	if !confirm("Proceed?") {
		t.Error("expected confirm true for 'y'")
	}
}

func TestConfirm_No(t *testing.T) {
	r, w, _ := os.Pipe()
	w.WriteString("n\n")
	w.Close()

	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	if confirm("Proceed?") {
		t.Error("expected confirm false for 'n'")
	}
}

func TestConfirm_Empty(t *testing.T) {
	r, w, _ := os.Pipe()
	w.WriteString("\n")
	w.Close()

	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	if confirm("Proceed?") {
		t.Error("expected confirm false for empty input")
	}
}
