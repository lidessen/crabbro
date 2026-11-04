//go:build linux

package inspect

import (
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func init() {
	lookupProcess = lookupLinux
}

func lookupLinux(addr net.Addr) Process {
	proc, ok := lookupLinuxRoot("/proc", addr)
	if !ok {
		return Process{Kind: "unknown", Platform: "linux"}
	}
	return proc
}

func lookupLinuxRoot(root string, addr net.Addr) (Process, bool) {
	ip, port, ok := splitAddr(addr)
	if !ok {
		return Process{}, false
	}
	inode := findInode(root, ip, port)
	if inode == "" {
		return Process{}, false
	}
	pid := findPIDByInode(root, inode)
	if pid <= 0 {
		return Process{}, false
	}
	return describeLinux(root, pid, 0), true
}

func splitAddr(addr net.Addr) (net.IP, int, bool) {
	if addr == nil {
		return nil, 0, false
	}
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, 0, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, 0, false
	}
	return ip, port, true
}

func findInode(root string, ip net.IP, port int) string {
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			lip, lport, ok := parseProcAddr(fields[1])
			if !ok || lport != port || !lip.Equal(ip) {
				continue
			}
			return fields[9]
		}
	}
	return ""
}

func parseProcAddr(s string) (net.IP, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return nil, 0, false
	}
	port64, err := strconv.ParseInt(parts[1], 16, 32)
	if err != nil {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(parts[0])
	if err != nil {
		return nil, 0, false
	}
	// The kernel prints IPv4 as a little-endian 32-bit word, and IPv6 as
	// four little-endian words.
	switch len(raw) {
	case 4:
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]), int(port64), true
	case 16:
		ip := make(net.IP, 16)
		for i := 0; i < 4; i++ {
			ip[i*4+0] = raw[i*4+3]
			ip[i*4+1] = raw[i*4+2]
			ip[i*4+2] = raw[i*4+1]
			ip[i*4+3] = raw[i*4+0]
		}
		return ip, int(port64), true
	default:
		return nil, 0, false
	}
}

func findPIDByInode(root, inode string) int {
	want := "socket:[" + inode + "]"
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(root, ent.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(root, ent.Name(), "fd", fd.Name()))
			if err == nil && target == want {
				return pid
			}
		}
	}
	return 0
}

func describeLinux(root string, pid, depth int) Process {
	comm := readTrim(filepath.Join(root, strconv.Itoa(pid), "comm"))
	exe, _ := os.Readlink(filepath.Join(root, strconv.Itoa(pid), "exe"))
	cmd := readCmdline(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	ppid := readPPID(filepath.Join(root, strconv.Itoa(pid), "status"))
	proc := Process{PID: pid, Name: comm, Executable: exe, Platform: "linux"}
	if depth < 6 && isHelper(cmd, comm) && ppid > 0 && ppid != pid {
		parent := describeLinux(root, ppid, depth+1)
		if parent.Name != "" {
			proc.HelperPID = pid
			proc.PID = parent.PID
			proc.Name = parent.Name
			if parent.Executable != "" {
				proc.Executable = parent.Executable
			}
		}
	}
	proc.Kind = kindOf(proc.Name, proc.Executable)
	return proc
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readCmdline(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}

func readPPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			return n
		}
	}
	return 0
}
