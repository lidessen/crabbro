package plugin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lidessen/crabbro/internal/decision"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
)

//go:embed js/*.mjs
var runtimeFS embed.FS

// ErrNoNode is returned when the plugin host cannot be started.
var ErrNoNode = errors.New("node is not installed")

// Manager installs plugins and supervises one Node host.
type Manager struct {
	Dir       string
	Secrets   *secret.Store
	Decision  *decision.Client
	Heartbeat time.Duration
	IdleGrace time.Duration
	HeapMB    int

	mu        sync.Mutex
	pendingMu sync.Mutex
	state     diskState
	cmd       *exec.Cmd
	conn      net.Conn
	pending   map[string]chan map[string]any
	active    map[string]bool // packages the host has been told to start, this host run
	writeMu   sync.Mutex
	lastBeat  time.Time
	misses    int
	rss       int64
	workers   int
	seq       int
	closed    bool
	sock      string
	sockDir   string
	stopBeat  chan struct{}
	stderr    bytes.Buffer
}

// ExecHost runs the Node host bound to sock. It does not return until the process exits.
func ExecHost(sock string) error {
	node, err := exec.LookPath("node")
	if err != nil {
		return ErrNoNode
	}
	dir, err := os.MkdirTemp("", "crabbro-host-")
	if err != nil {
		return err
	}
	if err := extractRuntime(dir); err != nil {
		return err
	}
	cmd := exec.Command(node, filepath.Join(dir, "host.mjs"), sock)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	return cmd.Run()
}

// NewManager opens or creates a plugin directory backed by the shared secret store.
func NewManager(dir string, secrets *secret.Store) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if secrets == nil {
		secrets = secret.New(filepath.Join(filepath.Dir(dir), "secrets"))
	}
	m := &Manager{
		Dir:       dir,
		Secrets:   secrets,
		Heartbeat: 2 * time.Second,
		IdleGrace: 30 * time.Second,
		HeapMB:    64,
		pending:   map[string]chan map[string]any{},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Close stops the host process.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.stopLocked()
}

func (m *Manager) stopLocked() {
	if m.stopBeat != nil {
		close(m.stopBeat)
		m.stopBeat = nil
	}
	if m.conn != nil {
		_ = m.writeTo(m.conn, map[string]any{"type": "shutdown", "id": "shutdown"})
		_ = m.conn.Close()
		m.conn = nil
		m.active = nil
	}
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_, _ = m.cmd.Process.Wait()
		m.cmd = nil
	}
	if m.sockDir != "" {
		_ = os.RemoveAll(m.sockDir)
		m.sockDir = ""
	}
}

// HostStatus is the supervisor view. It does not start Node.
type HostStatus struct {
	Running  bool  `json:"running"`
	PID      int   `json:"pid,omitempty"`
	RSSBytes int64 `json:"rssBytes,omitempty"`
	Workers  int   `json:"workers"`
}

func (m *Manager) Status() HostStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := HostStatus{Running: m.cmd != nil && m.cmd.Process != nil, RSSBytes: m.rss, Workers: m.workers}
	if st.Running {
		st.PID = m.cmd.Process.Pid
	}
	return st
}

// List does not start a worker.
func (m *Manager) List() []PackageRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]PackageRecord(nil), m.state.Packages...)
	return out
}

func (m *Manager) Instances(pluginID string) []InstanceRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []InstanceRecord
	for _, in := range m.state.Instances {
		if pluginID == "" || in.Plugin == pluginID {
			cp := in
			cp.Credentials = map[string]string{}
			for k := range in.Credentials {
				cp.Credentials[k] = k
			}
			out = append(out, cp)
		}
	}
	return out
}

// HasInstance implements rule.Runtime.
func (m *Manager) HasInstance(pluginID, instance string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.instanceByName(pluginID, instance)
	return in != nil && in.Enabled
}

// Secret implements rule.Runtime. The value is read from the shared secret store.
func (m *Manager) Secret(pluginID, instance, name string) (string, error) {
	m.mu.Lock()
	in := m.instanceByName(pluginID, instance)
	var ref string
	if in != nil {
		ref = in.Credentials[name]
	}
	secrets := m.Secrets
	m.mu.Unlock()
	if ref == "" {
		ref = name
	}
	if secrets == nil {
		return "", fmt.Errorf("secret store is not configured")
	}
	return secrets.Get(ref)
}

