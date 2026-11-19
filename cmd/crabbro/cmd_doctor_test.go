package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lidessen/crabbro/internal/shared"
)

func TestCmdDoctor_NoArgs(t *testing.T) {
	// No daemon running → should report daemon unreachable issue.
	cfg := &clientConfig{TCP: "127.0.0.1:1"}
	err := cmdDoctor(cfg, nil)
	if err != nil {
		t.Fatalf("cmdDoctor should not error on unreachable daemon: %v", err)
	}
}

func TestCmdDoctor_DaemonRunning(t *testing.T) {
	// Set up a fake daemon server.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(shared.StatusResponse{
			Version:           "0.1.0",
			UptimeSeconds:     3600,
			ProxyAddr:         "localhost:8080",
			RequestCount:      42,
			RuleCount:         3,
			MITMEnabled:       true,
			MITMCertInstalled: false,
		})
	})
	s := httptest.NewServer(mux)
	defer s.Close()

	cfg := &clientConfig{TCP: s.Listener.Addr().String()}
	err := cmdDoctor(cfg, []string{"--pretty"})
	if err != nil {
		t.Fatalf("cmdDoctor failed: %v", err)
	}
}

func TestCmdDoctor_DaemonRunningMITMDisabled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(shared.StatusResponse{
			Version:           "0.1.0",
			UptimeSeconds:     100,
			ProxyAddr:         "localhost:8080",
			RequestCount:      5,
			RuleCount:         0,
			MITMEnabled:       false,
			MITMCertInstalled: true,
		})
	})
	s := httptest.NewServer(mux)
	defer s.Close()

	cfg := &clientConfig{TCP: s.Listener.Addr().String()}
	err := cmdDoctor(cfg, nil)
	if err != nil {
		t.Fatalf("cmdDoctor failed: %v", err)
	}
}

func TestCmdDoctor_JSONOutput(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(shared.StatusResponse{
			Version:       "0.1.0",
			UptimeSeconds: 60,
			ProxyAddr:     "localhost:8080",
			MITMEnabled:   false,
			RuleCount:     0,
			RequestCount:  0,
		})
	})
	s := httptest.NewServer(mux)
	defer s.Close()

	cfg := &clientConfig{TCP: s.Listener.Addr().String()}
	output := captureStdout(func() {
		if err := cmdDoctor(cfg, nil); err != nil {
			t.Fatal(err)
		}
	})

	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("expected JSON output, got: %s", output)
	}
	if !result.DaemonRunning {
		t.Error("expected DaemonRunning true")
	}
}

func TestCmdDoctor_FixFlag(t *testing.T) {
	// With --fix but daemon unreachable, it prints issues and attempts fixes.
	cfg := &clientConfig{TCP: "127.0.0.1:1"}
	err := cmdDoctor(cfg, []string{"--fix"})
	// Should not error on unreachable daemon.
	if err != nil {
		t.Fatalf("cmdDoctor --fix should not error on unreachable daemon: %v", err)
	}
}

func TestDoctorResult_AllPass(t *testing.T) {
	r := doctorResult{
		DaemonRunning:     true,
		ProxyListening:    true,
		ProxyAddr:         "localhost:8080",
		MITMConfigured:    true,
		MITMEnabled:       true,
		MITMCertExists:    true,
		MITMCertInstalled: true,
		RulesCount:        3,
		RequestsCaptured:  100,
	}
	r.Issues = append(r.Issues, "All checks passed.")
	printDoctorPretty(r)
}
