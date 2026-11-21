package executor

import (
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

const (
	testAPIKey      = "sk-abc123456789012345"
	testBearerToken = "Bearer secret-token-value"
	testEmail       = "user@example.com"
)

// assertSafeSummary enforces the classification boundary: summaries are
// bounded, single-line, and free of the upstream free text used to pick the
// class.
func assertSafeSummary(t *testing.T, summary string) {
	t.Helper()
	if summary == "" {
		t.Fatal("classified summary must not be empty")
	}
	if n := utf8.RuneCountInString(summary); n > 120 {
		t.Errorf("summary %q is %d runes, want <= 120", summary, n)
	}
	for _, banned := range []string{testAPIKey, "secret-token-value", testEmail, "<html", "<body", "?api_key=", "\n", "\r", "\t"} {
		if strings.Contains(summary, banned) {
			t.Errorf("summary %q leaked upstream text %q", summary, banned)
		}
	}
	for _, r := range summary {
		if unicode.IsControl(r) {
			t.Errorf("summary %q contains control character %U", summary, r)
		}
	}
}

func TestClassifyUnsupportedResponseFormatIsRetryable(t *testing.T) {
	body := []byte(`{"error":{"message":"This response_format type is unavailable now","type":"invalid_request_error"}}`)
	fc := ClassifyHTTPFailure(http.StatusBadRequest, body, make(http.Header), "deepseek", nil)
	if !fc.Retryable || fc.Kind != ErrKindModelUnavailable || fc.CooldownScope != "model" {
		t.Fatalf("unexpected classification: %+v", fc)
	}
	assertSafeSummary(t, fc.Summary)
}

// An upstream response body may be an HTML dump full of credentials, emails,
// query tokens, and newlines. The classifier may inspect it, but the result
// must be a fixed, deterministic summary.
func TestClassifyHTTPFailureNeverCopiesUpstreamBody(t *testing.T) {
	body := []byte("<html><body>" + testAPIKey + " " + testBearerToken + " " + testEmail +
		"\nhttps://upstream.example/v1/chat?api_key=leaked-token</body></html>")
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusTooManyRequests, "rate limited (429)"},
		{http.StatusUnauthorized, "upstream auth error (401)"},
		{http.StatusForbidden, "upstream permission denied (403)"},
		{http.StatusNotFound, "model or endpoint not found (404)"},
		{http.StatusBadRequest, "upstream invalid request (400)"},
		{http.StatusUnprocessableEntity, "upstream invalid request (422)"},
		{http.StatusBadGateway, "upstream server error (502)"},
		{http.StatusTeapot, "upstream rejected request (418)"},
	}
	for _, tc := range cases {
		fc := ClassifyHTTPFailure(tc.status, body, make(http.Header), "plan-a", nil)
		if fc.Summary != tc.want {
			t.Errorf("status %d summary = %q, want %q", tc.status, fc.Summary, tc.want)
		}
		assertSafeSummary(t, fc.Summary)
	}
}

// Quota detection may read the body, but the stored summary stays fixed.
func TestClassifyHTTPFailureQuotaSummaryIsFixed(t *testing.T) {
	body := []byte(`{"error":{"message":"insufficient_quota ` + testAPIKey + ` ` + testEmail + `"}}`)
	fc := ClassifyHTTPFailure(http.StatusTooManyRequests, body, make(http.Header), "plan-a", nil)
	if fc.Kind != ErrKindQuotaExhausted || fc.Summary != "quota exhausted (429)" {
		t.Fatalf("unexpected classification: %+v", fc)
	}
	assertSafeSummary(t, fc.Summary)
}

// Status 0 carries a transport error string; only a fixed timeout/transport
// label may be stored.
func TestClassifyHTTPFailureTransportSummaryIsFixed(t *testing.T) {
	timeout := ClassifyHTTPFailure(0, []byte("dial tcp 10.0.0.1:443: i/o timeout "+testAPIKey), nil, "plan-a", nil)
	if timeout.Kind != ErrKindUpstreamUnavailable || timeout.Summary != "upstream timeout" {
		t.Fatalf("timeout classification = %+v", timeout)
	}
	assertSafeSummary(t, timeout.Summary)

	reset := ClassifyHTTPFailure(0, []byte("connection reset by peer "+testBearerToken), nil, "plan-a", nil)
	if reset.Kind != ErrKindUpstreamUnavailable || reset.Summary != "transport error" {
		t.Fatalf("transport classification = %+v", reset)
	}
	assertSafeSummary(t, reset.Summary)
}

// A fake-200 SSE error frame is fully upstream-controlled, including its
// message. Every class must store its own deterministic summary instead.
func TestClassifySSEFailureNeverCopiesUpstreamMessage(t *testing.T) {
	msg := "<html><body>" + testAPIKey + " " + testBearerToken + " " + testEmail +
		"\nhttps://upstream.example/v1/chat?api_key=leaked-token</body></html>"
	cases := []struct {
		code string
		want string
		kind string
	}{
		{"rate_limit_error", "upstream SSE error (rate_limit)", ErrKindRateLimit},
		{"insufficient_quota", "upstream SSE error (quota_exhausted)", ErrKindQuotaExhausted},
		{"authentication_error", "upstream SSE error (auth_error)", ErrKindAuthError},
		{"context_length_exceeded", "context length exceeded", ErrKindContextOverflow},
		{"model_not_found", "model or endpoint not found", ErrKindModelUnavailable},
		{"server_error", "upstream SSE error", ErrKindUpstreamUnavailable},
	}
	for _, tc := range cases {
		fc := ClassifySSEFailure(tc.code, msg, "plan-a", nil)
		if fc.Kind != tc.kind || fc.Summary != tc.want {
			t.Errorf("code %q classification = %+v, want kind %q summary %q", tc.code, fc, tc.kind, tc.want)
		}
		assertSafeSummary(t, fc.Summary)
	}
}

// Even a multi-kilobyte SSE message yields the same bounded summary.
func TestClassifySSEFailureBoundsHugeMessage(t *testing.T) {
	msg := strings.Repeat("<html><body>upstream dump</body></html>", 4096)
	fc := ClassifySSEFailure("rate_limit_error", msg, "plan-a", nil)
	if fc.Summary != "upstream SSE error (rate_limit)" {
		t.Fatalf("summary = %q, want fixed classification", fc.Summary)
	}
	assertSafeSummary(t, fc.Summary)
}
