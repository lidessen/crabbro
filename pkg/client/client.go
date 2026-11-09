// Package client is a typed control-API client for chilingd.
//
// It speaks HTTP over the same Unix socket or localhost TCP listener the CLI
// uses. A future native UI imports this package and pkg/api directly. It does
// not import a UI toolkit, and it does not link the daemon into the UI process:
// the proxy keeps running when the window closes.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lidessen/chiling/pkg/api"
)

// Options selects how the client reaches the daemon.
//
// An empty UDS, TCP, and BaseURL uses the default socket and localhost TCP
// fallback. BaseURL, when set, is the only target (tests use an httptest server).
// Timeout applies to unary calls. Zero means 10 seconds. Subscribe does not use it.
type Options struct {
	UDS       string
	TCP       string
	Token     string
	Timeout   time.Duration
	BaseURL   string
	Transport http.RoundTripper
}

// Client calls the /v1 control API.
type Client struct {
	uds     string
	tcp     string
	token   string
	timeout time.Duration
	baseURL string
	rt      http.RoundTripper
}

// APIError is a non-2xx control API response.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Details    []api.ErrorDetail
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("control API status %d", e.StatusCode)
}

// New returns a client. The daemon is not started; see EnsureDaemon.
func New(opt Options) *Client {
	if opt.UDS == "" && opt.TCP == "" && opt.BaseURL == "" {
		opt.UDS = expandPath(api.DefaultUDSPath)
		opt.TCP = api.DefaultTCPAddr
	} else if opt.UDS != "" {
		opt.UDS = expandPath(opt.UDS)
	}
	if opt.Timeout == 0 {
		opt.Timeout = 10 * time.Second
	}
	return &Client{
		uds:     opt.UDS,
		tcp:     opt.TCP,
		token:   opt.Token,
		timeout: opt.Timeout,
		baseURL: strings.TrimRight(opt.BaseURL, "/"),
		rt:      opt.Transport,
	}
}

// Do performs one HTTP request. UDS is tried first, then TCP.
// A non-2xx response is returned with a nil error so callers can read the body.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.do(ctx, method, path, body, c.timeout)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, timeout time.Duration) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}
	stream := timeout == 0
	if c.baseURL != "" {
		return c.roundTrip(ctx, c.baseURL, method, path, payload, timeout, stream, c.rt)
	}
	var first error
	if c.uds != "" {
		resp, err := c.roundTrip(ctx, "http://localhost", method, path, payload, timeout, stream, c.unixTransport(stream))
		if err == nil {
			return resp, nil
		}
		first = err
	}
	if c.tcp != "" {
		return c.roundTrip(ctx, "http://"+c.tcp, method, path, payload, timeout, stream, c.rt)
	}
	if first != nil {
		return nil, first
	}
	return nil, fmt.Errorf("no control API connection available (tried UDS and TCP)")
}

func (c *Client) unixTransport(stream bool) http.RoundTripper {
	return &http.Transport{
		DisableKeepAlives: !stream,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.uds)
		},
	}
}

// roundTrip performs one HTTP request over rt. A streaming call (events)
// marks the request non-reusable: when its body is closed early, the transport
// closes the connection at once instead of draining it for up to 50ms before
// the FIN, which would leave the daemon serving a dead subscriber while the
// subscriber loop already reconnected.
func (c *Client) roundTrip(ctx context.Context, base, method, path string, payload []byte, timeout time.Duration, stream bool, rt http.RoundTripper) (*http.Response, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Close = stream
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	client := &http.Client{Timeout: timeout, Transport: rt}
	return client.Do(req)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

func (c *Client) postJSON(ctx context.Context, path string, payload, out any) error {
	return c.methodJSON(ctx, http.MethodPost, path, payload, out)
}

func (c *Client) putJSON(ctx context.Context, path string, payload, out any) error {
	return c.methodJSON(ctx, http.MethodPut, path, payload, out)
}

func (c *Client) methodJSON(ctx context.Context, method, path string, payload, out any) error {
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	resp, err := c.Do(ctx, method, path, rdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var er api.ErrorResponse
		_ = json.Unmarshal(b, &er)
		return &APIError{
			StatusCode: resp.StatusCode,
			Code:       er.Error.Code,
			Message:    er.Error.Message,
			Details:    er.Error.Details,
		}
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return err
	}
	return nil
}

func expandPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// Status is GET /v1/status.
func (c *Client) Status(ctx context.Context) (api.DaemonStatus, error) {
	var st api.DaemonStatus
	err := c.getJSON(ctx, "/v1/status", &st)
	return st, err
}

// RequestQuery is the filter for GET /v1/requests.
type RequestQuery struct {
	Since     string
	SinceTime string
	Method    string
	Host      string
	Path      string
	Status    string
	Query     string
	Tag       string
	Session   string
	Type      string
	Initiator string
	Limit     int
	All       bool
}

func (q RequestQuery) values() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("since", q.Since)
	set("sinceTime", q.SinceTime)
	set("method", q.Method)
	set("host", q.Host)
	set("path", q.Path)
	set("status", q.Status)
	set("q", q.Query)
	set("tag", q.Tag)
	set("session", q.Session)
	set("type", q.Type)
	set("initiator", q.Initiator)
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprintf("%d", q.Limit))
	}
	if q.All {
		v.Set("all", "1")
	}
	return v
}

