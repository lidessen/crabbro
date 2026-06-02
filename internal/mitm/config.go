// Package mitm provides HTTPS MITM interception, including certificate
// generation, TLS interception, and domain allowlist/denylist matching.
package mitm

import (
	"net"
	"strings"
	"sync"
)

// DomainMatcher manages a concurrent-safe allowlist and denylist of domain
// patterns for MITM interception.
//
// Each pattern is a domain string. `*` matches any single domain segment
// (any sequence of characters except '.'). So `*.example.com` matches
// `a.example.com` but not `a.b.example.com`. A bare `*` matches all domains.
//
// Matching is case-insensitive and operates on the host part only (port is
// stripped during matching).
//
// Denylist takes precedence: if a host matches the denylist, MITM is skipped
// regardless of the allowlist.
//
// An empty allowlist (nil or zero-length) means MITM is permitted for all
// domains, subject to the denylist. A non-empty allowlist restricts MITM to
// only matching domains.
type DomainMatcher struct {
	mu     sync.RWMutex
	allow  []string
	deny   []string
}

// NewDomainMatcher creates a DomainMatcher with the given initial allow and
// deny patterns. Nil slices are treated as empty.
func NewDomainMatcher(allow, deny []string) *DomainMatcher {
	return &DomainMatcher{
		allow: copySlice(allow),
		deny:  copySlice(deny),
	}
}

// IsAllowed checks whether the given host is allowed for MITM interception.
// The host may include a port (e.g. "example.com:443"); it is stripped before
// matching. Returns true if MITM is permitted.
func (dm *DomainMatcher) IsAllowed(host string) bool {
	host = stripPort(host)

	dm.mu.RLock()
	defer dm.mu.RUnlock()

	// Denylist takes precedence.
	for _, p := range dm.deny {
		if matchDomain(host, p) {
			return false
		}
	}

	// Empty allowlist = all domains allowed (subject to denylist).
	if len(dm.allow) == 0 {
		return true
	}

	for _, p := range dm.allow {
		if matchDomain(host, p) {
			return true
		}
	}
	return false
}

// SetAllow replaces the entire allowlist with the given patterns.
func (dm *DomainMatcher) SetAllow(patterns []string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.allow = copySlice(patterns)
}

// SetDeny replaces the entire denylist with the given patterns.
func (dm *DomainMatcher) SetDeny(patterns []string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.deny = copySlice(patterns)
}

// AddAllow appends a single pattern to the allowlist.
func (dm *DomainMatcher) AddAllow(pattern string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.allow = append(dm.allow, pattern)
}

// AddDeny appends a single pattern to the denylist.
func (dm *DomainMatcher) AddDeny(pattern string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.deny = append(dm.deny, pattern)
}

// RemoveAllow removes a pattern from the allowlist by value comparison
// (case-sensitive exact match). If the pattern does not exist the list is
// unchanged.
func (dm *DomainMatcher) RemoveAllow(pattern string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.allow = removePattern(dm.allow, pattern)
}

// RemoveDeny removes a pattern from the denylist by value comparison
// (case-sensitive exact match). If the pattern does not exist the list is
// unchanged.
func (dm *DomainMatcher) RemoveDeny(pattern string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.deny = removePattern(dm.deny, pattern)
}

// AllowPatterns returns a copy of the current allowlist.
func (dm *DomainMatcher) AllowPatterns() []string {
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	return copySlice(dm.allow)
}

// DenyPatterns returns a copy of the current denylist.
func (dm *DomainMatcher) DenyPatterns() []string {
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	return copySlice(dm.deny)
}

// matchDomain checks whether host matches pattern.
//
// Both are split on '.', lowercased, and compared segment by segment.
// A '*' pattern segment matches exactly one host segment (any non-empty
// sequence of non-dot characters). A bare '*' pattern matches any host.
//
// Returns false when segment counts differ (no multi-segment wildcards).
func matchDomain(host, pattern string) bool {
	host = strings.ToLower(host)
	pattern = strings.ToLower(pattern)

	hParts := strings.Split(host, ".")
	pParts := strings.Split(pattern, ".")

	if len(pParts) == 1 && pParts[0] == "*" {
		// Bare "*" matches any domain.
		return true
	}

	if len(hParts) != len(pParts) {
		return false
	}

	for i := range hParts {
		if pParts[i] == "*" {
			continue
		}
		if hParts[i] != pParts[i] {
			return false
		}
	}
	return true
}

// stripPort removes the port portion from a host string if present.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// copySlice returns a copy of s (or nil when s is nil).
func copySlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// removePattern removes the first occurrence of pattern from the slice,
// preserving order.
func removePattern(s []string, pattern string) []string {
	for i, p := range s {
		if p == pattern {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}
