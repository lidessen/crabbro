//go:build darwin

package inspect

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
)

var osPlatform = "darwin"

func init() {
	lookupProcess = lookupDarwin
}

// lookupDarwin asks lsof for the PID that owns the local port. proc_pidfdinfo
// is the preferred API and is not linked into this build; lsof is the fallback
// the design allows. A failure returns an unknown source.
func lookupDarwin(addr net.Addr) Process {
	_, port, ok := splitDarwin(addr)
	if !ok {
		return Process{Kind: "unknown", Platform: "darwin"}
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:ESTABLISHED", "-F", "pcn").Output()
	if err != nil {
		return Process{Kind: "unknown", Platform: "darwin"}
	}
	var pid int
	var name string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			pid, _ = strconv.Atoi(strings.TrimPrefix(line, "p"))
		}
		if strings.HasPrefix(line, "c") {
			name = strings.TrimPrefix(line, "c")
		}
	}
	if pid <= 0 {
		return Process{Kind: "unknown", Platform: "darwin"}
	}
	return Process{PID: pid, Name: name, Kind: kindOf(name, ""), Platform: "darwin"}
}

func splitDarwin(addr net.Addr) (string, int, bool) {
	if addr == nil {
		return "", 0, false
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, false
	}
	return "", port, true
}
