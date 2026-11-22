package executor

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/router"
)

const (
	ErrKindRateLimit           = "rate_limit"
	ErrKindQuotaExhausted      = "quota_exhausted"
	ErrKindAuthError           = "auth_error"
	ErrKindModelUnavailable    = "model_unavailable"
	ErrKindContextOverflow     = "context_overflow"
	ErrKindClientBadRequest    = "client_bad_request"
	ErrKindUpstreamUnavailable = "upstream_unavailable"
)

// FailureClass describes how the executor should react to a failed attempt.
type FailureClass struct {
	Kind          string
	Retryable     bool
	ContextOnly   bool
	CooldownScope string // "" | "plan" | "model"
	CooldownDur   time.Duration
	CooldownLabel string
	Summary       string
}

// classifiedFailure builds a FailureClass whose Summary is a bounded,
// client-safe classification. Upstream free text never enters Summary: the
// classifier may inspect bodies and messages to choose a class, but only
// deterministic summaries are stored in health state and shown in the UI.
func classifiedFailure(kind string, retryable, contextOnly bool, scope string, dur time.Duration, label, summary string) FailureClass {
	return FailureClass{
		Kind:          kind,
		Retryable:     retryable,
		ContextOnly:   contextOnly,
		CooldownScope: scope,
		CooldownDur:   dur,
		CooldownLabel: label,
		Summary:       router.SanitizeReason(summary),
	}
}

// ClassifyHTTPFailure implements the failure & cooldown matrix from docs/design/gateway.md §7.3.
func ClassifyHTTPFailure(status int, body []byte, h http.Header, planID string, health *router.HealthStore, refs ...string) FailureClass {
	// The body is inspected only to choose a class. No slice of it is ever
	// copied into FailureClass.Summary.
	lower := strings.ToLower(string(body))

	switch status {
	case http.StatusTooManyRequests:
		dur := parseRetryAfter(h)
		if dur <= 0 && health != nil {
			if len(refs) > 0 {
				dur = health.NextKeyBackoff(planID, refs[0])
			} else {
				dur = health.NextRateLimitBackoff(planID)
			}
		}
		if dur <= 0 {
			dur = 15 * time.Second
		}
		if strings.Contains(lower, "insufficient_quota") || strings.Contains(lower, "quota exceeded") || strings.Contains(lower, "credit balance") {
			dur = maxDuration(dur, 5*time.Minute)
			return classifiedFailure(ErrKindQuotaExhausted, true, false, "key", dur, "key:"+dur.String(), "quota exhausted (429)")
		}
		return classifiedFailure(ErrKindRateLimit, true, false, "key", dur, "key:"+dur.String(), "rate limited (429)")
	case http.StatusPaymentRequired:
		return classifiedFailure(ErrKindQuotaExhausted, true, false, "key", 5*time.Minute, "key:5m", "payment or quota required (402)")
	case http.StatusUnauthorized:
		return classifiedFailure(ErrKindAuthError, true, false, "key", 10*time.Minute, "key:10m", "upstream auth error (401)")
	case http.StatusForbidden:
		return classifiedFailure(ErrKindAuthError, true, false, "key", 10*time.Minute, "key:10m", "upstream permission denied (403)")
	case http.StatusNotFound:
		return classifiedFailure(ErrKindModelUnavailable, true, false, "model", 10*time.Minute, "model:10m", "model or endpoint not found (404)")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if isContextOverflow(lower) {
			return classifiedFailure(ErrKindContextOverflow, true, true, "", 0, "", "context length exceeded")
		}
		if strings.Contains(lower, "model_not_found") || strings.Contains(lower, "does not exist") ||
			(strings.Contains(lower, "response_format") && (strings.Contains(lower, "unavailable") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "not support"))) {
			return classifiedFailure(ErrKindModelUnavailable, true, false, "model", 10*time.Minute, "model:10m", "model capability or endpoint unavailable")
		}
		if strings.Contains(lower, "credit balance is too low") || strings.Contains(lower, "insufficient_quota") {
			return classifiedFailure(ErrKindQuotaExhausted, true, false, "key", 5*time.Minute, "key:5m", "quota exhausted")
		}
		return classifiedFailure(ErrKindClientBadRequest, false, false, "", 0, "", "upstream invalid request ("+strconv.Itoa(status)+")")
	default:
		if status >= 500 || status == 0 {
			summary := "transport error"
			if status >= 500 {
				summary = "upstream server error (" + strconv.Itoa(status) + ")"
			} else if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") {
				summary = "upstream timeout"
			}
			return classifiedFailure(ErrKindUpstreamUnavailable, true, false, "plan", 15*time.Second, "plan:15s", summary)
		}
		return classifiedFailure(ErrKindClientBadRequest, false, false, "", 0, "", "upstream rejected request ("+strconv.Itoa(status)+")")
	}
}

// ClassifySSEFailure maps a fake-200 SSE error code/message onto FailureClass.
// The code and message are inspected only to choose a class. Upstream free
// text is never copied into FailureClass.Summary: a malicious or buggy
// upstream can put API keys, prompts, or response dumps in either field.
func ClassifySSEFailure(code, msg, planID string, health *router.HealthStore, refs ...string) FailureClass {
	combined := strings.ToLower(code + " " + msg)
	switch {
	case strings.Contains(combined, "rate_limit") || strings.Contains(combined, "too many requests") || strings.Contains(combined, "429"):
		dur := 15 * time.Second
		if health != nil {
			if len(refs) > 0 {
				dur = health.NextKeyBackoff(planID, refs[0])
			} else {
				dur = health.NextRateLimitBackoff(planID)
			}
		}
		return classifiedFailure(ErrKindRateLimit, true, false, "key", dur, "key:"+dur.String(), "upstream SSE error (rate_limit)")
	case strings.Contains(combined, "quota") || strings.Contains(combined, "credit") || strings.Contains(combined, "balance"):
		return classifiedFailure(ErrKindQuotaExhausted, true, false, "key", 5*time.Minute, "key:5m", "upstream SSE error (quota_exhausted)")
	case strings.Contains(combined, "auth") || strings.Contains(combined, "invalid_api_key") || strings.Contains(combined, "permission"):
		return classifiedFailure(ErrKindAuthError, true, false, "key", 10*time.Minute, "key:10m", "upstream SSE error (auth_error)")
	case isContextOverflow(combined):
		return classifiedFailure(ErrKindContextOverflow, true, true, "", 0, "", "context length exceeded")
	case strings.Contains(combined, "model_not_found"):
		return classifiedFailure(ErrKindModelUnavailable, true, false, "model", 10*time.Minute, "model:10m", "model or endpoint not found")
	default:
		return classifiedFailure(ErrKindUpstreamUnavailable, true, false, "plan", 15*time.Second, "plan:15s", "upstream SSE error")
	}
}

func isContextOverflow(lower string) bool {
	return strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "maximum context length") ||
		strings.Contains(lower, "prompt is too long") ||
		strings.Contains(lower, "too many tokens")
}

func parseRetryAfter(h http.Header) time.Duration {
	if len(h) == 0 {
		return 0
	}
	for _, name := range []string{"Retry-After", "retry-after-ms"} {
		raw := strings.TrimSpace(h.Get(name))
		if raw == "" {
			continue
		}
		if name == "retry-after-ms" {
			if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
				return time.Duration(ms) * time.Millisecond
			}
		}
		if sec, err := strconv.ParseInt(raw, 10, 64); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
		if t, err := http.ParseTime(raw); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
