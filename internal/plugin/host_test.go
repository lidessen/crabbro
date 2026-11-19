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
	"github.com/lidessen/crabbro/internal/secret"
)

func TestHostFrames(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	if err := extractRuntime(dir); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "h.sock")
	cmd := exec.Command("node", filepath.Join(dir, "host.mjs"), sock)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	var conn net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()
	write := func(msg map[string]any) {
		body, _ := json.Marshal(msg)
		var head [4]byte
		binary.BigEndian.PutUint32(head[:], uint32(len(body)))
		conn.Write(head[:])
		conn.Write(body)
	}
	read := func() map[string]any {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var n uint32
		if err := binary.Read(conn, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(buf, &msg); err != nil {
			t.Fatal(err)
		}
		return msg
	}
	write(map[string]any{"type": "hello", "id": "h"})
	if read()["type"] != "hello" {
		t.Fatal("hello")
	}
	entry, err := filepath.Abs(filepath.Join("..", "..", "testdata", "plugins", "hmac", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	write(map[string]any{"type": "activate", "id": "a", "package": "hmac", "entry": entry, "isolation": "worker", "heapMb": 64})
	msg := read()
	if msg["id"] != "a" {
		t.Fatalf("activate reply %#v", msg)
	}
}

func TestInstallAndSign(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed")
	}
	secStore := secret.New(t.TempDir())
	m, err := NewManager(t.TempDir(), secStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	src := filepath.Join("..", "..", "testdata", "plugins", "hmac")
	if _, err := m.Install(src, true); err != nil {
		t.Fatal(err)
	}
	// list does not require the host.
	if st := m.Status(); st.Running {
		t.Fatal("install started the host")
	}
	if err := secStore.Set("partner", "topsecret"); err != nil {
		t.Fatal(err)
	}
	if err := m.AddInstance("hmac", "primary", nil, map[string]string{"partner_hmac": "partner"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.Invoke(context.Background(), rule.InvokeRequest{
		Plugin: "hmac", Instance: "primary", Capability: "sign", Kind: "uses",
		Payload: map[string]any{"request": map[string]any{"method": "POST", "path": "/v1/pay"}},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	header, _ := res.Outputs["header"].(string)
	if header == "" || header == "v1=" {
		t.Fatalf("header = %q outputs=%v", header, res.Outputs)
	}
	if _, err := m.Describe("hmac"); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleScriptsDoNotRun(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed")
	}
	dir := t.TempDir()
	m, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	src := filepath.Join("..", "..", "testdata", "plugins", "lifecycle")
	if _, err := m.Install(src, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "packages", "lifecycle", "pwned")); !os.IsNotExist(err) {
		t.Fatal("preinstall ran")
	}
}

func TestMissingLock(t *testing.T) {
	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"name":"x","version":"1.0.0","crabbro":{"id":"x","api":"1.0.0","entry":"index.js"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.js"), []byte("export default {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(src, false); err == nil {
		t.Fatal("expected missing lockfile to fail")
	}
}
