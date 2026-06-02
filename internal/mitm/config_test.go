package mitm

import (
	"testing"
)

func TestNewDomainMatcher(t *testing.T) {
	dm := NewDomainMatcher([]string{"example.com"}, []string{"evil.com"})
	if len(dm.AllowPatterns()) != 1 {
		t.Fatal("expected 1 allow pattern")
	}
	if len(dm.DenyPatterns()) != 1 {
		t.Fatal("expected 1 deny pattern")
	}
}

func TestNewDomainMatcher_NilSlices(t *testing.T) {
	dm := NewDomainMatcher(nil, nil)
	if dm.AllowPatterns() != nil {
		t.Fatal("expected nil allow patterns")
	}
	if dm.DenyPatterns() != nil {
		t.Fatal("expected nil deny patterns")
	}
}

// --- Exact match ---

func TestIsAllowed_ExactMatchAllow(t *testing.T) {
	dm := NewDomainMatcher([]string{"example.com"}, nil)
	if !dm.IsAllowed("example.com") {
		t.Error("expected allowed")
	}
}

func TestIsAllowed_ExactMatchAllow_PortStripped(t *testing.T) {
	dm := NewDomainMatcher([]string{"example.com"}, nil)
	if !dm.IsAllowed("example.com:443") {
		t.Error("expected allowed with port stripped")
	}
}

func TestIsAllowed_NoMatchAllow(t *testing.T) {
	dm := NewDomainMatcher([]string{"example.com"}, nil)
	if dm.IsAllowed("other.com") {
		t.Error("expected not allowed")
	}
}

// --- Empty allowlist ---

func TestIsAllowed_EmptyAllowlist(t *testing.T) {
	dm := NewDomainMatcher(nil, nil)
	if !dm.IsAllowed("anything.example") {
		t.Error("empty allowlist should allow all")
	}
}

// --- Denylist ---

func TestIsAllowed_Denied(t *testing.T) {
	dm := NewDomainMatcher(nil, []string{"evil.com"})
	if dm.IsAllowed("evil.com") {
		t.Error("denied host should not be allowed")
	}
}

func TestIsAllowed_DenyTakesPrecedenceOverAllow(t *testing.T) {
	dm := NewDomainMatcher([]string{"*"}, []string{"evil.com"})
	if dm.IsAllowed("evil.com") {
		t.Error("denylist should take precedence over allowlist")
	}
}

func TestIsAllowed_DeniedWithPort(t *testing.T) {
	dm := NewDomainMatcher(nil, []string{"evil.com"})
	if dm.IsAllowed("evil.com:443") {
		t.Error("denied host with port should not be allowed")
	}
}

// --- Wildcard patterns ---

func TestMatchDomain_WildcardSubdomain(t *testing.T) {
	dm := NewDomainMatcher([]string{"*.example.com"}, nil)
	if !dm.IsAllowed("a.example.com") {
		t.Error("*.example.com should match a.example.com")
	}
	if !dm.IsAllowed("x.example.com") {
		t.Error("*.example.com should match x.example.com")
	}
}

func TestMatchDomain_WildcardDoesNotCrossSegments(t *testing.T) {
	dm := NewDomainMatcher([]string{"*.example.com"}, nil)
	if dm.IsAllowed("a.b.example.com") {
		t.Error("*.example.com should NOT match a.b.example.com")
	}
}

func TestMatchDomain_BareWildcard(t *testing.T) {
	dm := NewDomainMatcher([]string{"*"}, nil)
	if !dm.IsAllowed("anything.anywhere.com") {
		t.Error("bare * should match all domains")
	}
	if !dm.IsAllowed("localhost") {
		t.Error("bare * should match localhost")
	}
}

func TestMatchDomain_WildcardAtStart(t *testing.T) {
	dm := NewDomainMatcher([]string{"*.com"}, nil)
	if !dm.IsAllowed("example.com") {
		t.Error("*.com should match example.com")
	}
}

