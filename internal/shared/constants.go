package shared

import (
	"time"

	"github.com/lidessen/crabbro/pkg/api"
)

// Default configuration values for the proxy daemon and CLI.
const (
	// DefaultUDSPath is the default Unix domain socket path for the control API.
	DefaultUDSPath = api.DefaultUDSPath

	// DefaultTCPAddr is the default TCP address for the control API.
	DefaultTCPAddr = api.DefaultTCPAddr

	// DefaultProxyAddr is the default address for the HTTP proxy listener.
	DefaultProxyAddr = "localhost:8080"

	// DefaultRingBufferCapacity is the default number of requests kept in the
	// in-memory ring buffer.
	DefaultRingBufferCapacity = 1024

	// DefaultRuleTimeout is the maximum execution time per rule evaluation.
	DefaultRuleTimeout = 500 * time.Millisecond

	// DefaultRuleBudget is the total rule-evaluation budget for one exchange.
	DefaultRuleBudget = 2 * time.Second

	// DefaultRulePriority is the priority assigned to a rule when none is set.
	DefaultRulePriority = 100

	// MaxBodySize is the maximum request/response body size stored per exchange
	// (64 KB). Larger bodies are truncated.
	MaxBodySize = 64 * 1024

	// MaxBodyCacheBytes is the maximum total body cache size (~10 MB).
	MaxBodyCacheBytes = 10 * 1024 * 1024

	// MaxTransformBody is the largest response body a rewrite may read.
	// Larger bodies are left unchanged.
	MaxTransformBody = 256 * 1024

	// DefaultHistoryRetention is how long completed exchanges are kept.
	DefaultHistoryRetention = 7 * 24 * time.Hour

	// DefaultHistoryMaxRows is the cap on stored exchanges.
	DefaultHistoryMaxRows = 10000

	// MaxDelay is the longest response delay a rule may request.
	MaxDelay = 30 * time.Second
)
