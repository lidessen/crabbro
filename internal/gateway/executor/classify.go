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
	Kind            string
	Retryable       bool
	ContextOnly     bool
	CooldownScope   string // "" | "plan" | "model"
	CooldownDur     time.Duration
	CooldownLabel   string
	Summary         string
}

// ClassifyHTTPFailure implements the failure & cooldown matrix from docs/design/gateway.md §7.3.
func ClassifyHTTPFailure(status int, body []byte, h http.Header, planID string, health *router.HealthStore) FailureClass {
	lower := strings.ToLower(string(body))

	switch status {
	case http.StatusTooManyRequests:
		dur := parseRetryAfter(h)
		if dur <= 0 && health != nil {
			dur = health.NextRateLimitBackoff(planID)
		}
		if dur <= 0 {
			dur = 15 * time.Second
		}
		if strings.Contains(lower, "insufficient_quota") || strings.Contains(lower, "quota exceeded") || strings.Contains(lower, "credit balance") {
			return FailureClass{
				Kind:          ErrKindQuotaExhausted,
				Retryable:     true,
				CooldownScope: "plan",
				CooldownDur:   maxDuration(dur, 5*time.Minute),
				CooldownLabel: "plan:" + maxDuration(dur, 5*time.Minute).String(),
				Summary:       "quota exhausted (429)",
			}
		}
		return FailureClass{
			Kind:          ErrKindRateLimit,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   dur,
			CooldownLabel: "plan:" + dur.String(),
			Summary:       "rate limited (429)",
		}
	case http.StatusPaymentRequired:
		return FailureClass{
			Kind:          ErrKindQuotaExhausted,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   5 * time.Minute,
			CooldownLabel: "plan:5m",
			Summary:       "payment or quota required (402)",
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		return FailureClass{
			Kind:          ErrKindAuthError,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   10 * time.Minute,
			CooldownLabel: "plan:10m",
			Summary:       "authentication/authorization rejected",
		}
	case http.StatusNotFound:
		return FailureClass{
			Kind:          ErrKindModelUnavailable,
			Retryable:     true,
			CooldownScope: "model",
			CooldownDur:   10 * time.Minute,
			CooldownLabel: "model:10m",
			Summary:       "model or endpoint not found (404)",
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if isContextOverflow(lower) {
			return FailureClass{
				Kind:        ErrKindContextOverflow,
				Retryable:   true,
				ContextOnly: true,
				Summary:     "context length exceeded",
			}
		}
		if strings.Contains(lower, "model_not_found") || strings.Contains(lower, "does not exist") ||
			(strings.Contains(lower, "response_format") && (strings.Contains(lower, "unavailable") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "not support"))) {
			return FailureClass{
				Kind:          ErrKindModelUnavailable,
				Retryable:     true,
				CooldownScope: "model",
				CooldownDur:   10 * time.Minute,
				CooldownLabel: "model:10m",
				Summary:       "model capability or endpoint unavailable",
			}
		}
		if strings.Contains(lower, "credit balance is too low") || strings.Contains(lower, "insufficient_quota") {
			return FailureClass{
				Kind:          ErrKindQuotaExhausted,
				Retryable:     true,
				CooldownScope: "plan",
				CooldownDur:   5 * time.Minute,
				CooldownLabel: "plan:5m",
				Summary:       "quota exhausted",
			}
		}
		return FailureClass{
			Kind:      ErrKindClientBadRequest,
			Retryable: false,
			Summary:   "client bad request",
		}
	default:
		if status >= 500 || status == 0 {
			return FailureClass{
				Kind:          ErrKindUpstreamUnavailable,
				Retryable:     true,
				CooldownScope: "plan",
				CooldownDur:   15 * time.Second,
				CooldownLabel: "plan:15s",
				Summary:       "upstream unavailable",
			}
		}
		return FailureClass{
			Kind:      ErrKindClientBadRequest,
			Retryable: false,
			Summary:   "upstream rejected request",
		}
	}
}

// ClassifySSEFailure maps a fake-200 SSE error code/message onto FailureClass.
func ClassifySSEFailure(code, msg, planID string, health *router.HealthStore) FailureClass {
	combined := strings.ToLower(code + " " + msg)
	switch {
	case strings.Contains(combined, "rate_limit") || strings.Contains(combined, "too many requests") || strings.Contains(combined, "429"):
		dur := 15 * time.Second
		if health != nil {
			dur = health.NextRateLimitBackoff(planID)
		}
		return FailureClass{
			Kind:          ErrKindRateLimit,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   dur,
			CooldownLabel: "plan:" + dur.String(),
			Summary:       msg,
		}
	case strings.Contains(combined, "quota") || strings.Contains(combined, "credit") || strings.Contains(combined, "balance"):
		return FailureClass{
			Kind:          ErrKindQuotaExhausted,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   5 * time.Minute,
			CooldownLabel: "plan:5m",
			Summary:       msg,
		}
	case strings.Contains(combined, "auth") || strings.Contains(combined, "invalid_api_key") || strings.Contains(combined, "permission"):
		return FailureClass{
			Kind:          ErrKindAuthError,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   10 * time.Minute,
			CooldownLabel: "plan:10m",
			Summary:       msg,
		}
	case isContextOverflow(combined):
		return FailureClass{
			Kind:        ErrKindContextOverflow,
			Retryable:   true,
			ContextOnly: true,
			Summary:     msg,
		}
	case strings.Contains(combined, "model_not_found"):
		return FailureClass{
			Kind:          ErrKindModelUnavailable,
			Retryable:     true,
			CooldownScope: "model",
			CooldownDur:   10 * time.Minute,
			CooldownLabel: "model:10m",
			Summary:       msg,
		}
	default:
		return FailureClass{
			Kind:          ErrKindUpstreamUnavailable,
			Retryable:     true,
			CooldownScope: "plan",
			CooldownDur:   15 * time.Second,
			CooldownLabel: "plan:15s",
			Summary:       msg,
		}
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