// ListRequests is GET /v1/requests. HiddenCount counts exchanges the current
// scope hid from this response.
func (c *Client) ListRequests(ctx context.Context, q RequestQuery) (api.ListRequestsResponse, error) {
	path := "/v1/requests"
	if enc := q.values().Encode(); enc != "" {
		path += "?" + enc
	}
	var out api.ListRequestsResponse
	err := c.getJSON(ctx, path, &out)
	if out.Requests == nil {
		out.Requests = []api.RequestSummary{}
	}
	return out, err
}

// SearchRequests is ListRequests with a full-text query.
func (c *Client) SearchRequests(ctx context.Context, query string, q RequestQuery) (api.ListRequestsResponse, error) {
	q.Query = query
	return c.ListRequests(ctx, q)
}

// Include selects extra fields on GET /v1/requests/{id}.
type Include struct {
	Headers   bool
	Body      bool
	RuleTrace bool
}

// GetRequest is GET /v1/requests/{id}. Body and ResponseBody are set when
// Include.Body is true and the daemon still has the bytes.
func (c *Client) GetRequest(ctx context.Context, id string, inc Include) (api.RequestDetailResponse, error) {
	var parts []string
	if inc.Headers {
		parts = append(parts, "headers")
	}
	if inc.Body {
		parts = append(parts, "body")
	}
	if inc.RuleTrace {
		parts = append(parts, "rule-trace")
	}
	path := "/v1/requests/" + url.PathEscape(id)
	if len(parts) > 0 {
		path += "?include=" + strings.Join(parts, ",")
	}
	var out api.RequestDetailResponse
	err := c.getJSON(ctx, path, &out)
	return out, err
}

// ListSources is GET /v1/sources.
func (c *Client) ListSources(ctx context.Context) (api.SourceList, error) {
	var out api.SourceList
	err := c.getJSON(ctx, "/v1/sources", &out)
	if out.Sources == nil {
		out.Sources = []api.Source{}
	}
	return out, err
}

// GetSource is GET /v1/sources/{id}.
func (c *Client) GetSource(ctx context.Context, id string) (api.SourceDetail, error) {
	var out api.SourceDetail
	err := c.getJSON(ctx, "/v1/sources/"+url.PathEscape(id), &out)
	if out.Pages == nil {
		out.Pages = []api.Page{}
	}
	return out, err
}

// Scope is GET /v1/scope. The daemon opens an implicit recording session when
// none is active.
func (c *Client) Scope(ctx context.Context) (api.ScopeState, error) {
	var out api.ScopeState
	err := c.getJSON(ctx, "/v1/scope", &out)
	return out, err
}

func (c *Client) scopeOp(ctx context.Context, op string, expr []string) (api.ScopeState, error) {
	if expr == nil {
		expr = []string{}
	}
	var out api.ScopeState
	err := c.postJSON(ctx, "/v1/scope", map[string]any{"op": op, "expr": expr}, &out)
	return out, err
}

// ScopePick replaces the session pick list.
func (c *Client) ScopePick(ctx context.Context, expr []string) (api.ScopeState, error) {
	return c.scopeOp(ctx, "pick", expr)
}

// ScopeOmit adds omit expressions.
func (c *Client) ScopeOmit(ctx context.Context, expr []string) (api.ScopeState, error) {
	return c.scopeOp(ctx, "omit", expr)
}

// ScopeOff adds omit expressions. It is the same daemon operation as ScopeOmit.
func (c *Client) ScopeOff(ctx context.Context, expr []string) (api.ScopeState, error) {
	return c.scopeOp(ctx, "off", expr)
}

// ScopeOn restores one omitted expression.
func (c *Client) ScopeOn(ctx context.Context, expr string) (api.ScopeState, error) {
	return c.scopeOp(ctx, "on", []string{expr})
}