// Invoke implements rule.Runtime.
func (m *Manager) Invoke(ctx context.Context, req rule.InvokeRequest) (rule.InvokeResult, error) {
	if err := m.ensureHost(); err != nil {
		return rule.InvokeResult{}, err
	}
	m.mu.Lock()
	pkg := m.packageByID(req.Plugin)
	inst := m.instanceByName(req.Plugin, req.Instance)
	var rec PackageRecord
	var cfg map[string]any
	var creds map[string]string
	if pkg != nil {
		rec = *pkg
	}
	if inst != nil {
		cfg = inst.Config
		creds = inst.Credentials
	}
	m.mu.Unlock()
	if pkg == nil || inst == nil || !inst.Enabled {
		return rule.InvokeResult{}, fmt.Errorf("instance is not installed")
	}
	secrets := map[string]string{}
	for name := range creds {
		if v, err := m.Secret(req.Plugin, req.Instance, name); err == nil {
			secrets[name] = v
		}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	kind := req.Kind
	if kind == "" {
		kind = "uses"
	}
	// A plugin sees the instance's credentials only if its manifest asked
	// for secrets:read. Every credential is still scrubbed from what it
	// returns, whether or not it was handed over.
	visible := map[string]string{}
	if hasPerm(rec.Permissions, "secrets:read") {
		visible = secrets
	}
	msg := map[string]any{
		"type": kind, "package": rec.ID, "instance": req.Instance, "capability": req.Capability,
		"hook": req.Capability, "with": req.With, "env": req.Env, "payload": req.Payload,
		"config": cfg, "secrets": visible, "arguments": req.With,
	}
	// The host may have dropped the worker since the last call (idle sweep,
	// crash). It says so at once; start it again and ask once more.
	var reply map[string]any
	for attempt := 0; ; attempt++ {
		if err := m.activate(rec, cfg); err != nil {
			return rule.InvokeResult{}, err
		}
		var err error
		call := map[string]any{}
		for k, v := range msg {
			call[k] = v
		}
		reply, err = m.roundTrip(ctx, call, timeout)
		if err != nil {
			// A caller that gave up is not the plugin's fault; a plugin
			// that overran its deadline is stopped.
			if !errors.Is(err, context.Canceled) {
				_ = m.terminate(rec.ID)
			}
			return rule.InvokeResult{}, fmt.Errorf("%s: %w", kind, err)
		}
		errMsg, _ := reply["error"].(string)
		if attempt == 0 && (errMsg == "plugin is not active" || errMsg == "plugin exited") {
			m.forget(rec.ID)
			continue
		}
		if errMsg != "" {
			return rule.InvokeResult{}, fmt.Errorf("plugin failed")
		}
		break
	}
	result, _ := reply["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	outputs, _ := result["outputs"].(map[string]any)
	actions, err := rule.ValidateRawActions(result["actions"])
	if err != nil {
		return rule.InvokeResult{}, err
	}
	effect, _ := reply["effect"].(string)
	if effect == "" {
		effect = "mutate"
	}
	for i := range actions {
		actions[i] = scrubAction(actions[i], secrets)
	}
	return rule.InvokeResult{Outputs: scrubMap(outputs, secrets), Actions: actions, Effect: effect, Match: result["match"] != false}, nil
}

// Describe starts the worker and returns catalogs. list does not call this.
func (m *Manager) Describe(id string) (map[string]any, error) {
	if err := m.ensureHost(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	pkg := m.packageByID(id)
	var rec PackageRecord
	if pkg != nil {
		rec = *pkg
	}
	m.mu.Unlock()
	if pkg == nil {
		return nil, fmt.Errorf("plugin is not installed")
	}
	if err := m.activate(rec, nil); err != nil {
		return nil, err
	}
	reply, err := m.roundTrip(context.Background(), map[string]any{"type": "describe", "package": id}, 2*time.Second)
	if err != nil {
		return nil, err
	}
	desc, _ := reply["describe"].(map[string]any)
	if desc == nil {
		return map[string]any{}, nil
	}
	return desc, nil
}

func (m *Manager) activate(rec PackageRecord, cfg map[string]any) error {
	m.mu.Lock()
	known := m.active[rec.ID]
	m.mu.Unlock()
	if known {
		return nil
	}
	_, err := m.roundTrip(context.Background(), map[string]any{
		"type": "activate", "package": rec.ID, "entry": filepath.Join(rec.Path, rec.Entry),
		"isolation": rec.Isolation, "heapMb": m.HeapMB, "permissions": rec.Permissions, "config": cfg,
	}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	m.mu.Lock()
	if m.active != nil {
		m.active[rec.ID] = true
	}
	m.mu.Unlock()
	return nil
}

// forget drops what the manager believes about a package being started.
func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.active, id)
	m.mu.Unlock()
}

func (m *Manager) terminate(id string) error {
	m.forget(id)
	_, err := m.roundTrip(context.Background(), map[string]any{"type": "terminate", "package": id}, time.Second)
	return err
}

func (m *Manager) ensureHost() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil && m.cmd != nil && m.cmd.ProcessState == nil {
		return nil
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return ErrNoNode
	}
	dir := filepath.Join(m.Dir, "runtime")
	if err := extractRuntime(dir); err != nil {
		return err
	}
	// The socket lives in a directory only this user can enter (MkdirTemp
	// makes it 0700), so its own mode and the moment it is changed do not
	// matter, on any platform.
	sockDir, err := os.MkdirTemp("", "crabbro-host-")
	if err != nil {
		return err
	}
	sock := filepath.Join(sockDir, "host.sock")
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		_ = os.RemoveAll(sockDir)
		return err
	}
	token := hex.EncodeToString(raw[:])
	cmd := exec.Command(node, filepath.Join(dir, "host.mjs"), sock)
	m.stderr.Reset()
	cmd.Stderr = &m.stderr
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"CRABBRO_PLUGIN_HEARTBEAT_MS=" + strconv.Itoa(int(m.Heartbeat/time.Millisecond)),
		"CRABBRO_PLUGIN_TOKEN=" + token,
	}
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(sockDir)
		return err
	}
	m.cmd = cmd
	m.sockDir = sockDir
	deadline := time.Now().Add(5 * time.Second)
	var conn net.Conn
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(sockDir)
			return fmt.Errorf("plugin host did not listen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.conn = conn
	m.sock = sock
	m.pending = map[string]chan map[string]any{}
	m.active = map[string]bool{}
	go m.readLoop(conn, m.pending)
	m.lastBeat = time.Now()
	m.misses = 0
	m.stopBeat = make(chan struct{})
	go m.watch(m.stopBeat, cmd)
	m.mu.Unlock()
	_, err = m.roundTrip(context.Background(), map[string]any{"type": "hello", "api": "1.0.0", "token": token}, 2*time.Second)
	m.mu.Lock()
	if err != nil {
		m.stopLocked()
		return err
	}
	return nil
}

