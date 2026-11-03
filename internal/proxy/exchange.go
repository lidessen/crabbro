package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/rule"
	"github.com/lidessen/chiling/internal/shared"
)

// Exchange is one logical HTTP request/response. CONNECT passthrough does not
// create an Exchange. A decrypted request inside a tunnel does, and points at
// the parent tunnel.
type Exchange struct {
	ctx       context.Context
	record    *shared.Request
	outbound  *http.Request
	started   time.Time
	traces    []shared.RuleTrace
	tags      []string
	policyGen uint64
	ruleGen   uint64

	preview          []byte
	previewTruncated bool

	reqCapture  *captureReader
	respCapture *captureReader

	decision     string
	failureStage string

	matched       []*shared.Rule
	upgradeConn   net.Conn
	clientPreface []byte
}

// ExchangeHandler runs one stage of the onion and returns the response outer
// layers may still observe. It does not write to the client.
type ExchangeHandler func(*Exchange) (*http.Response, error)

// Middleware wraps the downstream handler. Request work runs before next;
// response work runs after next returns.
type Middleware interface {
	ID() string
	Priority() int
	Wrap(next ExchangeHandler) ExchangeHandler
}

// errReject is a typed rejection. The ingress boundary turns it into a
// defined HTTP response. It replaces the old silent abort.
type errReject struct {
	status int
	body   string
}

func (e *errReject) Error() string { return "rejected by rule" }

func compose(mws []Middleware, terminal ExchangeHandler) ExchangeHandler {
	h := terminal
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i].Wrap(h)
	}
	return h
}

// serveClientExchange runs one parsed HTTP request through the pipeline and
// writes the resulting response. ingress is "http" or "mitm".
func (p *Proxy) serveClientExchange(w http.ResponseWriter, r *http.Request, ingress string, tunnelID shared.RequestID, tunnelHost string) {
	defer func() {
		if rec := recover(); rec != nil {
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}
	}()

	if ingress == "http" {
		if r.URL == nil || !r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https") || r.URL.Host == "" || r.URL.User != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}

	ex, err := p.newExchange(r, ingress, tunnelID, tunnelHost)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	handler := compose(p.ruleMiddleware(ex), p.upstreamTerminal)
	resp, runErr := handler(ex)
	if runErr != nil {
		var rej *errReject
		if errors.As(runErr, &rej) {
			ex.decision = "reject"
			ex.failureStage = "middleware"
			p.finishExchange(ex, rej.status, textResponse(rej.status, rej.body), w, "request_failed")
			return
		}
		if ex.failureStage == "" {
			ex.failureStage = classifyStage(runErr)
		}
		status := http.StatusBadGateway
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ex.ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		p.finishExchange(ex, status, textResponse(status, http.StatusText(status)), w, "request_failed")
		return
	}
	if resp == nil {
		ex.failureStage = "middleware"
		p.finishExchange(ex, http.StatusBadGateway, textResponse(http.StatusBadGateway, "Bad Gateway"), w, "request_failed")
		return
	}
	if ex.upgradeConn != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		p.bridgeWebSocket(w, ex, resp)
		return
	}
	kind := "response_completed"
	if ex.decision == "reject" {
		kind = "request_failed"
	}
	p.finishExchange(ex, resp.StatusCode, resp, w, kind)
}