// ScopeReset clears the session scope.
func (c *Client) ScopeReset(ctx context.Context) (api.ScopeState, error) {
	return c.scopeOp(ctx, "reset", nil)
}

// ScopeBypass adds bypass expressions. Matching connections skip MITM and recording.
func (c *Client) ScopeBypass(ctx context.Context, expr []string) (api.ScopeState, error) {
	return c.scopeOp(ctx, "bypass", expr)
}

// ListRules is GET /v1/rules.
func (c *Client) ListRules(ctx context.Context) ([]api.Rule, error) {
	var resp api.ListRulesResponse
	err := c.getJSON(ctx, "/v1/rules", &resp)
	out := make([]api.Rule, 0, len(resp.Rules))
	for _, rule := range resp.Rules {
		if rule != nil {
			out = append(out, *rule)
		}
	}
	return out, err
}

// GetRule is GET /v1/rules/{id}.
func (c *Client) GetRule(ctx context.Context, id string) (api.Rule, error) {
	var out api.GetRuleResponse
	err := c.getJSON(ctx, "/v1/rules/"+url.PathEscape(id), &out)
	return out.Rule, err
}

// PreviewRules is POST /v1/rules/preview.
func (c *Client) PreviewRules(ctx context.Context, method, host, path string) (api.RulePreview, error) {
	var out api.RulePreview
	err := c.postJSON(ctx, "/v1/rules/preview", map[string]string{
		"method": method, "host": host, "path": path,
	}, &out)
	if out.Matched == nil {
		out.Matched = []string{}
	}
	if out.Disabled == nil {
		out.Disabled = []string{}
	}
	return out, err
}

// ValidateRule is POST /v1/config/validate for a rule draft.
func (c *Client) ValidateRule(ctx context.Context, id string) (api.ValidateResult, error) {
	q := url.Values{}
	q.Set("id", id)
	var out api.ValidateResult
	err := c.postJSON(ctx, "/v1/config/validate?"+q.Encode(), nil, &out)
	return out, err
}

// DraftRule stores a rule body without activating it.
func (c *Client) DraftRule(ctx context.Context, id string, body []byte) (api.ConfigAck, error) {
	return c.postConfig(ctx, "draft", id, body)
}

// CommitRule activates a drafted rule.
func (c *Client) CommitRule(ctx context.Context, id string) (api.ConfigAck, error) {
	return c.postConfig(ctx, "commit", id, nil)
}

// DiscardRule drops a rule draft.
func (c *Client) DiscardRule(ctx context.Context, id string) (api.ConfigAck, error) {
	return c.postConfig(ctx, "discard", id, nil)
}

func (c *Client) postConfig(ctx context.Context, action, id string, body []byte) (api.ConfigAck, error) {
	q := url.Values{}
	q.Set("id", id)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	resp, err := c.Do(ctx, http.MethodPost, "/v1/config/"+action+"?"+q.Encode(), rdr)
	if err != nil {
		return api.ConfigAck{}, err
	}
	defer resp.Body.Close()
	var out api.ConfigAck
	err = decode(resp, &out)
	return out, err
}

func (c *Client) toggleRule(ctx context.Context, id string, enabled bool) (api.Rule, error) {
	var out api.Rule
	err := c.postJSON(ctx, "/v1/rules/"+url.PathEscape(id)+"/toggle", map[string]bool{"enabled": enabled}, &out)
	return out, err
}

// EnableRule turns a rule on.
func (c *Client) EnableRule(ctx context.Context, id string) (api.Rule, error) {
	return c.toggleRule(ctx, id, true)
}

// DisableRule turns a rule off.
func (c *Client) DisableRule(ctx context.Context, id string) (api.Rule, error) {
	return c.toggleRule(ctx, id, false)
}

// UpdateRule changes a rule's name, priority, or script. A nil field keeps
// the value the rule has. This is PUT /v1/rules/{id}.
func (c *Client) UpdateRule(ctx context.Context, id string, req api.UpdateRuleRequest) (api.Rule, error) {
	var out api.Rule
	err := c.putJSON(ctx, "/v1/rules/"+url.PathEscape(id), req, &out)
	return out, err
}

