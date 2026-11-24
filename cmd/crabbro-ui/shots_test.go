//go:build shots

package main

import (
	"context"
	"fmt"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/client"
)

// TestShots draws every page against a real crabbrod with seeded traffic and
// writes PNGs, light and dark, for a design review:
//
//	CRABBROD=/path/to/crabbrod CRABBRO=/path/to/crabbro SHOTS=/out/dir \
//	  go test -tags shots -run TestShots ./cmd/crabbro-ui
func TestShots(t *testing.T) {
	out, daemon, cli := os.Getenv("SHOTS"), os.Getenv("CRABBROD"), os.Getenv("CRABBRO")
	if out == "" || daemon == "" || cli == "" {
		t.Skip("set SHOTS, CRABBROD and CRABBRO")
	}
	dir, err := os.MkdirTemp("", "chs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "proxy.sock")
	proxyAddr, tcpAddr := freeAddr(t), freeAddr(t)

	cmd := exec.Command(daemon, "-uds-path", sock, "-tcp-addr", tcpAddr, "-proxy-addr", proxyAddr,
		"-history", filepath.Join(dir, "history.db"), "-fixture-dir", filepath.Join(dir, "fx"))
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/login"):
			time.Sleep(120 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"eyJhbGciOi.demo","user":{"id":42,"name":"Mika","roles":["admin","dev"]},"expiresIn":3600}`)
		case strings.HasPrefix(r.URL.Path, "/v1/orders"):
			time.Sleep(310 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[{"id":1,"sku":"crab-01","qty":2},{"id":2,"sku":"shell-09","qty":1}],"total":3}`)
		case strings.HasPrefix(r.URL.Path, "/v1/missing"):
			http.Error(w, `{"error":"not found"}`, 404)
		case strings.HasPrefix(r.URL.Path, "/v1/boom"):
			time.Sleep(60 * time.Millisecond)
			http.Error(w, `{"error":"upstream exploded"}`, 500)
		case strings.HasPrefix(r.URL.Path, "/old"):
			http.Redirect(w, r, "/", 302)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><html><head><title>Shop</title></head><body><h1>Hello</h1><p>crab shop</p></body></html>")
		}
	}))
	defer up.Close()

	ctx := context.Background()
	c := client.New(client.Options{UDS: sock, TCP: tcpAddr})
	for i := 0; ; i++ {
		if _, err := c.Status(ctx); err == nil {
			break
		}
		if i > 100 {
			t.Fatal("daemon did not answer")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pu, _ := url.Parse("http://" + proxyAddr)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(method, path, body string) {
		req, _ := http.NewRequest(method, up.URL+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("User-Agent", "shop-ios/3.2")
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	run := func(args ...string) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		t.Logf("crabbro %v", args)
		cc := exec.CommandContext(cctx, cli, append([]string{"--uds-path", sock, "--tcp-addr", tcpAddr}, args...)...)
		cc.Env = append(os.Environ(), "HOME="+dir)
		if b, err := cc.CombinedOutput(); err != nil {
			t.Logf("crabbro %v: %v\n%s", args, err, b)
		}
	}

	if os.Getenv("SHOTS_EMPTY") == "" {
		send("GET", "/", "")
		send("GET", "/assets/app.css", "")
		if _, err := c.StartRecording(ctx, "login-hunt"); err != nil {
			t.Fatal(err)
		}
		send("POST", "/v1/login", `{"user":"mika","password":"hunter2"}`)
		send("GET", "/v1/orders?limit=20&cursor=abc", "")
		send("GET", "/v1/missing", "")
		send("DELETE", "/v1/orders/2", "")
		send("PUT", "/v1/orders/1", `{"qty":3}`)
		send("GET", "/v1/boom", "")
		send("GET", "/old", "")
		run("rules", "from", "req_3", "--action", "mock")
		run("rules", "from", "req_4", "--action", "delay")
		send("POST", "/v1/login", `{"user":"mika","password":"hunter2"}`)
		send("GET", "/v1/orders?limit=20", "")
		run("verify", "replay", "req_3")
	}

	a := newApp(c, ctx)
	go a.run(ctx)
	tt := ui.NewTester(a.view, 1280, 820)
	tt.SetScale(2)
	for i := 0; i < 100; i++ {
		tt.Frame()
		if a.snapshotState().conn == connConnected {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	settle := func() {
		for i := 0; i < 8; i++ {
			time.Sleep(120 * time.Millisecond)
			tt.Frame()
			tt.Advance(400 * time.Millisecond)
		}
	}
	shot := func(name string) {
		settle()
		f, err := os.Create(filepath.Join(out, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	for _, dark := range []bool{false, true} {
		tt.SetDark(dark)
		sfx := map[bool]string{false: "light", true: "dark"}[dark]
		a.setPage(pageTrajectory)
		shot("trace-" + sfx)
		a.setPage(pageRequests)
		a.reqSel = 2
		for tab := 0; tab < 3; tab++ {
			a.detailTab = tab
			shot(fmt.Sprintf("requests-%d-%s", tab, sfx))
		}
		a.setPage(pageRules)
		a.ruleEditMode = 0
		shot("rules-overview-" + sfx)
		a.ruleEditMode = 1
		shot("rules-code-" + sfx)
		a.setPage(pageDaemon)
		shot("daemon-" + sfx)
	}
	tt.SetSize(860, 540)
	tt.SetDark(false)
	a.setPage(pageRequests)
	a.detailTab = 0
	shot("requests-min")
	a.setPage(pageTrajectory)
	shot("trace-min")
	a.setPage(pageRules)
	shot("rules-min")
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}
