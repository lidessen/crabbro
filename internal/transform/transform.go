// Package transform applies declarative, stateless upstream request normalization.
package transform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Spec describes operations applied in strip, set, URL, drop, rename, set order.
// Query values replace a key; null removes it. Path and PathPrefix are decoded paths.
type Spec struct {
	StripHeaders     []string           `yaml:"stripHeaders,omitempty" json:"stripHeaders,omitempty"`
	SetHeaders       map[string]string  `yaml:"setHeaders,omitempty" json:"setHeaders,omitempty"`
	Path             *string            `yaml:"path,omitempty" json:"path,omitempty"`
	PathPrefix       *string            `yaml:"pathPrefix,omitempty" json:"pathPrefix,omitempty"`
	Query            map[string]*string `yaml:"query,omitempty" json:"query,omitempty"`
	DropBodyFields   []string           `yaml:"dropBodyFields,omitempty" json:"dropBodyFields,omitempty"`
	RenameBodyFields map[string]string  `yaml:"renameBodyFields,omitempty" json:"renameBodyFields,omitempty"`
	SetBodyFields    map[string]any     `yaml:"setBodyFields,omitempty" json:"setBodyFields,omitempty"`
}

// Validate rejects credential and framing headers, including a custom auth header.
func (s Spec) Validate(protected ...string) error {
	for k, v := range s.SetHeaders {
		name := strings.ToLower(k)
		if name == "authorization" || name == "proxy-authorization" || name == "x-api-key" || name == "x-goog-api-key" || strings.Contains(name, "api-key") || strings.Contains(name, "token") || name == "host" || name == "content-length" || name == "transfer-encoding" {
			return fmt.Errorf("protected header %q cannot be set", k)
		}
		for _, p := range protected {
			if p != "" && strings.EqualFold(k, p) {
				return fmt.Errorf("protected header %q cannot be set", k)
			}
		}
		if !validHeaderName(k) || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid header %q", k)
		}
	}
	for _, p := range s.StripHeaders {
		n := strings.TrimSuffix(p, "*")
		if !validHeaderName(n) {
			return fmt.Errorf("invalid header pattern %q", p)
		}
	}
	if s.Path != nil && *s.Path != "" && !strings.HasPrefix(*s.Path, "/") {
		return fmt.Errorf("path must start with /")
	}
	if s.PathPrefix != nil && *s.PathPrefix != "" && !strings.HasPrefix(*s.PathPrefix, "/") {
		return fmt.Errorf("pathPrefix must start with /")
	}
	for _, p := range s.DropBodyFields {
		if !validPath(p) {
			return fmt.Errorf("invalid body field path")
		}
	}
	for p, dst := range s.RenameBodyFields {
		if !validPath(p) || !validPath(dst) || p == dst || strings.HasPrefix(dst, p+".") {
			return fmt.Errorf("invalid body field rename")
		}
	}
	for p := range s.SetBodyFields {
		if !validPath(p) {
			return fmt.Errorf("invalid body field path")
		}
	}
	return nil
}
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}
func validPath(s string) bool {
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
	}
	return true
}

// Apply returns independent copies; no input is modified even on error.
func Apply(u *url.URL, h http.Header, body []byte, s Spec, protected ...string) (*url.URL, http.Header, []byte, error) {
	if u == nil {
		return nil, nil, nil, fmt.Errorf("nil URL")
	}
	if err := s.Validate(protected...); err != nil {
		return nil, nil, nil, err
	}
	out := *u
	headers := h.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	b := append([]byte(nil), body...)
	for _, pattern := range s.StripHeaders {
		prefix := strings.HasSuffix(pattern, "*")
		p := strings.ToLower(strings.TrimSuffix(pattern, "*"))
		for k := range headers {
			n := strings.ToLower(k)
			if n == p || prefix && strings.HasPrefix(n, p) {
				delete(headers, k)
			}
		}
		if (prefix && strings.HasPrefix("user-agent", p)) || (!prefix && p == "user-agent") {
			headers["User-Agent"] = []string{""}
		}
	}
	for k, v := range s.SetHeaders {
		headers.Set(k, v)
	}
	if s.Path != nil {
		out.Path = *s.Path
		out.RawPath = ""
	}
	if s.PathPrefix != nil {
		out.Path = strings.TrimSuffix(*s.PathPrefix, "/") + "/" + strings.TrimPrefix(out.Path, "/")
		out.RawPath = ""
	}
	if s.Query != nil {
		q := out.Query()
		for k, v := range s.Query {
			if v == nil {
				q.Del(k)
			} else {
				q.Set(k, *v)
			}
		}
		out.RawQuery = q.Encode()
	}
	if len(s.DropBodyFields)+len(s.RenameBodyFields)+len(s.SetBodyFields) > 0 {
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil || obj == nil {
			return nil, nil, nil, fmt.Errorf("transform body must be a JSON object")
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, nil, nil, fmt.Errorf("transform body must be a JSON object")
		}
		for _, p := range s.DropBodyFields {
			remove(obj, p)
		}
		keys := sorted(s.RenameBodyFields)
		for _, p := range keys {
			if v, ok := remove(obj, p); ok {
				if err := put(obj, s.RenameBodyFields[p], v); err != nil {
					return nil, nil, nil, err
				}
			}
		}
		for _, p := range sorted(s.SetBodyFields) {
			if err := put(obj, p, s.SetBodyFields[p]); err != nil {
				return nil, nil, nil, err
			}
		}
		var err error
		b, err = json.Marshal(obj)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("invalid transform body value: %w", err)
		}
	}
	headers.Del("Content-Length")
	return &out, headers, b, nil
}
func sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func remove(m map[string]any, p string) (any, bool) {
	parts := strings.Split(p, ".")
	for _, k := range parts[:len(parts)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	k := parts[len(parts)-1]
	v, ok := m[k]
	delete(m, k)
	return v, ok
}
func put(m map[string]any, p string, v any) error {
	parts := strings.Split(p, ".")
	for _, k := range parts[:len(parts)-1] {
		x, ok := m[k]
		if !ok {
			next := map[string]any{}
			m[k] = next
			m = next
			continue
		}
		next, ok := x.(map[string]any)
		if !ok {
			return fmt.Errorf("body field parent is not an object")
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
	return nil
}

// Request applies specs in order and updates HTTP framing to match the body.
func Request(r *http.Request, body []byte, protected string, specs ...Spec) ([]byte, error) {
	u, h, b := r.URL, r.Header, body
	for _, s := range specs {
		var err error
		u, h, b, err = Apply(u, h, b, s, protected)
		if err != nil {
			return nil, err
		}
	}
	r.URL = u
	r.Header = h
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	return b, nil
}