func (p *Proxy) newExchange(r *http.Request, ingress string, tunnelID shared.RequestID, tunnelHost string) (*Exchange, error) {
	start := time.Now()
	target, err := requestURL(r, ingress, tunnelHost)
	if err != nil {
		return nil, err
	}

	policy := p.policyForTunnel()
	var policyGen uint64
	if policy != nil {
		policyGen = policy.Generation
	}

	reqHeaders, flatReq := headerFields(r.Header)
	record := &shared.Request{
		StartedAt:         start,
		Method:            r.Method,
		Scheme:            target.Scheme,
		Host:              target.Host,
		Path:              target.Path,
		Query:             target.RawQuery,
		URL:               target.String(),
		OriginalURL:       target.String(),
		EffectiveURL:      target.String(),
		ClientAddr:        r.RemoteAddr,
		RequestHeaders:    flatReq,
		RequestHeaderList: reqHeaders,
		Intercepted:       ingress == "mitm",
		TunnelID:          tunnelID,
		PolicyGeneration:  policyGen,
	}
	if p.RingBuffer != nil {
		stored := p.RingBuffer.Add(record.Method, record.Scheme, record.Host, record.Path, record.Query, record.URL, record.ClientAddr)
		stored.StartedAt = start
		stored.OriginalURL = record.OriginalURL
		stored.EffectiveURL = record.EffectiveURL
		stored.RequestHeaders = record.RequestHeaders
		stored.RequestHeaderList = record.RequestHeaderList
		stored.Intercepted = record.Intercepted
		stored.TunnelID = record.TunnelID
		stored.PolicyGeneration = record.PolicyGeneration
		record = stored
	}
	if p.Sessions != nil {
		p.Sessions.NoteRequest(record)
	}

	p.emit(shared.EventRequestStarted, record.ID, nil)
	p.emit(shared.EventRequestHeaders, record.ID, nil)

	ctx := r.Context()
	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), nil)
	if err != nil {
		return nil, err
	}
	out.Header = r.Header.Clone()
	stripHopByHop(out.Header)
	if isWebSocketUpgrade(r) {
		restoreWebSocketHeaders(out.Header, r.Header)
	}
	out.Host = target.Host
	out.ContentLength = r.ContentLength
	if r.Body != nil {
		out.Body = r.Body
	}

	ex := &Exchange{
		ctx:       ctx,
		record:    record,
		outbound:  out,
		started:   start,
		policyGen: policyGen,
	}
	p.prepareRequestBody(ex, r)
	return ex, nil
}

func requestURL(r *http.Request, ingress, tunnelHost string) (*url.URL, error) {
	if ingress == "http" {
		u := *r.URL
		return &u, nil
	}
	host := tunnelHost
	if host == "" {
		host = r.Host
	}
	path := "/"
	if r.URL != nil {
		path = r.URL.RequestURI()
		if path == "" {
			path = r.URL.Path
		}
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return url.Parse("https://" + host + path)
}

// prepareRequestBody gives rules a bounded preview without dropping the
// forwarded body. With no enabled rules the body is not read ahead; capture
// tees bytes as the transport forwards them.
func (p *Proxy) prepareRequestBody(ex *Exchange, r *http.Request) {
	if r.Body == nil || r.ContentLength == 0 {
		ex.outbound.Body = nil
		ex.outbound.ContentLength = 0
		return
	}
	limit := shared.MaxBodySize
	if p.enabledRuleCount() == 0 {
		ex.reqCapture = &captureReader{r: r.Body, limit: limit}
		ex.outbound.Body = io.NopCloser(ex.reqCapture)
		ex.outbound.ContentLength = r.ContentLength
		return
	}
	preview, extra, err := readPreview(r.Body, limit)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		ex.failureStage = "client_body"
	}
	ex.preview = preview
	ex.previewTruncated = len(extra) > 0
	var rest io.Reader = r.Body
	if len(extra) > 0 {
		rest = io.MultiReader(bytes.NewReader(extra), r.Body)
	}
	stream := io.MultiReader(bytes.NewReader(preview), rest)
	ex.reqCapture = &captureReader{r: stream, limit: limit}
	ex.outbound.Body = io.NopCloser(ex.reqCapture)
	ex.outbound.ContentLength = r.ContentLength
	if len(preview) > 0 {
		ex.record.RequestBodyRef = p.BodyCache.Store(append([]byte(nil), preview...))
		ex.record.RequestBodyBytes = int64(len(preview))
		ex.record.RequestBodyTruncated = ex.previewTruncated
		p.emit(shared.EventRequestBodyAvailable, ex.record.ID, nil)
	}
}

func (p *Proxy) enabledRuleCount() int {
	if p.RuleStore == nil {
		return 0
	}
	_, rules := p.RuleStore.Snapshot()
	return len(rules)
}

