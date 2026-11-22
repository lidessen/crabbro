package router

import (
	"strings"
	"testing"
	"time"
	"unicode"
)

const (
	testReasonAPIKey = "sk-abc123456789012345"
	testReasonBearer = "Bearer secret-token-value"
	testReasonEmail  = "user@example.com"
)

// assertNoLeakedText enforces the strict allowlist boundary: no fragment of
// unrecognized input, secret-like token, HTML, URL, or control character may
// survive sanitization.
func assertNoLeakedText(t *testing.T, reason string) {
	t.Helper()
	for _, banned := range []string{
		testReasonAPIKey, "secret-token-value", testReasonEmail,
		"private response", "credential=example-secret", "xsk-example",
		"leaked-token", "secondary-leak", "quoted-leak", "json-leak",
		"<html", "<body", "?api_key=", "https://", "\n", "\r", "\t",
	} {
		if strings.Contains(reason, banned) {
			t.Errorf("reason %q leaked %q", reason, banned)
		}
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			t.Errorf("reason %q contains control character %U", reason, r)
		}
	}
}

// unsafeReasonSamples are shaped like upstream bodies and query strings: HTML,
// a private response dump, credential parameters, odd key prefixes, a fake API
// key, a Bearer token, an email, query tokens, newlines, and a large payload.
func unsafeReasonSamples() []string {
	return []string{
		"<body>private response</body>",
		"?credential=example-secret",
		"xsk-example",
		testReasonAPIKey,
		testReasonBearer,
		testReasonEmail,
		unsafeReason(),
	}
}

func unsafeReason() string {
	return "<html><body>private response</body> " + testReasonAPIKey + " " + testReasonBearer + " " + testReasonEmail +
		" ?credential=example-secret xsk-example" +
		"\nhttps://upstream.example/v1/chat?api_key=leaked-token&key=secondary-leak" +
		` api_key="quoted-leak" {"api_key":"json-leak"}` +
		" " + strings.Repeat("<div>response dump</div>", 64) + "</html>"
}

// The allowlist is closed: every fixed classifier summary passes unchanged,
// including the parameterized status-code formats.
func TestSanitizeReasonAllowsOnlyClassifiedSummaries(t *testing.T) {
	allowed := []string{
		"rate limited (429)",
		"quota exhausted",
		"quota exhausted (429)",
		"payment or quota required (402)",
		"upstream auth error (401)",
		"upstream permission denied (403)",
		"model or endpoint not found",
		"model or endpoint not found (404)",
		"model capability or endpoint unavailable",
		"context length exceeded",
		"upstream timeout",
		"transport error",
		"mid-stream drop",
		"upstream SSE error",
		"upstream SSE error (rate_limit)",
		"upstream SSE error (quota_exhausted)",
		"upstream SSE error (auth_error)",
		"429 rate limited",
		"insufficient balance",
		"upstream invalid request (400)",
		"upstream invalid request (422)",
		"upstream rejected request (418)",
		"upstream server error (502)",
		"upstream server error (503)",
	}
	for _, reason := range allowed {
		if got := SanitizeReason(reason); got != reason {
			t.Errorf("SanitizeReason(%q) = %q, want the classified summary unchanged", reason, got)
		}
	}
}

// Unrecognized text may never pass, and no part of it may be echoed: the only
// output is the fixed unclassified marker.
func TestSanitizeReasonRejectsUnrecognizedText(t *testing.T) {
	cases := append(unsafeReasonSamples(),
		"rate limited (429) please retry",
		"prefix rate limited (429)",
		"upstream invalid request (40)",
		"upstream invalid request (4000)",
		"upstream server error (50x)",
	)
	for _, reason := range cases {
		got := SanitizeReason(reason)
		if got != unclassifiedReason {
			t.Errorf("SanitizeReason(%q) = %q, want %q", reason, got, unclassifiedReason)
		}
		assertNoLeakedText(t, got)
	}
}

