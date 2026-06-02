package shared

import "time"

// Default configuration values for the proxy daemon and CLI.
const (
	// DefaultUDSPath is the default Unix domain socket path for the control API.
	DefaultUDSPath = "~/.chiling/proxy.sock"

	// DefaultTCPAddr is the default TCP address for the control API.
	DefaultTCPAddr = "localhost:16379"

	// DefaultProxyAddr is the default address for the HTTP proxy listener.
	DefaultProxyAddr = "localhost:8080"

	// DefaultRingBufferCapacity is the default number of requests kept in the
	// in-memory ring buffer.
	DefaultRingBufferCapacity = 1024

	// DefaultRuleTimeout is the maximum execution time per rule evaluation.
	DefaultRuleTimeout = 500 * time.Millisecond

	// DefaultRulePriority is the priority assigned to a rule when none is set.
	DefaultRulePriority = 100

	// MaxBodySize is the maximum request/response body size stored per exchange
	// (64 KB). Larger bodies are truncated.
	MaxBodySize = 64 * 1024

	// MaxBodyCacheBytes is the maximum total body cache size (~10 MB).
	MaxBodyCacheBytes = 10 * 1024 * 1024
)
