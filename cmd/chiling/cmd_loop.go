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

	"github.com/lidessen/chiling/internal/shared"
)

func cmdDaemon(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chiling daemon <start|stop>")
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
	fs.Parse(args)

	if err := cmdDaemonStop(cfg); err != nil && !os.IsNotExist(err) {
		// stop is best-effort when nothing is running
	}
	bin, err := findDaemonBin()
	if err != nil {
		return err
	}
	argv := []string{"--proxy-addr", *proxyAddr, "--uds-path", cfg.UDS, "--tcp-addr", cfg.TCP}
	if *mitmOn {
		argv = append(argv, "--mitm")
	}
	if *allow != "" {
		argv = append(argv, "--mitm-allow", *allow)
	}
	logPath := filepath.Join(filepath.Dir(cfg.UDS), "chilingd.log")
	if err := os.MkdirAll(filepath.Dir(cfg.UDS), 0700); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, argv...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	pidPath := filepath.Join(filepath.Dir(cfg.UDS), "chilingd.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := doRequest(cfg, http.MethodGet, "/v1/status", nil)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
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
	pidPath := filepath.Join(filepath.Dir(cfg.UDS), "chilingd.pid")
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(os.Interrupt)
	os.Remove(pidPath)
	fmt.Printf("{\"stopped\":true,\"pid\":%d}\n", pid)
	return nil
}

func findDaemonBin() (string, error) {
	self, err := os.Executable()
	if err == nil {
		cand := filepath.Join(filepath.Dir(self), "chilingd")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("chilingd"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("chilingd not found next to chiling or on PATH")
}

func cmdRecord(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chiling record <start|stop|status>")
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
	fs.Parse(args)
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: chiling find \"<what you are looking for>\"")
	}
	body, _ := json.Marshal(map[string]any{"query": query})
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
		return fmt.Errorf("usage: chiling verify replay <request-id> [--expect-status N] [--expect-body-contains TEXT]")
	}
	fs := flag.NewFlagSet("verify replay", flag.ContinueOnError)
	status := fs.Int("expect-status", 0, "Expected HTTP status")
	contains := fs.String("expect-body-contains", "", "Substring expected in the response body")
	fs.Parse(args[1:])
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: chiling verify replay <request-id>")
	}
	body, _ := json.Marshal(shared.VerifyRequest{
		RequestID:          fs.Arg(0),
		ExpectStatus:       *status,
		ExpectBodyContains: *contains,
	})
	return printAPI(cfg, http.MethodPost, "/v1/verify/replay", bytes.NewReader(body))
}

func cmdDecision(cfg *clientConfig, args []string) error {
	if len(args) > 0 && args[0] != "status" {
		return fmt.Errorf("usage: chiling decision status")
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