// Normalization collapses whitespace and control characters before the exact
// allowlist check; empty input stays empty.
func TestSanitizeReasonNormalizesBeforeMatching(t *testing.T) {
	if got := SanitizeReason("\n rate  limited\t(429) "); got != "rate limited (429)" {
		t.Errorf("normalized classified reason = %q, want %q", got, "rate limited (429)")
	}
	for _, reason := range []string{"", " \n\t\x00 "} {
		if got := SanitizeReason(reason); got != "" {
			t.Errorf("SanitizeReason(%q) = %q, want empty", reason, got)
		}
	}
}

// The store itself must not retain raw free text even when an internal caller
// bypasses the classifier.
func TestHealthStoreCooldownsStoreClassifiedOrOmittedReasons(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	h := NewHealthStore()

	var callbackReason string
	h.OnChange = func(planID, state, reason string) {
		callbackReason = reason
	}
	h.EnterPlanCooldown("plan-a", "plan", unsafeReason(), time.Minute, now)
	if callbackReason != unclassifiedReason {
		t.Errorf("callback reason = %q, want %q", callbackReason, unclassifiedReason)
	}
	assertNoLeakedText(t, callbackReason)

	h.mu.Lock()
	storedPlan := h.plans["plan-a"].lastError
	h.mu.Unlock()
	if storedPlan != unclassifiedReason {
		t.Errorf("stored plan reason = %q, want %q", storedPlan, unclassifiedReason)
	}
	assertNoLeakedText(t, storedPlan)
	if got := h.Snapshot("plan-a", now).LastError; got != unclassifiedReason {
		t.Errorf("plan snapshot reason = %q, want %q", got, unclassifiedReason)
	}

	h.EnterModelCooldown("plan-b", "model-x", unsafeReason(), time.Minute, now)
	h.mu.Lock()
	storedModel := h.plans["plan-b"].lastError
	h.mu.Unlock()
	if storedModel != unclassifiedReason {
		t.Errorf("stored model reason = %q, want %q", storedModel, unclassifiedReason)
	}
	if got := h.Snapshot("plan-b", now).LastError; got != unclassifiedReason {
		t.Errorf("model snapshot reason = %q, want %q", got, unclassifiedReason)
	}

	// A classified summary is what the classifier emits, so it survives.
	h.EnterPlanCooldown("plan-c", "plan", "quota exhausted", time.Minute, now)
	if got := h.Snapshot("plan-c", now).LastError; got != "quota exhausted" {
		t.Errorf("classified snapshot reason = %q, want %q", got, "quota exhausted")
	}
}

// Snapshot is the API boundary: even state written directly must come out as
// either an allowlisted summary or the fixed unclassified marker.
func TestSnapshotWithCredentialsCooling(t *testing.T) {
	h := NewHealthStore()
	now := time.Now()
	h.EnterKeyCooldown("p", "a", time.Minute, now)
	if snap := h.SnapshotWithCredentials("p", []string{"a"}, now); snap.Healthy || snap.CooldownUntil == nil || !snap.CooldownUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("single key snapshot = %+v", snap)
	}
	if snap := h.SnapshotWithCredentials("p", []string{"a", "b"}, now); !snap.Healthy {
		t.Fatalf("available key snapshot = %+v", snap)
	}
	h.EnterKeyCooldown("p", "b", 2*time.Minute, now)
	if snap := h.SnapshotWithCredentials("p", []string{"a", "b"}, now); snap.Healthy || snap.CooldownUntil == nil || !snap.CooldownUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("all keys cooling snapshot = %+v", snap)
	}
	if snap := h.SnapshotWithCredentials("p", []string{"a", "b"}, now.Add(time.Minute)); !snap.Healthy {
		t.Fatalf("expired cooldown snapshot = %+v", snap)
	}
}

func TestHealthStoreSnapshotOmitsUnrecognizedDirectState(t *testing.T) {
	for _, reason := range unsafeReasonSamples() {
		h := NewHealthStore()
		h.mu.Lock()
		h.ensureLocked("plan-direct").lastError = reason
		h.mu.Unlock()

		got := h.Snapshot("plan-direct", time.Now().UTC()).LastError
		if got != unclassifiedReason {
			t.Errorf("snapshot reason for %q = %q, want %q", reason, got, unclassifiedReason)
		}
		assertNoLeakedText(t, got)
	}
}
