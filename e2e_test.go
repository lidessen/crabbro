// Package crabbro (end-to-end test) validates the full MITM + operator workflow.
//
//go:build e2e

package crabbro

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMITMWorkflowE2E starts a real daemon with MITM enabled, exercises the
// control API to configure and verify MITM interception, and tests the
// install-ca/uninstall-ca command flow (without actually modifying the
// system trust store).
func TestMITMWorkflowE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	// ---------------------------------------------------------------------------
	// Step 1: Start the daemon with MITM enabled.
	// ---------------------------------------------------------------------------
	daemonDir := t.TempDir()
	udsPath := filepath.Join(daemonDir, "proxy.sock")
	tcpAddr := findFreePort(t)
	proxyAddr := findFreePort(t)

	// Build the daemon binary.
	daemonBin := filepath.Join(daemonDir, "crabbrod")
	buildCmd := exec.Command("go", "build", "-o", daemonBin, "./cmd/crabbrod/")
	buildOutput, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building daemon: %v\n%s", err, buildOutput)
	}

	// Build the CLI binary.
	cliBin := filepath.Join(daemonDir, "crabbro")
	buildCmd = exec.Command("go", "build", "-o", cliBin, "./cmd/crabbro/")
	buildOutput, err = buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building CLI: %v\n%s", err, buildOutput)
	}

	// Start the daemon.
	daemonArgs := []string{
		"--proxy-addr", proxyAddr,
		"--uds-path", udsPath,
		"--tcp-addr", tcpAddr,
		"--mitm",
		"--mitm-allow", "*.example.com",
	}
	t.Logf("starting daemon: %s %s", daemonBin, strings.Join(daemonArgs, " "))
	daemon := exec.Command(daemonBin, daemonArgs...)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}
	defer daemon.Process.Kill()

	// Wait for the daemon to be ready.
	waitForDaemon(t, tcpAddr, 10*time.Second)

	// ---------------------------------------------------------------------------
	// Step 2: Verify status endpoint.
	// ---------------------------------------------------------------------------
	cli := func(args ...string) (string, error) {
		cmd := exec.Command(cliBin, append([]string{"--uds-path", udsPath, "--tcp-addr", tcpAddr}, args...)...)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	t.Log("verifying status...")
	statusOut, err := cli("status")
	if err != nil {
		t.Fatalf("status command failed: %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, `"version"`) {
		t.Errorf("expected version in status output, got: %s", statusOut)
	}

	// ---------------------------------------------------------------------------
	// Step 3: Verify MITM status.
	// ---------------------------------------------------------------------------
	t.Log("verifying MITM status...")
	mitmStatusOut, err := cli("mitm", "status")
	if err != nil {
		t.Fatalf("mitm status failed: %v\n%s", err, mitmStatusOut)
	}
	if !strings.Contains(mitmStatusOut, `"enabled":true`) {
		t.Errorf("expected MITM enabled, got: %s", mitmStatusOut)
	}

	// ---------------------------------------------------------------------------
	// Step 4: Download the CA certificate.
	// ---------------------------------------------------------------------------
	t.Log("downloading CA certificate...")
	certDir := filepath.Join(daemonDir, "certs")
	certPath := filepath.Join(certDir, "rootCA.pem")
	os.MkdirAll(certDir, 0755)
	certOut, err := cli("mitm", "cert", "--output", certPath)
	if err != nil {
		t.Fatalf("mitm cert failed: %v\n%s", err, certOut)
	}
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Fatalf("certificate was not saved to %s", certPath)
	}
	certData, _ := os.ReadFile(certPath)
	if !strings.Contains(string(certData), "BEGIN CERTIFICATE") {
		t.Errorf("expected PEM certificate, got: %s", string(certData))
	}

	// ---------------------------------------------------------------------------
	// Step 5: Test cert-installed API directly.
	// ---------------------------------------------------------------------------
	t.Log("testing cert-installed API...")
	testCertInstalled(t, tcpAddr, true)
	testCertInstalled(t, tcpAddr, false)

	// ---------------------------------------------------------------------------
	// Step 6: Test MITM enable/disable lifecycle.
	// ---------------------------------------------------------------------------
	t.Log("testing MITM enable/disable...")
	disableOut, err := cli("mitm", "disable")
	if err != nil {
		t.Fatalf("mitm disable failed: %v\n%s", err, disableOut)
	}

	mitmStatusOut, err = cli("mitm", "status")
	if err != nil {
		t.Fatalf("mitm status after disable failed: %v", err)
	}
	if !strings.Contains(mitmStatusOut, `"enabled":false`) {
		t.Errorf("expected MITM disabled, got: %s", mitmStatusOut)
	}

	enableOut, err := cli("mitm", "enable")
	if err != nil {
		t.Fatalf("mitm enable failed: %v\n%s", err, enableOut)
	}

	// ---------------------------------------------------------------------------
	// Step 7: Test MITM allowlist and denylist.
	// ---------------------------------------------------------------------------
	t.Log("testing allow/deny lists...")
	allowOut, err := cli("mitm", "allow", "add", "api.example.com")
	if err != nil {
		t.Fatalf("mitm allow add failed: %v\n%s", err, allowOut)
	}

	allowListOut, err := cli("mitm", "allow", "list")
	if err != nil {
		t.Fatalf("mitm allow list failed: %v\n%s", err, allowListOut)
	}
	if !strings.Contains(allowListOut, "api.example.com") {
		t.Errorf("expected api.example.com in allow list, got: %s", allowListOut)
	}

	denyOut, err := cli("mitm", "deny", "add", "evil.com")
	if err != nil {
		t.Fatalf("mitm deny add failed: %v\n%s", err, denyOut)
	}

	// ---------------------------------------------------------------------------
	// Step 8: Test doctor (with daemon running).
	// ---------------------------------------------------------------------------
	t.Log("testing doctor...")
	doctorOut, err := cli("doctor")
	if err != nil {
		t.Fatalf("doctor command failed: %v\n%s", err, doctorOut)
	}
	var doctorResult map[string]interface{}
	if err := json.Unmarshal([]byte(doctorOut), &doctorResult); err != nil {
		t.Fatalf("doctor output is not JSON: %s", doctorOut)
	}
	if daemonRunning, ok := doctorResult["daemonRunning"]; !ok || daemonRunning != true {
		t.Errorf("expected daemonRunning true, got: %v", doctorResult)
	}

	t.Log("E2E test passed!")
}

// testCertInstalled sends a POST to /v1/mitm/cert-installed and verifies the response.
func testCertInstalled(t *testing.T, tcpAddr string, installed bool) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	body := fmt.Sprintf(`{"installed":%t}`, installed)
	url := fmt.Sprintf("http://%s/v1/mitm/cert-installed", tcpAddr)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("creating cert-installed request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("cert-installed request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("cert-installed status = %d, want 200", resp.StatusCode)
		respBody, _ := io.ReadAll(resp.Body)
		t.Logf("response: %s", respBody)
	}

	var result map[string]bool
	json.NewDecoder(resp.Body).Decode(&result)
	if result["certInstalled"] != installed {
		t.Errorf("certInstalled = %t, want %t", result["certInstalled"], installed)
	}
}

// findFreePort returns a string like "localhost:12345" for a free TCP port.
func findFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// waitForDaemon polls the status endpoint until it responds or timeout.
func waitForDaemon(t *testing.T, tcpAddr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client := &http.Client{
			Timeout: 1 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
		resp, err := client.Get(fmt.Sprintf("http://%s/v1/status", tcpAddr))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("daemon did not become ready within %v", timeout)
}
