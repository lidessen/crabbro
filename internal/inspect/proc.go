package inspect

import (
	"net"
	"sync"
	"time"
)

// lookupProcess is set by the OS-specific file. A nil function means the
// platform cannot see process owners; callers still record the exchange.
var lookupProcess func(addr net.Addr) Process

var (
	procCacheMu sync.Mutex
	procCache   = map[string]cachedProc{}
)

type cachedProc struct {
	proc Process
	at   time.Time
}

// LookupProcess returns the process that owns addr. Failure is an unknown
// source, never an error that drops the exchange. Results are cached briefly
// for the life of a short connection.
func LookupProcess(addr net.Addr) Process {
	if addr == nil || lookupProcess == nil {
		return Process{Kind: "unknown", Platform: platformName()}
	}
	key := addr.String()
	procCacheMu.Lock()
	if c, ok := procCache[key]; ok && time.Since(c.at) < 30*time.Second {
		procCacheMu.Unlock()
		return c.proc
	}
	procCacheMu.Unlock()
	proc := lookupProcess(addr)
	if proc.Kind == "" {
		proc.Kind = "unknown"
	}
	if proc.Platform == "" {
		proc.Platform = platformName()
	}
	procCacheMu.Lock()
	procCache[key] = cachedProc{proc: proc, at: time.Now()}
	procCacheMu.Unlock()
	return proc
}

func platformName() string {
	return osPlatform
}
