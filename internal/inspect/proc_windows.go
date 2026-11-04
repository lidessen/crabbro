//go:build windows

package inspect

import (
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"
)

var osPlatform = "windows"

func init() {
	lookupProcess = lookupWindows
}

var (
	modIPHlp  = syscall.NewLazyDLL("iphlpapi.dll")
	modKernel = syscall.NewLazyDLL("kernel32.dll")
	procTable = modIPHlp.NewProc("GetExtendedTcpTable")
	procOpen  = modKernel.NewProc("OpenProcess")
	procQuery = modKernel.NewProc("QueryFullProcessImageNameW")
	procClose = modKernel.NewProc("CloseHandle")
)

const tcpTableOwnerPIDAll = 5

// lookupWindows uses GetExtendedTcpTable(TCP_TABLE_OWNER_PID_ALL). A failure
// returns an unknown source and does not drop the exchange.
func lookupWindows(addr net.Addr) Process {
	ip, port, ok := splitWin(addr)
	if !ok {
		return Process{Kind: "unknown", Platform: "windows"}
	}
	var size uint32
	_, _, _ = procTable.Call(0, uintptr(unsafe.Pointer(&size)), 1, syscall.AF_INET, tcpTableOwnerPIDAll, 0)
	if size == 0 {
		return Process{Kind: "unknown", Platform: "windows"}
	}
	buf := make([]byte, size)
	r, _, _ := procTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 1, syscall.AF_INET, tcpTableOwnerPIDAll, 0)
	if r != 0 {
		return Process{Kind: "unknown", Platform: "windows"}
	}
	n := binary.LittleEndian.Uint32(buf[:4])
	row := 24
	for i := uint32(0); i < n; i++ {
		off := 4 + int(i)*row
		if off+row > len(buf) {
			break
		}
		localAddr := buf[off+4 : off+8]
		localPort := binary.BigEndian.Uint16(buf[off+8 : off+10])
		pid := binary.LittleEndian.Uint32(buf[off+20 : off+24])
		if int(localPort) == port && net.IP(localAddr).Equal(ip.To4()) {
			return processFromPID(int(pid))
		}
	}
	return Process{Kind: "unknown", Platform: "windows"}
}

func processFromPID(pid int) Process {
	const queryLimited = 0x1000
	h, _, _ := procOpen.Call(queryLimited, 0, uintptr(pid))
	if h == 0 {
		return Process{PID: pid, Kind: "unknown", Platform: "windows"}
	}
	defer procClose.Call(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procQuery.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	exe := ""
	if r != 0 {
		exe = syscall.UTF16ToString(buf[:n])
	}
	name := exe
	return Process{PID: pid, Name: name, Executable: exe, Kind: kindOf(name, exe), Platform: "windows"}
}

func splitWin(addr net.Addr) (net.IP, int, bool) {
	if addr == nil {
		return nil, 0, false
	}
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, 0, false
	}
	port, err := atoi(portStr)
	if err != nil {
		return nil, 0, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, 0, false
	}
	return ip, port, true
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, syscall.EINVAL
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, syscall.EINVAL
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