func (m *Manager) watch(stop chan struct{}, cmd *exec.Cmd) {
	tick := time.NewTicker(m.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			m.mu.Lock()
			if m.cmd != cmd {
				m.mu.Unlock()
				return
			}
			missed := time.Since(m.lastBeat) > 3*m.Heartbeat
			rss := rssKB(cmd.Process.Pid)
			tooBig := rss > 512*1024
			m.mu.Unlock()
			if missed || tooBig {
				m.mu.Lock()
				if m.cmd == cmd {
					m.stopLocked()
				}
				m.mu.Unlock()
				return
			}
		}
	}
}

// readLoop is the only reader of the host connection. It hands each reply
// to the call waiting for its id, so calls to different plugins run side by
// side, and it fails every waiter if the connection ends.
func (m *Manager) readLoop(conn net.Conn, pending map[string]chan map[string]any) {
	defer func() {
		m.pendingMu.Lock()
		for id, ch := range pending {
			close(ch)
			delete(pending, id)
		}
		m.pendingMu.Unlock()
		// A host that went away on its own is replaced on the next call,
		// rather than left in place with a dead socket.
		m.mu.Lock()
		if m.conn == conn && !m.closed {
			m.stopLocked()
		}
		m.mu.Unlock()
	}()
	r := bufio.NewReader(conn)
	for {
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return
		}
		if n == 0 || n > 8*1024*1024 {
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(buf, &msg); err != nil {
			continue
		}
		switch msg["type"] {
		case "heartbeat":
			m.noteHeartbeat(msg)
		case "broker":
			go m.handleBroker(conn, msg)
		case "log":
			// Operator log. Not a trace, and not forwarded with secrets.
		default:
			id, _ := msg["id"].(string)
			m.pendingMu.Lock()
			ch := pending[id]
			m.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
		}
	}
}