func (p *Proxy) ruleMiddleware(ex *Exchange) []Middleware {
	if p.RuleStore == nil || p.RuleEngine == nil {
		return nil
	}
	gen, rules := p.RuleStore.Snapshot()
	ex.ruleGen = gen
	ex.record.RuleGeneration = gen
	mws := make([]Middleware, 0, len(rules))
	for _, rl := range rules {
		mws = append(mws, &ruleMiddleware{rule: rl, proxy: p})
	}
	return mws
}

type ruleMiddleware struct {
	rule  *shared.Rule
	proxy *Proxy
}

func (m *ruleMiddleware) ID() string    { return string(m.rule.ID) }
func (m *ruleMiddleware) Priority() int { return m.rule.Priority }

func (m *ruleMiddleware) Wrap(next ExchangeHandler) ExchangeHandler {
	return func(ex *Exchange) (*http.Response, error) {
		if time.Since(ex.started) > shared.DefaultRuleBudget {
			ex.traces = append(ex.traces, shared.RuleTrace{
				RuleID:      m.rule.ID,
				RuleVersion: m.rule.Version,
				Error:       "rule evaluation budget exceeded",
			})
			if m.rule.FailureMode == "fail-closed" {
				ex.decision = "reject"
				return nil, &errReject{status: http.StatusForbidden, body: "rule evaluation budget exceeded"}
			}
			return next(ex)
		}

		view := ex.viewRequest()
		result := m.proxy.RuleEngine.Evaluate(m.rule.Script, view, string(ex.preview), shared.DefaultRuleTimeout)
		trace := shared.RuleTrace{
			RuleID:      m.rule.ID,
			RuleVersion: m.rule.Version,
			Matched:     result.Matched && result.SchemaValid && result.Error == "",
			DurationMs:  result.DurationMs,
			Error:       result.Error,
		}
		if result.Error != "" || !result.SchemaValid {
			trace.Matched = false
			ex.traces = append(ex.traces, trace)
			m.proxy.emit(shared.EventRuleEvaluated, ex.record.ID, map[string]any{
				"ruleId": m.rule.ID, "matched": false, "error": result.Error,
			})
			if m.rule.FailureMode == "fail-closed" {
				ex.decision = "reject"
				return nil, &errReject{status: http.StatusForbidden, body: "rule failed closed"}
			}
			return next(ex)
		}
		if !result.Matched {
			ex.traces = append(ex.traces, trace)
			m.proxy.emit(shared.EventRuleEvaluated, ex.record.ID, map[string]any{
				"ruleId": m.rule.ID, "matched": false,
			})
			return next(ex)
		}

		trace.Phase = "request"
		trace.Actions = result.Actions
		ex.traces = append(ex.traces, trace)
		ex.matched = append(ex.matched, m.rule)
		m.proxy.emit(shared.EventRuleEvaluated, ex.record.ID, map[string]any{
			"ruleId": m.rule.ID, "matched": true,
		})
		m.proxy.emit(shared.EventRuleApplied, ex.record.ID, map[string]any{
			"ruleId": m.rule.ID, "actions": result.Actions,
		})

		reqActions, respActions := splitStage(result.Actions)
		synthetic, err := applyRequestPatch(ex, reqActions)
		if err != nil {
			return nil, err
		}
		if synthetic != nil {
			ex.decision = "respond"
			if err := applyResponseActions(synthetic, respActions); err != nil {
				return nil, err
			}
			return synthetic, nil
		}
		resp, err := next(ex)
		if err != nil || resp == nil {
			return resp, err
		}
		if len(respActions) > 0 {
			if err := applyResponseActions(resp, respActions); err != nil {
				if m.rule.FailureMode == "fail-closed" {
					ex.decision = "reject"
					return nil, &errReject{status: http.StatusForbidden, body: "response rule failed closed"}
				}
				ex.traces = append(ex.traces, shared.RuleTrace{
					RuleID: m.rule.ID, RuleVersion: m.rule.Version, Phase: "response", Error: err.Error(),
				})
				return resp, nil
			}
			return resp, nil
		}
		return m.applyResponsePhase(ex, resp)
	}
}

