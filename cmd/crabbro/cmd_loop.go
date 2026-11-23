package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

func cmdDaemon(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro daemon <start|stop>")
	}
	switch args[0] {
	case "start":
		return cmdDaemonStart(cfg, args[1:])
	case "stop":
		return cmdDaemonStop(cfg)
	default:
		return fmt.Errorf("unknown daemon command: %s", args[0])
	}
}

func cmdDaemonStart(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	mitmOn := fs.Bool("mitm", false, "Enable HTTPS MITM")
	allow := fs.String("mitm-allow", "", "Comma-separated MITM allow patterns")
	proxyAddr := fs.String("proxy-addr", shared.DefaultProxyAddr, "Proxy listen address")
	gatewayAddr := fs.String("gateway-addr", shared.DefaultGatewayAddr, "Gateway listen address ('off' disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := stopDaemon(cfg); err != nil {
		return fmt.Errorf("stop existing daemon: %w", err)
	}
	bin, err := findDaemonBin()
	if err != nil {
		return err
	}
	argv := []string{"--proxy-addr", *proxyAddr, "--uds-path", cfg.UDS, "--tcp-addr", cfg.TCP}
	if *gatewayAddr != "" && *gatewayAddr != shared.DefaultGatewayAddr {
		argv = append(argv, "--gateway-addr", *gatewayAddr)
	}
	if *mitmOn {
		argv = append(argv, "--mitm")
	}
	if *allow != "" {
		argv = append(argv, "--mitm-allow", *allow)
	}
	logPath := filepath.Join(filepath.Dir(cfg.UDS), "crabbrod.log")
	if err := os.MkdirAll(filepath.Dir(cfg.UDS), 0700); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, argv...)
	detachDaemon(cmd)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	defer logf.Close()
	defer cmd.Process.Release()
	ready := false
	defer func() {
		if !ready {
			_ = cmd.Process.Kill()
		}
	}()
	pidPath := filepath.Join(filepath.Dir(cfg.UDS), "crabbrod.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := doRequest(cfg, http.MethodGet, "/v1/status", nil)
		if err == nil {
			var status shared.StatusResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && status.PID == cmd.Process.Pid {
				ready = true
				fmt.Printf("{\"pid\":%d,\"proxyAddr\":%q,\"uds\":%q}\n", cmd.Process.Pid, *proxyAddr, cfg.UDS)
				return nil
			}
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	if last != nil {
		return fmt.Errorf("daemon did not become ready: %w", last)
	}
	return fmt.Errorf("daemon did not become ready")
}

func cmdDaemonStop(cfg *clientConfig) error {
	return stopDaemon(cfg)
}

// stopDaemon only signals the process identified by the local PID file. A
// current daemon confirms its PID over the control API; older releases are
// checked by process name so an unrelated process is never signaled blindly.
func stopDaemon(cfg *clientConfig) error {
	pidPath := filepath.Join(filepath.Dir(cfg.UDS), "crabbrod.pid")
	b, err := os.ReadFile(pidPath)
	if os.IsNotExist(err) {
		if _, err := daemonStatus(cfg); err == nil {
			return fmt.Errorf("daemon is running without a PID file; stop it using its process manager")
		}
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid daemon PID file %s", pidPath)
	}
	status, statusErr := daemonStatus(cfg)
	if statusErr == nil && status.PID != 0 && status.PID != pid {
		return fmt.Errorf("daemon PID file identifies %d, but control API reports %d", pid, status.PID)
	}
	if statusErr != nil || status.PID == 0 {
		name, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || statusErr == nil {
				return fmt.Errorf("cannot identify daemon PID %d: %w", pid, err)
			}
			return os.Remove(pidPath)
		}
		if filepath.Base(strings.TrimSpace(string(name))) != "crabbrod" {
			return fmt.Errorf("PID %d is not crabbrod", pid)
		}
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("signal daemon %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := daemonStatus(cfg); err != nil {
			running, err := daemonProcessRunning(pid)
			if err != nil {
				return err
			}
			if !running {
				if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
					return err
				}
				fmt.Printf("{\"stopped\":true,\"pid\":%d}\n", pid)
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon %d did not stop", pid)
}

func daemonProcessRunning(pid int) (bool, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("check daemon PID %d: %w", pid, err)
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z"), nil
}

func daemonStatus(cfg *clientConfig) (shared.StatusResponse, error) {
	var status shared.StatusResponse
	resp, err := doRequest(cfg, http.MethodGet, "/v1/status", nil)
	if err != nil {
		return status, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("daemon status: %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&status)
	return status, err
}

func findDaemonBin() (string, error) {
	self, _ := os.Executable()
	return findDaemonBinFrom(self)
}

func findDaemonBinFrom(self string) (string, error) {
	if self != "" {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		cand := filepath.Join(filepath.Dir(self), "crabbrod")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("crabbrod"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("crabbrod not found next to crabbro or on PATH")
}

func cmdRecord(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro record <start|stop|status>")
	}
	switch args[0] {
	case "start":
		name := ""
		fs := flag.NewFlagSet("record start", flag.ContinueOnError)
		fs.StringVar(&name, "name", "", "Session name")
		fs.Parse(args[1:])
		body, _ := json.Marshal(map[string]string{"name": name})
		return printAPI(cfg, http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	case "stop":
		cur, err := getJSON(cfg, "/v1/sessions/current")
		if err != nil {
			return err
		}
		sess, _ := cur["session"].(map[string]any)
		if sess == nil || sess["id"] == nil {
			return fmt.Errorf("no recording session is active")
		}
		id, _ := sess["id"].(string)
		return printAPI(cfg, http.MethodPost, "/v1/sessions/"+id+"/stop", nil)
	case "status":
		return printAPI(cfg, http.MethodGet, "/v1/sessions/current", nil)
	default:
		return fmt.Errorf("unknown record command: %s", args[0])
	}
}

func cmdFind(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("find", flag.ContinueOnError)
	pretty := fs.Bool("pretty", false, "Human-readable output")
	typ := fs.String("type", "", "Only flows of this resource type")
	initiator := fs.String("initiator", "", "Only flows with this initiator URL")
	fs.Parse(args)
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: crabbro find \"<what you are looking for>\"")
	}
	body, _ := json.Marshal(map[string]any{"query": query, "type": *typ, "initiator": *initiator})
	resp, err := doRequest(cfg, http.MethodPost, "/v1/find", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	if !*pretty {
		fmt.Println(string(raw))
		return nil
	}
	var parsed sharedFind
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	if len(parsed.Hits) == 0 {
		fmt.Println("no matching flows")
		return nil
	}
	for _, h := range parsed.Hits {
		fmt.Printf("%s  score=%.2f  %s\n", h.RequestID, h.Score, h.Reason)
	}
	return nil
}

type sharedFind struct {
	Hits []struct {
		RequestID string  `json:"requestId"`
		Score     float64 `json:"score"`
		Reason    string  `json:"reason"`
	} `json:"hits"`
}

func cmdVerify(cfg *clientConfig, args []string) error {
	if len(args) < 1 || args[0] != "replay" {
		return fmt.Errorf("usage: crabbro verify replay <request-id> [--expect-status N] [--expect-body-contains TEXT]")
	}
	fs := flag.NewFlagSet("verify replay", flag.ContinueOnError)
	status := fs.Int("expect-status", 0, "Expected HTTP status")
	contains := fs.String("expect-body-contains", "", "Substring expected in the response body")
	fs.Parse(args[1:])
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: crabbro verify replay <request-id>")
	}
	body, _ := json.Marshal(shared.VerifyRequest{
		RequestID:          fs.Arg(0),
		ExpectStatus:       *status,
		ExpectBodyContains: *contains,
	})
	return printAPI(cfg, http.MethodPost, "/v1/verify/replay", bytes.NewReader(body))
}

func cmdHistory(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: crabbro history <status|prune>")
	}
	switch args[0] {
	case "status":
		return printAPI(cfg, http.MethodGet, "/v1/history", nil)
	case "prune":
		return printAPI(cfg, http.MethodPost, "/v1/history/prune", nil)
	default:
		return fmt.Errorf("unknown history command: %s", args[0])
	}
}

func cmdDecision(cfg *clientConfig, args []string) error {
	if len(args) > 0 && args[0] != "status" {
		return fmt.Errorf("usage: crabbro decision status")
	}
	return printAPI(cfg, http.MethodGet, "/v1/decision", nil)
}

func printAPI(cfg *clientConfig, method, path string, body io.Reader) error {
	resp, err := doRequest(cfg, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func getJSON(cfg *clientConfig, path string) (map[string]any, error) {
	resp, err := doRequest(cfg, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