func (m *Manager) handleBroker(conn net.Conn, msg map[string]any) {
	op, _ := msg["op"].(string)
	pkg, _ := msg["package"].(string)
	id, _ := msg["id"].(string)
	payload, _ := msg["payload"].(map[string]any)
	delete(payload, "apiKey")
	delete(payload, "endpoint")
	delete(payload, "authorization")
	delete(payload, "headers")
	var body any
	var err error
	switch op {
	case "decision.ask":
		body, err = m.decisionAsk(payload)
	case "history.list", "history.get":
		err = fmt.Errorf("permission denied")
	case "net.fetch":
		err = fmt.Errorf("permission denied")
	case "fs.read", "fs.write":
		body, err = m.brokerFS(pkg, op, payload)
	case "secrets.get":
		if !m.packageHasPerm(pkg, "secrets:read") {
			err = fmt.Errorf("permission denied")
			break
		}
		name, _ := payload["name"].(string)
		instance, _ := payload["instance"].(string)
		if instance == "" {
			instance = "local"
		}
		v, secErr := m.Secret(pkg, instance, name)
		if secErr != nil {
			err = fmt.Errorf("secret unavailable")
		} else {
			body = map[string]any{"value": v}
		}
	default:
		err = fmt.Errorf("unknown broker op")
	}
	reply := map[string]any{"type": "brokerResult", "id": id, "package": pkg, "payload": body}
	if err != nil {
		reply["error"] = "broker failed"
		reply["payload"] = nil
	}
	_ = m.writeTo(conn, reply)
}

