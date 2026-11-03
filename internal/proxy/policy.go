package proxy

import (
	"net"
	"strings"
	"sync/atomic"

	"github.com/lidessen/chiling/internal/mitm"
	"golang.org/x/net/idna"
)

// NetworkPolicy is an immutable MITM decision for new CONNECT tunnels.
// An exchange keeps the generation it was admitted under.
//
// EmptyAllowDenies is the target default: an empty allowlist intercepts
// nothing. A bare "*" allow pattern is an explicit operator choice.
type NetworkPolicy struct {
	Generation       uint64
	Enabled          bool
	Allow            []string
	Deny             []string
	EmptyAllowDenies bool
}

// ShouldIntercept reports whether a new CONNECT to hostport should be decrypted.
// hostport is normalized (IDNA, trailing dot, port) before pattern matching.
// Deny patterns win. The decision does not consult rule hosts.
func (p NetworkPolicy) ShouldIntercept(hostport string) bool {
	if !p.Enabled {
		return false
	}
	host := normalizeHost(hostport)
	if host == "" {
		return false
	}
	for _, pattern := range p.Deny {
		if mitm.HostMatches(host, pattern) {
			return false
		}
	}
	if len(p.Allow) == 0 {
		return !p.EmptyAllowDenies
	}
	for _, pattern := range p.Allow {
		if mitm.HostMatches(host, pattern) {
			return true
		}
	}
	return false
}

// PolicyStore publishes network policy snapshots atomically.
type PolicyStore struct {
	cur    atomic.Pointer[NetworkPolicy]
	nextID atomic.Uint64
}

// Publish stores a copy of pol and returns the generation assigned to it.
func (s *PolicyStore) Publish(pol NetworkPolicy) uint64 {
	pol.Generation = s.nextID.Add(1)
	pol.Allow = append([]string(nil), pol.Allow...)
	pol.Deny = append([]string(nil), pol.Deny...)
	s.cur.Store(&pol)
	return pol.Generation
}

// Current returns the active snapshot, or nil when nothing has been published.
func (s *PolicyStore) Current() *NetworkPolicy {
	return s.cur.Load()
}

// normalizeHost lowercases a DNS name, strips a trailing dot, decodes IDNA,
// and removes the port. IP literals are returned without a port or brackets.
func normalizeHost(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		ascii = host
	}
	return strings.ToLower(ascii)
}
