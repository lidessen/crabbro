package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/chiling/internal/plugin"
	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/rule"
)

func TestPluginUsesSignsThroughProxy(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed")
	}
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Signature")
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	mgr, err := plugin.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "plugins", "hmac"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Install(src, true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PutSecret("partner", "topsecret"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddInstance("hmac", "primary", nil, map[string]string{"partner_hmac": "partner"}); err != nil {
		t.Fatal(err)
	}
	eng := rule.NewRuleEngine()
	eng.Runtime = mgr
	script := `
version: 1
id: sign-upstream
on:
  request:
    method: POST
    path: /v1/pay
request:
  pipeline:
    - id: signed
      uses: hmac.sign
      instance: primary
      with:
        algorithm: sha256
    - uses: chiling.set-request-header
      if: pipeline.signed.outputs.ok == true
      with:
        name: X-Signature
        value: "${{ pipeline.signed.outputs.header }}"
`
	rs := rule.NewRuleStore()
	if _, err := rs.Create("sign", 10, script); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(4), rs, eng)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Post(upstream.URL+"/v1/pay", "text/plain", strings.NewReader("pay"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if !strings.HasPrefix(got, "v1=") || len(got) < 8 {
		t.Fatalf("signature = %q", got)
	}
}