func (m *Manager) decisionAsk(payload map[string]any) (any, error) {
	if m.Decision == nil {
		return map[string]any{"decisionDiagnostic": decision.Diagnostic{Category: "not_configured"}}, decision.ErrNotConfigured
	}
	prompt, _ := payload["prompt"].(string)
	kind, _ := payload["type"].(string)
	var options []string
	if raw, ok := payload["options"].([]any); ok {
		for _, o := range raw {
			if s, ok := o.(string); ok {
				options = append(options, s)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := m.Decision.Ask(ctx, prompt, kind, options)
	out := map[string]any{
		"choice": res.Choice, "text": res.Text, "decisionDiagnostic": res.Diagnostic,
		"model": res.Model, "provider": res.Provider, "calibrated": res.Calibrated,
	}
	if res.Mode != "" {
		out["mode"] = res.Mode
	}
	if res.Noul != nil {
		out["noul"] = *res.Noul
	}
	if res.Score != nil {
		out["score"] = *res.Score
	}
	return out, err
}

// packageHasPerm reports whether an installed package declared a permission.
func (m *Manager) packageHasPerm(pkg, perm string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.packageByID(pkg)
	return rec != nil && hasPerm(rec.Permissions, perm)
}

func (m *Manager) brokerFS(pkg, op string, payload map[string]any) (any, error) {
	m.mu.Lock()
	rec := m.packageByID(pkg)
	var perms []string
	if rec != nil {
		perms = rec.Permissions
	}
	m.mu.Unlock()
	if !hasPerm(perms, "fs:data") {
		return nil, fmt.Errorf("permission denied")
	}
	rel, _ := payload["path"].(string)
	if rel == "" || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("invalid path")
	}
	base := filepath.Join(m.Dir, "data", pkg)
	full := filepath.Join(base, rel)
	if !strings.HasPrefix(full, base+string(os.PathSeparator)) && full != base {
		return nil, fmt.Errorf("invalid path")
	}
	if op == "fs.read" {
		b, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": string(b)}, nil
	}
	data, _ := payload["data"].(string)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// roundTrip sends one frame and waits for the reply with its id. Only the
// send and the bookkeeping are serialized; the wait is not, so a slow plugin
// does not hold up another. The caller's context and the timeout both end
// the wait.
func (m *Manager) roundTrip(ctx context.Context, msg map[string]any, timeout time.Duration) (map[string]any, error) {
	m.mu.Lock()
	conn, pending := m.conn, m.pending
	if conn == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("plugin host is not running")
	}
	m.seq++
	id := fmt.Sprintf("m%d", m.seq)
	m.mu.Unlock()
	msg["id"] = id

	ch := make(chan map[string]any, 1)
	m.pendingMu.Lock()
	pending[id] = ch
	m.pendingMu.Unlock()
	defer func() {
		m.pendingMu.Lock()
		delete(pending, id)
		m.pendingMu.Unlock()
	}()

	if err := m.writeTo(conn, msg); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case frame, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("plugin host exited")
		}
		return frame, nil
	case <-timer.C:
		return nil, fmt.Errorf("plugin timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) noteHeartbeat(frame map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastBeat = time.Now()
	m.misses = 0
	if f, ok := frame["rssBytes"].(float64); ok {
		m.rss = int64(f)
	}
	if f, ok := frame["workers"].(float64); ok {
		m.workers = int(f)
	}
}

// writeTo sends one length-prefixed frame. One writer at a time, so frames
// from concurrent calls do not interleave.
func (m *Manager) writeTo(conn net.Conn, msg map[string]any) error {
	if conn == nil {
		return fmt.Errorf("plugin host is not running")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	_, err = conn.Write(frame)
	return err
}

func extractRuntime(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := fs.ReadDir(runtimeFS, "js")
	if err != nil {
		return err
	}
	for _, ent := range entries {
		b, err := fs.ReadFile(runtimeFS, "js/"+ent.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, ent.Name()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func rssKB(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				return n
			}
		}
	}
	return 0
}

func secretValues(secrets map[string]string) []string {
	if len(secrets) == 0 {
		return nil
	}
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if v != "" {
			vals = append(vals, v)
		}
	}
	return vals
}

func scrubAction(a shared.RuleAction, secrets map[string]string) shared.RuleAction {
	if a.Params == nil {
		return a
	}
	vals := secretValues(secrets)
	if len(vals) == 0 {
		return a
	}
	if m, ok := secret.ScrubAny(a.Params, vals...).(map[string]any); ok {
		a.Params = m
	}
	return a
}

func scrubMap(m map[string]any, secrets map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	vals := secretValues(secrets)
	if len(vals) == 0 {
		return m
	}
	if out, ok := secret.ScrubAny(m, vals...).(map[string]any); ok {
		return out
	}
	return map[string]any{}
}

// Hooks runs onRequest for enabled plugins whose static match passes.
// Registration alone does not insert a plugin. The caller decides to run hooks.
func (m *Manager) Hooks(ctx context.Context, phase string, req *shared.Request, body string) ([]shared.RuleAction, []shared.RuleTrace) {
	m.mu.Lock()
	pkgs := append([]PackageRecord(nil), m.state.Packages...)
	m.mu.Unlock()
	var actions []shared.RuleAction
	var traces []shared.RuleTrace
	hook := "onRequest"
	if phase == "response" {
		hook = "onResponse"
	}
	for _, pkg := range pkgs {
		if !pkg.Enabled || !contains(pkg.Activation, "onRequest") && phase == "request" {
			continue
		}
		if phase == "request" && !matchStatic(pkg.Match, req) {
			continue
		}
		inst := "local"
		if !m.HasInstance(pkg.ID, inst) {
			continue
		}
		res, err := m.Invoke(ctx, rule.InvokeRequest{
			Plugin: pkg.ID, Instance: inst, Capability: hook, Kind: "hook",
			Payload: map[string]any{"request": map[string]any{"method": req.Method, "host": req.Host, "path": req.Path, "body": body}},
			Timeout: shared.DefaultRuleTimeout, ExchangeID: string(req.ID),
		})
		tr := shared.RuleTrace{RuleID: shared.RuleID("plugin:" + pkg.ID), Phase: phase, Matched: err == nil && res.Match}
		if err != nil {
			tr.Error = "plugin failed"
			tr.Matched = false
		} else {
			tr.Actions = res.Actions
			actions = append(actions, res.Actions...)
		}
		traces = append(traces, tr)
	}
	return actions, traces
}

func matchStatic(match Match, req *shared.Request) bool {
	if req == nil {
		return true
	}
	if len(match.Methods) > 0 {
		ok := false
		for _, method := range match.Methods {
			if strings.EqualFold(method, req.Method) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if match.PathPrefix != "" && !strings.HasPrefix(req.Path, match.PathPrefix) {
		return false
	}
	if len(match.Hosts) == 0 {
		return true
	}
	host := req.Host
	if i := strings.LastIndex(host, ":"); i > 0 && strings.Count(host, ":") == 1 {
		host = host[:i]
	}
	for _, pattern := range match.Hosts {
		if pattern == "*" || strings.EqualFold(pattern, host) {
			return true
		}
		if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