func TestMatchDomain_WildcardInMiddle(t *testing.T) {
	dm := NewDomainMatcher([]string{"api.*.com"}, nil)
	if !dm.IsAllowed("api.example.com") {
		t.Error("api.*.com should match api.example.com")
	}
}

func TestMatchDomain_WildcardAtEnd(t *testing.T) {
	dm := NewDomainMatcher([]string{"api.example.*"}, nil)
	if !dm.IsAllowed("api.example.com") {
		t.Error("api.example.* should match api.example.com")
	}
}

// --- Case insensitivity ---

func TestMatchDomain_CaseInsensitivePattern(t *testing.T) {
	dm := NewDomainMatcher([]string{"*.EXAMPLE.COM"}, nil)
	if !dm.IsAllowed("a.example.com") {
		t.Error("matching should be case-insensitive")
	}
}

func TestMatchDomain_CaseInsensitiveHost(t *testing.T) {
	dm := NewDomainMatcher([]string{"*.example.com"}, nil)
	if !dm.IsAllowed("A.EXAMPLE.COM") {
		t.Error("matching should be case-insensitive")
	}
}

// --- Mutability ---

func TestSetAllow(t *testing.T) {
	dm := NewDomainMatcher([]string{"old.com"}, nil)
	dm.SetAllow([]string{"new.com"})
	if dm.IsAllowed("old.com") {
		t.Error("old pattern should be removed after SetAllow")
	}
	if !dm.IsAllowed("new.com") {
		t.Error("new pattern should match after SetAllow")
	}
}

func TestSetDeny(t *testing.T) {
	dm := NewDomainMatcher(nil, []string{"old.com"})
	dm.SetDeny([]string{"new.com"})
	if !dm.IsAllowed("old.com") {
		t.Error("old deny pattern should have been removed after SetDeny")
	}
	if dm.IsAllowed("new.com") {
		t.Error("new deny pattern should be active after SetDeny")
	}
}

func TestAddAllow(t *testing.T) {
	dm := NewDomainMatcher(nil, nil)
	dm.AddAllow("example.com")
	if !dm.IsAllowed("example.com") {
		t.Error("AddAllow should add a matching pattern")
	}
}

func TestAddDeny(t *testing.T) {
	dm := NewDomainMatcher(nil, nil)
	dm.AddDeny("evil.com")
	if dm.IsAllowed("evil.com") {
		t.Error("AddDeny should add a denying pattern")
	}
}

func TestRemoveAllow(t *testing.T) {
	dm := NewDomainMatcher([]string{"example.com", "other.com"}, nil)
	dm.RemoveAllow("example.com")
	if dm.IsAllowed("example.com") {
		t.Error("RemoveAllow should remove pattern")
	}
	if !dm.IsAllowed("other.com") {
		t.Error("RemoveAllow should keep other patterns")
	}
}

func TestRemoveDeny(t *testing.T) {
	dm := NewDomainMatcher(nil, []string{"evil.com", "bad.com"})
	dm.RemoveDeny("evil.com")
	if !dm.IsAllowed("evil.com") {
		t.Error("RemoveDeny should have removed evil.com from denylist")
	}
	if dm.IsAllowed("bad.com") {
		t.Error("bad.com should still be denied after RemoveDeny")
	}
}

func TestRemoveAllow_NonExistent(t *testing.T) {
	dm := NewDomainMatcher([]string{"example.com"}, nil)
	dm.RemoveAllow("nonexistent.com")
	// Should not panic and should not affect existing patterns
	if !dm.IsAllowed("example.com") {
		t.Error("removing non-existent pattern should not affect existing ones")
	}
}

// --- Pattern list isolation ---

func TestAllowPatterns_ReturnsCopy(t *testing.T) {
	dm := NewDomainMatcher([]string{"a.com", "b.com"}, nil)
	patterns := dm.AllowPatterns()
	patterns[0] = "modified"
	// Verify the original is unmodified
	original := dm.AllowPatterns()
	if original[0] == "modified" {
		t.Error("AllowPatterns should return a copy")
	}
}