// DeleteRule removes a rule. This is DELETE /v1/rules/{id}.
func (c *Client) DeleteRule(ctx context.Context, id string) error {
	resp, err := c.Do(ctx, http.MethodDelete, "/v1/rules/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, nil)
}

// ListPlugins is GET /v1/plugins.
func (c *Client) ListPlugins(ctx context.Context) ([]api.Plugin, error) {
	var out api.PluginList
	err := c.getJSON(ctx, "/v1/plugins", &out)
	if out.Plugins == nil {
		out.Plugins = []api.Plugin{}
	}
	return out.Plugins, err
}

// InspectPlugin is GET /v1/plugins/{id}. The host returns a free-form description.
func (c *Client) InspectPlugin(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	err := c.getJSON(ctx, "/v1/plugins/"+url.PathEscape(id), &out)
	return out, err
}

// PluginTools is GET /v1/plugins/{id}/tools.
func (c *Client) PluginTools(ctx context.Context, id string) (any, error) {
	var out struct {
		Tools any `json:"tools"`
	}
	err := c.getJSON(ctx, "/v1/plugins/"+url.PathEscape(id)+"/tools", &out)
	return out.Tools, err
}

// InvokePlugin calls one tool on a plugin instance.
func (c *Client) InvokePlugin(ctx context.Context, id, instance, tool string, arguments map[string]any) (map[string]any, error) {
	path := "/v1/plugins/" + url.PathEscape(id) + "/instances/" + url.PathEscape(instance) + "/tools/" + url.PathEscape(tool)
	var out map[string]any
	err := c.postJSON(ctx, path, map[string]any{"arguments": arguments}, &out)
	return out, err
}

// ListSessions is GET /v1/sessions.
func (c *Client) ListSessions(ctx context.Context) ([]api.Session, error) {
	var out api.SessionList
	err := c.getJSON(ctx, "/v1/sessions", &out)
	if out.Sessions == nil {
		out.Sessions = []api.Session{}
	}
	return out.Sessions, err
}

// CurrentSession is GET /v1/sessions/current. The pointer is nil when nothing is recording.
func (c *Client) CurrentSession(ctx context.Context) (*api.Session, error) {
	var out api.CurrentSession
	err := c.getJSON(ctx, "/v1/sessions/current", &out)
	return out.Session, err
}

// StartRecording opens a named recording session.
func (c *Client) StartRecording(ctx context.Context, name string) (api.Session, error) {
	var out api.Session
	err := c.postJSON(ctx, "/v1/sessions", map[string]string{"name": name}, &out)
	return out, err
}

// StopRecording ends a session. An empty id stops the current one.
func (c *Client) StopRecording(ctx context.Context, id string) (api.Session, error) {
	path := "/v1/sessions/" + url.PathEscape(id) + "/stop"
	if id == "" {
		cur, err := c.CurrentSession(ctx)
		if err != nil {
			return api.Session{}, err
		}
		if cur == nil {
			return api.Session{}, &APIError{StatusCode: http.StatusNotFound, Code: api.ErrCodeNotFound, Message: "no recording session"}
		}
		path = "/v1/sessions/" + url.PathEscape(cur.ID) + "/stop"
	}
	var out api.Session
	err := c.postJSON(ctx, path, map[string]any{}, &out)
	return out, err
}

// Activity is the paged trajectory, oldest first. cursor is an act_N id from
// the previous page's NextCursor. An empty cursor starts at the oldest retained step.
func (c *Client) Activity(ctx context.Context, session, cursor string, limit int) (api.TrajectoryPage, error) {
	return c.Trajectory(ctx, session, cursor, limit)
}

// Trajectory is GET /v1/trajectory.
func (c *Client) Trajectory(ctx context.Context, session, cursor string, limit int) (api.TrajectoryPage, error) {
	q := url.Values{}
	if session != "" {
		q.Set("session", session)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := "/v1/trajectory"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out api.TrajectoryPage
	err := c.getJSON(ctx, path, &out)
	if out.Items == nil {
		out.Items = []api.Activity{}
	}
	return out, err
}

// Replay sends a captured request back through the proxy.
func (c *Client) Replay(ctx context.Context, req api.VerifyRequest) (api.VerifyResult, error) {
	var out api.VerifyResult
	err := c.postJSON(ctx, "/v1/verify/replay", req, &out)
	return out, err
}

// Verify is Replay.
func (c *Client) Verify(ctx context.Context, req api.VerifyRequest) (api.VerifyResult, error) {
	return c.Replay(ctx, req)
}

// History is GET /v1/history.
func (c *Client) History(ctx context.Context) (api.HistoryStatus, error) {
	var out api.HistoryStatus
	err := c.getJSON(ctx, "/v1/history", &out)
	return out, err
}

// CAStatus is GET /v1/mitm/status. The route exists only when the daemon was
// started with a CA configured.
func (c *Client) CAStatus(ctx context.Context) (api.MITMStatusResponse, error) {
	var out api.MITMStatusResponse
	err := c.getJSON(ctx, "/v1/mitm/status", &out)
	return out, err
}