func (m *ruleMiddleware) applyResponsePhase(ex *Exchange, resp *http.Response) (*http.Response, error) {
	preview, truncated := previewResponse(resp)
	_, flat := headerFields(resp.Header)
	phase := &rule.Phase{
		HasResponse:           true,
		ResponseStatus:        resp.StatusCode,
		ResponseHeaders:       flat,
		ResponseBody:          string(preview),
		ResponseBodyTruncated: truncated,
	}
	result := m.proxy.RuleEngine.EvaluatePhase(m.rule.Script, ex.viewRequest(), string(ex.preview), phase, shared.DefaultRuleTimeout)
	trace := shared.RuleTrace{
		RuleID: m.rule.ID, RuleVersion: m.rule.Version, Phase: "response",
		DurationMs: result.DurationMs, Error: result.Error,
		Matched: result.Matched && result.SchemaValid && result.Error == "",
	}
	if result.Error != "" || !result.SchemaValid || !result.Matched {
		trace.Matched = false
		ex.traces = append(ex.traces, trace)
		if result.Error != "" && m.rule.FailureMode == "fail-closed" {
			ex.decision = "reject"
			return nil, &errReject{status: http.StatusForbidden, body: "response rule failed closed"}
		}
		return resp, nil
	}
	_, respActions := splitStage(result.Actions)
	trace.Actions = respActions
	ex.traces = append(ex.traces, trace)
	if len(respActions) == 0 {
		return resp, nil
	}
	if err := applyResponseActions(resp, respActions); err != nil {
		trace.Error = err.Error()
		trace.Matched = false
		if m.rule.FailureMode == "fail-closed" {
			ex.decision = "reject"
			return nil, &errReject{status: http.StatusForbidden, body: "response rule failed closed"}
		}
		return resp, nil
	}
	m.proxy.emit(shared.EventRuleApplied, ex.record.ID, map[string]any{
		"ruleId": m.rule.ID, "phase": "response", "actions": respActions,
	})
	return resp, nil
}

func applyRequestPatch(ex *Exchange, actions []shared.RuleAction) (*http.Response, error) {
	var synthetic *http.Response
	for _, action := range actions {
		switch action.Type {
		case shared.ActionTag:
			ex.tags = append(ex.tags, stringListParam(action.Params["tags"])...)
		case shared.ActionSetRequestHeader:
			name, _ := action.Params["name"].(string)
			value, _ := action.Params["value"].(string)
			ex.outbound.Header.Set(name, value)
		case shared.ActionRemoveRequestHeader:
			name, _ := action.Params["name"].(string)
			ex.outbound.Header.Del(name)
		case shared.ActionRewriteURL:
			raw, _ := action.Params["url"].(string)
			u, err := url.Parse(raw)
			if err != nil {
				return nil, err
			}
			ex.outbound.URL = u
			ex.outbound.Host = u.Host
			ex.record.EffectiveURL = u.String()
		case shared.ActionBlock:
			status := intParam(action.Params["status"], http.StatusForbidden)
			body, _ := action.Params["body"].(string)
			ex.decision = "respond"
			synthetic = textResponse(status, body)
		case shared.ActionMockResponse:
			status := intParam(action.Params["status"], http.StatusOK)
			body, _ := action.Params["body"].(string)
			resp := textResponse(status, body)
			if headers, ok := action.Params["headers"].(map[string]string); ok {
				for k, v := range headers {
					resp.Header.Set(k, v)
				}
			}
			ex.decision = "respond"
			synthetic = resp
		case shared.ActionAbort:
			ex.decision = "reject"
			return nil, &errReject{status: http.StatusForbidden, body: "rejected by rule"}
		}
	}
	return synthetic, nil
}

func (ex *Exchange) viewRequest() *shared.Request {
	u := ex.outbound.URL
	_, flat := headerFields(ex.outbound.Header)
	return &shared.Request{
		ID:             ex.record.ID,
		Method:         ex.outbound.Method,
		Scheme:         u.Scheme,
		Host:           u.Host,
		Path:           u.Path,
		Query:          u.RawQuery,
		URL:            u.String(),
		OriginalURL:    ex.record.OriginalURL,
		RequestHeaders: flat,
		ClientAddr:     ex.record.ClientAddr,
	}
}

