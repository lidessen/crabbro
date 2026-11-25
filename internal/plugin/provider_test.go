package plugin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/provider"
)

func TestProviderRegistrationAndAuthRPC(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	for _, isolation := range []string{"worker", "process"} {
		t.Run(isolation, func(t *testing.T) {
			dir := t.TempDir()
			code := `export default { providers: { partner: { auth: { "plugin-token": {
   prepare: async ({credentialRef}) => ({credentialRef}),
   refresh: async ({credentialRef}) => ({credentialRef, retry: true})
  } } } } }`
			if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte(code), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := NewManager(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			spec := provider.ProviderSpec{ID: "partner", DisplayName: "Partner", SupportedAuth: []provider.AuthMethodSpec{{Kind: "plugin-token"}}}
			m.state.Packages = []PackageRecord{{ID: "p", Path: dir, Entry: "index.mjs", Enabled: true, Isolation: isolation}}
			m.state.Instances = []InstanceRecord{{Plugin: "p", Name: "local", Enabled: true, Credentials: map[string]string{"token": "assigned"}}}
			m.state.Providers = map[string][]provider.ProviderSpec{"p": {spec}}
			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewManager(m.Dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reloaded.Close()
			registry := provider.NewRegistry()
			if err := reloaded.RegisterPluginProviders(registry, "p"); err != nil {
				t.Fatal(err)
			}
			if _, ok := registry.Lookup("partner"); !ok {
				t.Fatal("persisted provider missing")
			}
			copy := reloaded.ProviderDeclarations("p")
			copy[0].SupportedAuth[0].Kind = "changed"
			req := provider.AuthRequest{PluginID: "p", Instance: "local", ProviderID: "partner", Kind: "plugin-token", CredentialRef: "assigned"}
			req.CredentialRef = "unrelated"
			if _, err := reloaded.CallProviderAuth(context.Background(), "provider.auth.prepare", req); err == nil {
				t.Fatal("unrelated credential accepted")
			}
			req.CredentialRef = "assigned"
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "probe.sock"))
			if errors.Is(err, syscall.EPERM) {
				t.Skip("sandbox denies Unix socket listeners")
			}
			if err != nil {
				t.Fatal(err)
			}
			listener.Close()
			bridge := provider.PluginAuthenticator{RPC: reloaded, Request: req}
			result, err := bridge.Prepare(context.Background())
			if err != nil || result.CredentialRef != "assigned" {
				t.Fatalf("prepare: %+v, %v", result, err)
			}
			result, err = bridge.Refresh(context.Background())
			if err != nil || !result.Retry {
				t.Fatalf("refresh: %+v, %v", result, err)
			}
			req.CredentialRef = "unrelated"
			if _, err := reloaded.CallProviderAuth(context.Background(), "provider.auth.prepare", req); err == nil {
				t.Fatal("unrelated credential accepted")
			}
		})
	}
}

func TestWriteFrameSizeLimit(t *testing.T) {
	m := &Manager{}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if err := m.writeTo(left, map[string]any{"data": strings.Repeat("x", maxFrameBytes)}); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

// Exercise the process-isolated RPC handler over its actual framed stdio without
// relying on a Unix socket, which may be unavailable in restricted environments.
func TestProviderAuthProcessFrames(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	if err := extractRuntime(dir); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "entry.mjs")
	if err := os.WriteFile(entry, []byte(`export default {providers:{partner:{auth:{"plugin-token":{prepare: async ({credentialRef}) => ({credentialRef}), refresh: async ({credentialRef}) => ({credentialRef,retry:true})}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(dir, "process.mjs"))
	cmd.Env = append(os.Environ(), "CRABBRO_PLUGIN_ENTRY="+entry)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); _ = cmd.Wait() }()
	read := func() map[string]any {
		var n uint32
		if err := binary.Read(output, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		if n == 0 || n > maxFrameBytes {
			t.Fatalf("invalid frame size %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(output, body); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if read()["type"] != "ready" {
		t.Fatal("process not ready")
	}
	for _, method := range []string{"provider.auth.prepare", "provider.auth.refresh"} {
		data, err := json.Marshal(map[string]any{"type": method, "id": "one", "providerId": "partner", "kind": "plugin-token", "credentialRef": "assigned"})
		if err != nil {
			t.Fatal(err)
		}
		if err := binary.Write(input, binary.BigEndian, uint32(len(data))); err != nil {
			t.Fatal(err)
		}
		if _, err := input.Write(data); err != nil {
			t.Fatal(err)
		}
		response := read()
		result, _ := response["result"].(map[string]any)
		if result["credentialRef"] != "assigned" || (method == "provider.auth.refresh" && result["retry"] != true) {
			t.Fatalf("invalid %s response: %v", method, response)
		}
	}
}
