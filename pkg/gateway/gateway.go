// Package gateway embeds Crabbro's LLM gateway without starting a listener.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	core "github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/pkg/api"
)

type Options struct {
	RootDir        string
	SecretsDir     string
	CaptureMode    string
	MaxRequestBody int64
	UsageRetention time.Duration
}

type Engine struct{ gateway *core.Gateway }

// Open loads configuration and opens the usage ledger without binding a socket.
func Open(opt Options) (*Engine, error) {
	if opt.RootDir == "" || opt.SecretsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if opt.RootDir == "" {
			opt.RootDir = filepath.Join(home, ".crabbro", "gateway")
		}
		if opt.SecretsDir == "" {
			opt.SecretsDir = filepath.Join(home, ".crabbro", "secrets")
		}
	}
	if opt.CaptureMode == "" {
		opt.CaptureMode = "off"
	}
	g, err := core.New(core.Options{RootDir: opt.RootDir, CaptureMode: opt.CaptureMode, MaxRequestBody: opt.MaxRequestBody, UsageRetention: opt.UsageRetention}, secret.New(opt.SecretsDir), nil)
	if err != nil {
		return nil, err
	}
	e := &Engine{gateway: g}
	if err := e.Reload(); err != nil {
		_ = g.Stop()
		return nil, err
	}
	return e, nil
}

func (e *Engine) Handler() http.Handler           { return e.gateway }
func (e *Engine) RoundTripper() http.RoundTripper { return transport{e.Handler()} }
func (e *Engine) HTTPClient() *http.Client        { return &http.Client{Transport: e.RoundTripper()} }
func (e *Engine) Close() error                    { return e.gateway.Stop() }

// Reload preserves the last valid configuration when a file is invalid.
// Secrets are read from disk on demand and do not require a separate reload.
func (e *Engine) Reload() error {
	ds, complete := e.gateway.Reload()
	if !complete || len(ds) != 0 {
		return fmt.Errorf("gateway configuration reload failed: %v", ds)
	}
	return nil
}

func (e *Engine) RefreshQuotas(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	plans := e.gateway.Config.Snapshot().Plans
	e.gateway.Quotas.Refresh(ctx, plans)
	if err := ctx.Err(); err != nil {
		return err
	}
	var errs []error
	for id := range plans {
		if q, ok := e.gateway.Quotas.Get(id); ok && q.LastError != "" {
			errs = append(errs, fmt.Errorf("plan %s: %s", id, q.LastError))
		}
	}
	return errors.Join(errs...)
}

type Capabilities struct{ Tools, Vision, Thinking, StructuredOutput, PromptCache bool }
type Model struct {
	ID            string
	Tier          string
	ContextWindow int
	Capabilities  Capabilities
}
type Plan struct {
	ID                 string
	Healthy, HasSecret bool
	Models             []Model
	Windows            []api.GatewayWindowUsage
}
type Catalog struct {
	Routes []api.GatewayRouteStatus
	Plans  []Plan
}

// Catalog returns enabled routes and plans, detached from the configuration snapshot.
func (e *Engine) Catalog() Catalog {
	snap := e.gateway.Config.Snapshot()
	var c Catalog
	for _, r := range e.gateway.Routes().Routes {
		if r.Enabled {
			r.Profiles = slices.Clone(r.Profiles)
			r.Protocols = slices.Clone(r.Protocols)
			r.Models = slices.Clone(r.Models)
			for i := range r.Targets {
				r.Targets[i].WhenTier = slices.Clone(r.Targets[i].WhenTier)
			}
			c.Routes = append(c.Routes, r)
		}
	}
	for _, s := range e.gateway.Plans().Plans {
		p := snap.Plans[s.ID]
		if p == nil {
			continue
		}
		item := Plan{ID: s.ID, Healthy: s.Healthy, HasSecret: s.HasSecret, Windows: s.Windows}
		for id, m := range p.Models {
			if m == nil {
				continue
			}
			x := m.Capabilities
			item.Models = append(item.Models, Model{ID: id, Tier: m.EffectiveTier(), ContextWindow: m.ContextWindow, Capabilities: Capabilities{x.Tools, x.Vision, x.Thinking, x.StructuredOutput, x.PromptCache}})
		}
		sort.Slice(item.Models, func(i, j int) bool { return item.Models[i].ID < item.Models[j].ID })
		c.Plans = append(c.Plans, item)
	}
	return c
}

type transport struct{ handler http.Handler }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	handlerReq := req.WithContext(ctx)
	r, w := io.Pipe()
	pw := &pipeWriter{header: make(http.Header), body: w, ready: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer func() {
			defer close(pw.finished)
			if recover() != nil {
				pw.finish()
				_ = w.CloseWithError(errors.New("gateway handler panicked"))
			} else {
				pw.finish()
				_ = w.Close()
			}
			if req.Body != nil {
				_ = req.Body.Close()
			}
		}()
		t.handler.ServeHTTP(pw, handlerReq)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = r.CloseWithError(ctx.Err())
		case <-pw.finished:
			cancel()
		}
	}()
	select {
	case <-pw.ready:
		return &http.Response{StatusCode: pw.status, Status: fmt.Sprintf("%d %s", pw.status, http.StatusText(pw.status)), Header: pw.resultHeader, Body: &responseBody{PipeReader: r, cancel: cancel}, Request: req, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, ContentLength: -1}, nil
	case <-req.Context().Done():
		cancel()
		_ = r.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
}

type responseBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *responseBody) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}

type pipeWriter struct {
	header       http.Header
	resultHeader http.Header
	body         *io.PipeWriter
	ready        chan struct{}
	finished     chan struct{}
	once         sync.Once
	status       int
}

func (w *pipeWriter) Header() http.Header { return w.header }
func (w *pipeWriter) WriteHeader(status int) {
	w.once.Do(func() { w.status = status; w.resultHeader = w.header.Clone(); close(w.ready) })
}
func (w *pipeWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}
func (w *pipeWriter) Flush()  { w.WriteHeader(http.StatusOK) }
func (w *pipeWriter) finish() { w.WriteHeader(http.StatusOK) }