func (p *Proxy) upstreamTerminal(ex *Exchange) (*http.Response, error) {
	if ex.outbound.Body == nil {
		ex.outbound.Body = http.NoBody
		ex.outbound.ContentLength = 0
	}
	ex.outbound.RequestURI = ""
	if isWebSocketUpgrade(ex.outbound) {
		return p.websocketRoundTrip(ex)
	}
	resp, err := p.roundTripper().RoundTrip(ex.outbound.WithContext(ex.ctx))
	if err != nil {
		if ex.failureStage == "" {
			ex.failureStage = classifyStage(err)
		}
		return nil, err
	}
	return resp, nil
}

func (p *Proxy) finishExchange(ex *Exchange, status int, resp *http.Response, w http.ResponseWriter, eventKind string) {
	if resp == nil {
		resp = textResponse(status, http.StatusText(status))
	}
	if resp.Body == nil {
		resp.Body = io.NopCloser(strings.NewReader(""))
	}
	defer resp.Body.Close()

	if ex.reqCapture != nil && len(ex.reqCapture.buf) > 0 && ex.record.RequestBodyRef == "" {
		ex.record.RequestBodyRef = p.BodyCache.Store(append([]byte(nil), ex.reqCapture.buf...))
		p.emit(shared.EventRequestBodyAvailable, ex.record.ID, nil)
	}
	if ex.reqCapture != nil && ex.reqCapture.n > 0 {
		ex.record.RequestBodyBytes = ex.reqCapture.n
		if ex.reqCapture.truncated {
			ex.record.RequestBodyTruncated = true
		}
	}

	respHeaders, flatResp := headerFields(resp.Header)
	ex.record.Status = status
	if resp.StatusCode != 0 {
		ex.record.Status = resp.StatusCode
	}
	ex.record.ResponseHeaders = flatResp
	ex.record.ResponseHeaderList = respHeaders
	if len(ex.tags) > 0 {
		ex.record.Tags = append(ex.record.Tags, ex.tags...)
	}
	if len(ex.traces) > 0 {
		ex.record.RuleTrace = append([]shared.RuleTrace(nil), ex.traces...)
	}
	ex.record.FailureStage = ex.failureStage
	if ex.outbound != nil && ex.outbound.URL != nil {
		ex.record.EffectiveURL = ex.outbound.URL.String()
	}

	p.emit(shared.EventResponseHeaders, ex.record.ID, nil)

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(ex.record.Status)

	ex.respCapture = &captureReader{r: resp.Body, limit: shared.MaxBodySize}
	dst := io.Writer(w)
	if f, ok := w.(http.Flusher); ok {
		dst = flushWriter{w: w, f: f}
	}
	_, copyErr := io.Copy(dst, ex.respCapture)
	if len(ex.respCapture.buf) > 0 {
		ex.record.ResponseBodyRef = p.BodyCache.Store(append([]byte(nil), ex.respCapture.buf...))
		p.emit(shared.EventResponseBodyAvailable, ex.record.ID, nil)
	}
	ex.record.ResponseBodyBytes = ex.respCapture.n
	ex.record.ResponseBodyTruncated = ex.respCapture.truncated

	now := time.Now()
	ex.record.CompletedAt = &now
	ex.record.DurationMs = now.Sub(ex.started).Milliseconds()
	if copyErr != nil && ex.record.FailureStage == "" {
		ex.record.FailureStage = "downstream_write"
		eventKind = "request_failed"
	}
	payload := map[string]any{"status": ex.record.Status}
	if ex.record.FailureStage != "" {
		payload["stage"] = ex.record.FailureStage
	}
	p.emit(eventKindType(eventKind), ex.record.ID, payload)
	if p.Sessions != nil && ex.record.SessionID != "" {
		p.Sessions.Note("request_completed", ex.record.SessionID, string(ex.record.ID), "", ex.record.Method+" "+ex.record.URL)
	}
	p.persist(ex.record)
}