func TestDenyPatterns_ReturnsCopy(t *testing.T) {
	dm := NewDomainMatcher(nil, []string{"x.com"})
	patterns := dm.DenyPatterns()
	patterns[0] = "modified"
	original := dm.DenyPatterns()
	if original[0] == "modified" {
		t.Error("DenyPatterns should return a copy")
	}
}

// --- Edge cases ---

func TestMatchDomain_NoWildcard(t *testing.T) {
	if !matchDomain("example.com", "example.com") {
		t.Error("exact match should succeed")
	}
}

func TestMatchDomain_EmptyHost(t *testing.T) {
	if matchDomain("", "example.com") {
		t.Error("empty host should not match")
	}
}

func TestMatchDomain_EmptyPattern(t *testing.T) {
	if matchDomain("example.com", "") {
		t.Error("empty pattern should not match non-empty host")
	}
}

func TestMatchDomain_LengthMismatch(t *testing.T) {
	if matchDomain("a.b.example.com", "*.example.com") {
		t.Error("different segment counts should not match")
	}
}

func TestStripPort_NoPort(t *testing.T) {
	result := stripPort("example.com")
	if result != "example.com" {
		t.Errorf("expected 'example.com', got '%s'", result)
	}
}

func TestStripPort_WithPort(t *testing.T) {
	result := stripPort("example.com:443")
	if result != "example.com" {
		t.Errorf("expected 'example.com', got '%s'", result)
	}
}

func TestStripPort_IPv6(t *testing.T) {
	result := stripPort("[::1]:8080")
	if result != "::1" {
		t.Errorf("expected '::1', got '%s'", result)
	}
}

func TestStripPort_IPv6NoPort(t *testing.T) {
	result := stripPort("::1")
	if result != "::1" {
		t.Errorf("expected '::1', got '%s'", result)
	}
}

// --- Concurrency (safety smoke test) ---

func TestDomainMatcher_ConcurrencySafe(t *testing.T) {
	dm := NewDomainMatcher([]string{"*.example.com"}, []string{"evil.com"})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			dm.IsAllowed("test.example.com")
			dm.AllowPatterns()
			dm.DenyPatterns()
		}
		done <- struct{}{}
	}()

	go func() {
		for i := 0; i < 100; i++ {
			dm.AddAllow("extra.com")
			dm.AddDeny("bad.net")
			dm.RemoveAllow("extra.com")
			dm.RemoveDeny("bad.net")
		}
		done <- struct{}{}
	}()

	<-done
	<-done
}

// --- Complex scenarios ---

func TestIsAllowed_MultipleAllowPatterns(t *testing.T) {
	dm := NewDomainMatcher([]string{"api.example.com", "app.example.com"}, nil)
	if !dm.IsAllowed("api.example.com") {
		t.Error("api.example.com should match")
	}
	if !dm.IsAllowed("app.example.com") {
		t.Error("app.example.com should match")
	}
	if dm.IsAllowed("other.example.com") {
		t.Error("other.example.com should not match")
	}
}

func TestIsAllowed_MultipleDenyPatterns(t *testing.T) {
	dm := NewDomainMatcher([]string{"*"}, []string{"evil.com", "bad.com"})
	if dm.IsAllowed("evil.com") {
		t.Error("evil.com should be denied")
	}
	if dm.IsAllowed("bad.com") {
		t.Error("bad.com should be denied")
	}
	if !dm.IsAllowed("good.com") {
		t.Error("good.com should be allowed")
	}
}

func TestIsAllowed_AllowBareWildcardDenySpecific(t *testing.T) {
	dm := NewDomainMatcher([]string{"*"}, []string{"deny.example.com"})
	if dm.IsAllowed("deny.example.com") {
		t.Error("deny.example.com should be blocked")
	}
	if !dm.IsAllowed("other.example.com") {
		t.Error("other.example.com should be allowed")
	}
}
