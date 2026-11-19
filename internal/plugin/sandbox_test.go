package plugin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/rule"
)

// frameHost drives the Node host over its socket, one frame at a time.
type frameHost struct {
	t    *testing.T
	conn net.Conn
}

func startFrameHost(t *testing.T) *frameHost {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	dir, err := os.MkdirTemp("", "cp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := extractRuntime(dir); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "h.sock")
	cmd := exec.Command("node", filepath.Join(dir, "host.mjs"), sock)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var conn net.Conn
	for i := 0; i < 150 && conn == nil; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			conn = c
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if conn == nil {
		t.Fatal("host did not listen")
	}
	t.Cleanup(func() { conn.Close() })
	return &frameHost{t, conn}
}

func (h *frameHost) send(msg map[string]any) {
	body, _ := json.Marshal(msg)
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(body)))
	h.conn.Write(head[:])
	h.conn.Write(body)
}

// recv is the next frame that is not a log or a heartbeat, nil on timeout.
func (h *frameHost) recv(d time.Duration) map[string]any {
	h.conn.SetReadDeadline(time.Now().Add(d))
	for {
		var n uint32
		if err := binary.Read(h.conn, binary.BigEndian, &n); err != nil {
			return nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(h.conn, buf); err != nil {
			return nil
		}
		var m map[string]any
		_ = json.Unmarshal(buf, &m)
		if m["type"] == "log" || m["type"] == "heartbeat" {
			continue
		}
		return m
	}
}

func (h *frameHost) activate(id, pkg, entry, isolation string) {
	h.send(map[string]any{"type": "activate", "id": id, "package": pkg, "entry": entry, "isolation": isolation, "heapMb": 64})
	if r := h.recv(3 * time.Second); r == nil || r["error"] != nil {
		h.t.Fatalf("activate: %v", r)
	}
}

func (h *frameHost) call(id, pkg, capability string, wait time.Duration) map[string]any {
	h.send(map[string]any{"type": "uses", "id": id, "package": pkg, "capability": capability, "with": map[string]any{}})
	return h.recv(wait)
}

func fixture(t *testing.T, name string) string {
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "plugins", name, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Nothing the loader and preload close should get through: not the blocked
// built-ins, not the ones a denylist forgets, not createRequire, and not a
// helper file that borrows a host file's name.
func TestPluginSandboxClosesKnownEscapes(t *testing.T) {
	h := startFrameHost(t)
	h.activate("a", "sandbox", fixture(t, "sandbox"), "worker")
	r := h.call("c", "sandbox", "probe", 3*time.Second)
	result, _ := r["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result: %v", r)
	}
	outputs, _ := result["outputs"].(map[string]any)
	if through, _ := outputs["through"].([]any); len(through) > 0 {
		t.Errorf("escapes still open: %v", through)
	}
}

// A worker that died (a throw on a timer, the heap limit) must be replaced,
// not left in the table to time out every later call.
func TestCrashedWorkerIsReplaced(t *testing.T) {
	h := startFrameHost(t)
	entry := fixture(t, "crashy")
	h.activate("a1", "crashy", entry, "worker")
	if r := h.call("c1", "crashy", "ok", 2*time.Second); r == nil || r["error"] != nil {
		t.Fatalf("healthy call: %v", r)
	}
	h.call("c2", "crashy", "die", 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	// The daemon activates before every call.
	h.activate("a2", "crashy", entry, "worker")
	if r := h.call("c3", "crashy", "ok", 2*time.Second); r == nil || r["error"] != nil {
		t.Fatalf("plugin did not come back after a crash: %v", r)
	}
}

// managerWith is a Manager whose state holds the crashy fixture as an
// installed, enabled package with one instance.
func managerWith(t *testing.T) *Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	dir := filepath.Dir(fixture(t, "crashy"))
	m.state.Packages = []PackageRecord{{ID: "crashy", Path: dir, Entry: "index.js", Isolation: "worker", Enabled: true}}
	m.state.Instances = []InstanceRecord{{Plugin: "crashy", Name: "local", Enabled: true}}
	return m
}

func invoke(m *Manager, capability string, timeout time.Duration) (rule.InvokeResult, error) {
	return m.Invoke(context.Background(), rule.InvokeRequest{
		Plugin: "crashy", Instance: "local", Capability: capability, Kind: "uses", Timeout: timeout,
	})
}

// One plugin call in flight must not hold up another: the second returns
// long before the first finishes.
func TestPluginCallsRunConcurrently(t *testing.T) {
	m := managerWith(t)
	if _, err := invoke(m, "ok", 2*time.Second); err != nil { // start the host and worker
		t.Fatal(err)
	}
	slowDone := make(chan error, 1)
	go func() { _, err := invoke(m, "slow", 2*time.Second); slowDone <- err }()
	time.Sleep(80 * time.Millisecond)
	start := time.Now()
	if _, err := invoke(m, "ok", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Errorf("a fast call waited %v behind a slow one", took)
	}
	if err := <-slowDone; err != nil {
		t.Errorf("slow call: %v", err)
	}
}

// A plugin that crashed is started again by the next call.
func TestInvokeRestartsCrashedPlugin(t *testing.T) {
	m := managerWith(t)
	if _, err := invoke(m, "die", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := invoke(m, "ok", 2*time.Second); err != nil {
		t.Fatalf("after a crash: %v", err)
	}
}

// A caller that goes away must not stop the plugin for everyone else.
func TestCanceledCallDoesNotTerminatePlugin(t *testing.T) {
	m := managerWith(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := m.Invoke(ctx, rule.InvokeRequest{Plugin: "crashy", Instance: "local", Capability: "slow", Kind: "uses", Timeout: 2 * time.Second}); err == nil {
		t.Fatal("canceled call returned no error")
	}
	if _, err := invoke(m, "ok", 2*time.Second); err != nil {
		t.Fatalf("plugin unusable after a cancel: %v", err)
	}
}
