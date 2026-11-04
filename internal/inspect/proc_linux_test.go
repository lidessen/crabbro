//go:build linux

package inspect

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLinuxLookupFindsDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	proc := LookupProcess(client.LocalAddr())
	if proc.Kind == "unknown" || proc.PID != os.Getpid() {
		t.Fatalf("process = %+v, want pid %d", proc, os.Getpid())
	}
}

func TestLinuxHelperRollup(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 127.0.0.1:40000 is 0100007F:9C40
	write("net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n   0: 0100007F:9C40 0100007F:0050 01 00000000:00000000 00:00000000 00000000     0        0 99 1 0000000000000000 100 0 0 10 0\n")
	write("42/comm", "chrome\n")
	write("42/cmdline", "chrome\x00--type=utility\x00")
	write("42/status", "Name:\tchrome\nPPid:\t7\n")
	write("7/comm", "chrome\n")
	write("7/cmdline", "chrome\x00")
	write("7/status", "Name:\tchrome\nPPid:\t1\n")
	if err := os.MkdirAll(filepath.Join(root, "42/fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[99]", filepath.Join(root, "42/fd/3")); err != nil {
		t.Fatal(err)
	}
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:40000")
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := lookupLinuxRoot(root, addr)
	if !ok {
		t.Fatal("lookup failed")
	}
	if proc.HelperPID != 42 || proc.PID != 7 || proc.Kind != "browser" {
		t.Fatalf("process = %+v", proc)
	}
	if strconv.Itoa(proc.PID) != "7" {
		t.Fatal(proc.PID)
	}
}
