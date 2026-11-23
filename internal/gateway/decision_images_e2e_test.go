package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

func TestDecisionImageGatewayLoopback(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	imageBody := fmt.Sprintf(`{"model":"decision","state":{"text":"hello","image":"window"},"assets":{"window":{"type":"image","format":"png","data_base64":%q}},"questions":{"ok":{"type":"noul","instructions":"ok?"}}}`, encoded)
	textBody := `{"model":"decision","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`
	for _, tc := range []struct {
		name, protocol, response, expected string
		supported                          bool
	}{
		{"openai", "openai-decisions", `{"model":"target","answers":[{"type":"predicate","name":"ok","probability":0.9}]}`, "data:image/png;base64," + encoded, true},
		{"google", "gemini-decisions", `{"answers":{"ok":{"type":"noul","noul":0.9}}}`, encoded, true},
		{"unsupported", "anthropic", `{"content":[{"type":"text","text":"{}"}]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			var received atomic.Value
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received.Store(string(raw))
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Crabbro-Decision-Images", "preserved")
				fmt.Fprint(w, tc.response)
			}))
			defer upstream.Close()
			dir := t.TempDir()
			sec := secret.New(filepath.Join(dir, "secrets"))
			if err := sec.Set("test", "test-value"); err != nil {
				t.Fatal(err)
			}
			gw, err := New(Options{ListenAddr: "127.0.0.1:0", RootDir: filepath.Join(dir, "gateway")}, sec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer gw.Stop()
			plan := fmt.Sprintf("version: 1\nid: p\ncredential: test\nendpoints:\n  %s:\n    baseUrl: %s\nmodels:\n  target:\n    tier: fast\n", tc.protocol, upstream.URL)
			route := fmt.Sprintf("version: 1\nid: decision\nenabled: true\nfamily: decision\nmatch:\n  protocols: [typesafe-jev]\npolicy:\n  preset: strict-failover\n  allowCrossProtocol: true\n  allowEmulation: true\ntargets:\n  - plan: p\n    model: target\n    endpointProtocol: %s\n", tc.protocol)
			for _, c := range []struct{ k, id, body string }{{config.KindPlan, "p", plan}, {config.KindRoute, "decision", route}} {
				if err := gw.Config.Draft(c.k, c.id, []byte(c.body)); err != nil {
					t.Fatal(err)
				}
				if err := gw.Config.Commit(c.k, c.id); err != nil {
					t.Fatal(err)
				}
			}
			if err := gw.Start(); err != nil {
				t.Fatal(err)
			}
			post := func(body string) *http.Response {
				t.Helper()
				r, err := http.Post("http://"+gw.Addr()+"/v1/systemone", "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				return r
			}
			r := post(imageBody)
			if !tc.supported {
				if r.StatusCode < 400 || hits.Load() != 0 {
					t.Fatalf("unsupported: status=%d hits=%d", r.StatusCode, hits.Load())
				}
				return
			}
			if r.StatusCode != http.StatusOK || r.Header.Get("X-Crabbro-Decision-Images") != "preserved" || hits.Load() != 1 {
				t.Fatalf("image: status=%d headers=%v hits=%d", r.StatusCode, r.Header, hits.Load())
			}
			var request any
			if err := json.Unmarshal([]byte(received.Load().(string)), &request); err != nil {
				t.Fatal(err)
			}
			if !containsImageValue(request, tc.expected) {
				t.Fatalf("upstream did not receive exact image: %s", received.Load())
			}
			r = post(textBody)
			if r.StatusCode != http.StatusOK || r.Header.Get("X-Crabbro-Decision-Images") != "" || hits.Load() != 2 {
				t.Fatalf("spoofed marker: status=%d headers=%v hits=%d", r.StatusCode, r.Header, hits.Load())
			}
		})
	}
}

func containsImageValue(v any, expected string) bool {
	switch x := v.(type) {
	case string:
		return x == expected
	case []any:
		for _, item := range x {
			if containsImageValue(item, expected) {
				return true
			}
		}
	case map[string]any:
		for _, item := range x {
			if containsImageValue(item, expected) {
				return true
			}
		}
	}
	return false
}
