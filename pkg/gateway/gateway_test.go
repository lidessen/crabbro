package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCatalogAndStreaming(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"model-a\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	dir := t.TempDir()
	for _, sub := range []string{"plans", "routes", "secrets"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	plan := fmt.Sprintf("version: 1\nid: test-plan\ncredential: test-key\nendpoints:\n  anthropic:\n    baseUrl: %s\nmodels:\n  model-a:\n    tier: strong\n    contextWindow: 200000\n    capabilities:\n      tools: true\n", upstream.URL)
	route := "version: 1\nid: test-route\ndescription: Example route\npriority: 10\nfamily: generative\nmatch:\n  protocols: [anthropic]\n  models: [group/test]\ntargets:\n  - plan: test-plan\n    model: model-a\n    priority: 10\n"
	for path, content := range map[string]string{"plans/test-plan.yaml": plan, "routes/test-route.yaml": route, "secrets/test-key": "fixture"} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Open(Options{RootDir: dir, SecretsDir: filepath.Join(dir, "secrets")})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	c := e.Catalog()
	if len(c.Plans) != 1 || !c.Plans[0].HasSecret || len(c.Plans[0].Models) != 1 || c.Plans[0].Models[0].ContextWindow != 200000 || !c.Plans[0].Models[0].Capabilities.Tools || len(c.Routes) != 1 || c.Routes[0].Description != "Example route" {
		t.Fatalf("unexpected catalog: %+v", c)
	}
	req, _ := http.NewRequest("POST", "http://crabbro.local/v1/messages", strings.NewReader(`{"model":"group/test","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %s: %s", resp.Status, b)
	}
	got := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(resp.Body).ReadString('\n'); got <- line }()
	select {
	case line := <-got:
		if !strings.Contains(line, "message_start") {
			t.Fatalf("first line: %q", line)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first event was buffered")
	}
	close(release)
	_, _ = io.Copy(io.Discard, resp.Body)

	routeConfig := e.gateway.Config.Snapshot().AllRoutes[0]
	routeConfig.Match.Profiles = []string{"profile"}
	routeConfig.Match.Protocols = []string{"anthropic"}
	routeConfig.Match.Models = []string{"group/test"}
	routeConfig.Targets[0].WhenTier = []string{"strong"}
	first := e.Catalog()
	first.Routes[0].Profiles[0] = "changed"
	first.Routes[0].Protocols[0] = "changed"
	first.Routes[0].Models[0] = "changed"
	first.Routes[0].Targets[0].WhenTier[0] = "changed"
	second := e.Catalog().Routes[0]
	if second.Profiles[0] != "profile" || second.Protocols[0] != "anthropic" || second.Models[0] != "group/test" || second.Targets[0].WhenTier[0] != "strong" {
		t.Fatalf("catalog mutation changed configuration: %+v", second)
	}
}

func TestResponseBodyCloseCancelsIdleUpstream(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	tr := transport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		w.(http.Flusher).Flush()
		_, _ = io.Copy(w, resp.Body)
	})}
	req, err := http.NewRequest(http.MethodGet, "http://gateway.local/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("closing response did not cancel idle upstream")
	}
}

func TestNoForbiddenDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "github.com/egoist/mygo") || strings.Contains(dep, "goja") || dep == "github.com/lidessen/crabbro/internal/proxy" || dep == "github.com/lidessen/crabbro/internal/mitm" || dep == "github.com/lidessen/crabbro/internal/rule" {
			t.Fatalf("forbidden dependency: %s", dep)
		}
	}
}
