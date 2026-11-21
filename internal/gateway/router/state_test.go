package router

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	testReasonAPIKey = "sk-abc123456789012345"
	testReasonBearer = "Bearer secret-token-value"
	testReasonEmail  = "user@example.com"
)

func assertSafeStoredReason(t *testing.T, reason string) {
	t.Helper()
	if n := utf8.RuneCountInString(reason); n > maxReasonRunes {
		t.Errorf("reason %q is %d runes, want <= %d", reason, n, maxReasonRunes)
	}
	for _, banned := range []string{testReasonAPIKey, "secret-token-value", testReasonEmail, "leaked-token", "secondary-leak", "quoted-leak", "json-leak", "\n", "\r", "\t"} {
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

// unsafeReason is shaped like an upstream body: an HTML dump with a fake API
// key, Bearer token, email, query token, newlines, and a large payload.
func unsafeReason() string {
	return "<html><body>" + testReasonAPIKey + " " + testReasonBearer + " " + testReasonEmail +
		"\nhttps://upstream.example/v1/chat?api_key=leaked-token&key=secondary-leak" +
		` api_key="quoted-leak" {"api_key":"json-leak"}` +
		"</body></html> " + strings.Repeat("<div>response dump</div>", 64)
}

func TestSanitizeReasonCollapsesControlsAndRedactsSecrets(t *testing.T) {
	got := SanitizeReason(unsafeReason())
	assertSafeStoredReason(t, got)
	if !strings.Contains(got, redactedReasonToken) {
		t.Errorf("reason %q should show a redaction marker", got)
	}
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("reason %q should be a single line", got)
	}
	if twice := SanitizeReason(got); twice != got {
		t.Errorf("SanitizeReason is not idempotent: %q -> %q", got, twice)
	}
}

func TestSanitizeReasonCapsLongValues(t *testing.T) {
	got := SanitizeReason(strings.Repeat("upstream body dump ", 100))
	if n := utf8.RuneCountInString(got); n > maxReasonRunes {
		t.Fatalf("reason has %d runes, want <= %d: %q", n, maxReasonRunes, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated reason %q should end with an ellipsis", got)
	}
}

func TestSanitizeReasonPreservesPlainReasons(t *testing.T) {
	for _, reason := range []string{"rate limited (429)", "quota exhausted", "upstream server error (502)"} {
		if got := SanitizeReason(reason); got != reason {
			t.Errorf("SanitizeReason(%q) = %q, want unchanged", reason, got)
		}
	}
	if got := SanitizeReason(" \n\t\x00 "); got != "" {
		t.Errorf("whitespace-only reason = %q, want empty", got)
	}
}

// The store itself must not retain raw free text even when an internal caller
// bypasses the classifier.
func TestHealthStoreCooldownsStoreSanitizedReasons(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	h := NewHealthStore()

	var callbackReason string
	h.OnChange = func(planID, state, reason string) {
		callbackReason = reason
	}
	h.EnterPlanCooldown("plan-a", "plan", unsafeReason(), time.Minute, now)
	assertSafeStoredReason(t, callbackReason)

	h.mu.Lock()
	storedPlan := h.plans["plan-a"].lastError
	h.mu.Unlock()
	assertSafeStoredReason(t, storedPlan)
	assertSafeStoredReason(t, h.Snapshot("plan-a", now).LastError)

	h.EnterModelCooldown("plan-b", "model-x", unsafeReason(), time.Minute, now)
	h.mu.Lock()
	storedModel := h.plans["plan-b"].lastError
	h.mu.Unlock()
	assertSafeStoredReason(t, storedModel)
	assertSafeStoredReason(t, h.Snapshot("plan-b", now).LastError)
}

// Snapshot is the API boundary: even state written directly must come out
// sanitized.
func TestHealthStoreSnapshotSanitizesDirectState(t *testing.T) {
	h := NewHealthStore()
	h.mu.Lock()
	h.ensureLocked("plan-direct").lastError = unsafeReason()
	h.mu.Unlock()

	got := h.Snapshot("plan-direct", time.Now().UTC()).LastError
	assertSafeStoredReason(t, got)
	if got == "" {
		t.Fatal("expected a bounded reason, got empty")
	}
}