func (p *Proxy) emit(typ shared.EventType, id shared.RequestID, payload any) {
	if p.EventHub == nil {
		return
	}
	p.EventHub.Emit(&shared.Event{
		Type:      typ,
		RequestID: id,
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

func eventKindType(kind string) shared.EventType {
	if kind == "request_failed" {
		return shared.EventRequestFailed
	}
	return shared.EventResponseCompleted
}

func (p *Proxy) roundTripper() http.RoundTripper {
	p.transportOnce.Do(func() {
		tr := &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		if p.DialTLS != nil {
			dialTLS := p.DialTLS
			tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, splitErr := net.SplitHostPort(addr)
				if splitErr != nil {
					host = addr
				}
				d := &net.Dialer{Timeout: 10 * time.Second}
				return dialTLS(d, network, addr, &tls.Config{ServerName: host, NextProtos: []string{"h2", "http/1.1"}})
			}
		}
		p.httpTransport = tr
	})
	return p.httpTransport
}

func textResponse(status int, body string) *http.Response {
	if status == 0 {
		status = http.StatusOK
	}
	resp := &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	if body != "" {
		resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	return resp
}

func copyHeader(dst, src http.Header) {
	hop := hopTokens(src)
	for k, vs := range src {
		if hop[strings.ToLower(k)] || isFixedHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func stripHopByHop(h http.Header) {
	hop := hopTokens(h)
	for k := range h {
		if hop[strings.ToLower(k)] || isFixedHop(k) {
			h.Del(k)
		}
	}
}

func hopTokens(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, c := range h.Values("Connection") {
		for _, token := range strings.Split(c, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token != "" {
				out[token] = true
			}
		}
	}
	return out
}

func isFixedHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade":
		return true
	}
	return false
}

func headerFields(h http.Header) ([]shared.HeaderField, shared.Headers) {
	flat := shared.Headers{}
	if len(h) == 0 {
		return nil, flat
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([]shared.HeaderField, 0, len(keys))
	for _, k := range keys {
		vs := append([]string(nil), h[k]...)
		list = append(list, shared.HeaderField{Name: k, Values: vs})
		if strings.EqualFold(k, "Set-Cookie") {
			if len(vs) > 0 {
				flat[k] = vs[0]
			}
			continue
		}
		flat[k] = strings.Join(vs, ", ")
	}
	return list, flat
}

func readPreview(r io.Reader, limit int) ([]byte, []byte, error) {
	if r == nil {
		return nil, nil, nil
	}
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	if n < 0 {
		n = 0
	}
	if n > limit {
		return append([]byte(nil), buf[:limit]...), append([]byte(nil), buf[limit:n]...), err
	}
	return append([]byte(nil), buf[:n]...), nil, err
}

// captureReader forwards every byte and retains at most limit bytes.
type captureReader struct {
	r         io.Reader
	buf       []byte
	limit     int
	n         int64
	truncated bool
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		if len(c.buf) < c.limit {
			space := c.limit - len(c.buf)
			take := n
			if take > space {
				take = space
				c.truncated = true
			}
			c.buf = append(c.buf, p[:take]...)
			if n > space {
				c.truncated = true
			}
		} else {
			c.truncated = true
		}
	}
	return n, err
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.f != nil {
		f.f.Flush()
	}
	return n, err
}

func stringListParam(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func intParam(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return fallback
	}
}

func classifyStage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream_timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "tls"):
		return "tls"
	case strings.Contains(msg, "dial"):
		return "dial"
	default:
		return "upstream"
	}
}

// authorityMatches reports whether a decrypted request and optional TLS SNI
// agree with the CONNECT authority. An absent SNI is accepted for IP literals.
func authorityMatches(connectHost, requestHost, sni string) bool {
	base := normalizeHost(connectHost)
	if base == "" {
		return false
	}
	if requestHost != "" && normalizeHost(requestHost) != base {
		return false
	}
	if sni == "" {
		return net.ParseIP(base) != nil
	}
	return normalizeHost(sni) == base
}
