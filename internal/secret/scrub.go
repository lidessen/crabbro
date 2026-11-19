package secret

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// RedactedPlaceholder replaces secret values in scrubbed payloads and logs.
const RedactedPlaceholder = "[REDACTED]"

const minScrubLength = 6

var defaultAuthHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Api-Key",
	"X-Goog-Api-Key",
}

var sensitiveQueryParams = []string{
	"key",
	"api_key",
	"apikey",
	"access_token",
}

// StripAuthHeaders returns a cloned header map with authentication headers removed.
func StripAuthHeaders(h http.Header, extraHeaders ...string) http.Header {
	if h == nil {
		return nil
	}
	out := h.Clone()
	for _, name := range defaultAuthHeaders {
		out.Del(name)
	}
	for _, name := range extraHeaders {
		if strings.TrimSpace(name) != "" {
			out.Del(name)
		}
	}
	return out
}

// ScrubHeaders clones h, strips authentication headers, and redacts any remaining
// header values that contain known secrets.
func ScrubHeaders(h http.Header, values []string, extraHeaders ...string) http.Header {
	out := StripAuthHeaders(h, extraHeaders...)
	if out == nil || len(values) == 0 {
		return out
	}
	cleanVals := normalizeValues(values)
	if len(cleanVals) == 0 {
		return out
	}
	for k, vs := range out {
		for i, v := range vs {
			vs[i] = scrubNormalized(v, cleanVals)
		}
		out[k] = vs
	}
	return out
}

// ScrubURL strips sensitive credential query parameters (such as Gemini ?key=...)
// from rawURL while preserving all non-secret URL parts.
func ScrubURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.User != nil {
		u.User = nil
	}
	q := u.Query()
	changed := false
	for _, p := range sensitiveQueryParams {
		if q.Has(p) {
			q.Del(p)
			changed = true
		}
	}
	// Also strip case-insensitive variants.
	for k := range q {
		lk := strings.ToLower(k)
		for _, p := range sensitiveQueryParams {
			if lk == p {
				q.Del(k)
				changed = true
				break
			}
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// ScrubText replaces any resolved secret values (length >= 6) in s with [REDACTED].
func ScrubText(s string, values ...string) string {
	if s == "" || len(values) == 0 {
		return s
	}
	cleanVals := normalizeValues(values)
	if len(cleanVals) == 0 {
		return s
	}
	return scrubNormalized(s, cleanVals)
}

// ScrubAny recursively replaces any resolved secret values inside maps, slices, and strings.
func ScrubAny(v any, values ...string) any {
	cleanVals := normalizeValues(values)
	if len(cleanVals) == 0 {
		return v
	}
	return scrubValue(v, cleanVals)
}

func scrubValue(v any, vals []string) any {
	switch t := v.(type) {
	case string:
		return scrubNormalized(t, vals)
	case []string:
		out := make([]string, len(t))
		for i, item := range t {
			out[i] = scrubNormalized(item, vals)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = scrubValue(item, vals)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, item := range t {
			out[k] = scrubNormalized(item, vals)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = scrubValue(item, vals)
		}
		return out
	default:
		return v
	}
}

func normalizeValues(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minScrubLength {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	// Replace longest values first so shorter substrings do not leave fragments behind.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

func scrubNormalized(s string, vals []string) string {
	for _, v := range vals {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, RedactedPlaceholder)
		}
	}
	return s
}
