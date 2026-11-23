package transform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MaxResponseBody is the maximum buffered upstream and transformed response size.
const MaxResponseBody = 16 * 1024 * 1024

// ResponseSpec selects a nested JSON object and then applies body field operations.
// UnwrapPath and ExtractField are equivalent dot-separated object paths and are
// mutually exclusive. Endpoint specifications run before model specifications.
type ResponseSpec struct {
	StripHeaders     []string          `yaml:"stripHeaders,omitempty" json:"stripHeaders,omitempty"`
	SetHeaders       map[string]string `yaml:"setHeaders,omitempty" json:"setHeaders,omitempty"`
	UnwrapPath       string            `yaml:"unwrapPath,omitempty" json:"unwrapPath,omitempty"`
	ExtractField     string            `yaml:"extractField,omitempty" json:"extractField,omitempty"`
	DropBodyFields   []string          `yaml:"dropBodyFields,omitempty" json:"dropBodyFields,omitempty"`
	RenameBodyFields map[string]string `yaml:"renameBodyFields,omitempty" json:"renameBodyFields,omitempty"`
	SetBodyFields    map[string]any    `yaml:"setBodyFields,omitempty" json:"setBodyFields,omitempty"`
}

func (s ResponseSpec) spec() Spec {
	return Spec{StripHeaders: s.StripHeaders, SetHeaders: s.SetHeaders, DropBodyFields: s.DropBodyFields, RenameBodyFields: s.RenameBodyFields, SetBodyFields: s.SetBodyFields}
}

// Validate checks header and body operations without exposing body values.
func (s ResponseSpec) Validate(protected ...string) error {
	if s.UnwrapPath != "" && s.ExtractField != "" {
		return fmt.Errorf("unwrapPath and extractField are mutually exclusive")
	}
	for _, p := range []string{s.UnwrapPath, s.ExtractField} {
		if p != "" && !validPath(p) {
			return fmt.Errorf("invalid response field path")
		}
	}
	return s.spec().Validate(protected...)
}

// ReadResponse reads a complete body or returns an error instead of truncating it.
func ReadResponse(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxResponseBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", MaxResponseBody)
	}
	return b, nil
}

// Response applies response specifications without modifying the supplied headers or body.
func Response(h http.Header, body []byte, protected string, specs ...ResponseSpec) (http.Header, []byte, error) {
	if len(body) > MaxResponseBody {
		return nil, nil, fmt.Errorf("response body exceeds %d bytes", MaxResponseBody)
	}
	headers, b := h.Clone(), append([]byte(nil), body...)
	for _, s := range specs {
		if err := s.Validate(protected); err != nil {
			return nil, nil, err
		}
		path := s.UnwrapPath
		if path == "" {
			path = s.ExtractField
		}
		if path != "" {
			var obj map[string]any
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil || obj == nil {
				return nil, nil, fmt.Errorf("response body must be a JSON object")
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				return nil, nil, fmt.Errorf("response body must be a JSON object")
			}
			var value any = obj
			for _, part := range strings.Split(path, ".") {
				m, ok := value.(map[string]any)
				if !ok {
					return nil, nil, fmt.Errorf("response field is not an object")
				}
				value, ok = m[part]
				if !ok {
					return nil, nil, fmt.Errorf("response field not found")
				}
			}
			if _, ok := value.(map[string]any); !ok || value == nil {
				return nil, nil, fmt.Errorf("response field is not an object")
			}
			var err error
			b, err = json.Marshal(value)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid response body value")
			}
		}
		// Share object field operations without request-specific header behavior.
		_, next, out, err := apply(&url.URL{}, headers, b, s.spec(), false, protected)
		if err != nil {
			return nil, nil, err
		}
		headers, b = next, out
		if len(b) > MaxResponseBody {
			return nil, nil, fmt.Errorf("response body exceeds %d bytes", MaxResponseBody)
		}
	}
	return headers, b, nil
}
