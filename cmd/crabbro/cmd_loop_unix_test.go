//go:build unix

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDaemonHelper(t *testing.T) {
	switch os.Getenv("CRABBRO_TEST_HELPER") {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelper$")
		cmd.Env = append(os.Environ(), "CRABBRO_TEST_HELPER=child")
		detachDaemon(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Process.Release(); err != nil {
			t.Fatal(err)
		}
	case "child":
		uds := os.Getenv("CRABBRO_TEST_SOCKET")
		ln, err := net.Listen("unix", uds)
		if err != nil {
			t.Fatal(err)
		}
		http.HandleFunc("/v1/status", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"pid":%d,"executable":%q}`, os.Getpid(), os.Args[0])
		})
		go http.Serve(ln, nil)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		if err := os.WriteFile(filepath.Join(filepath.Dir(uds), "crabbrod.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
		<-ch
		ln.Close()
	}
}

func TestDaemonDetachAndStop(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "crabbro-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	uds := filepath.Join(dir, "api.sock")
	parent := exec.Command(os.Args[0], "-test.run=^TestDaemonHelper$")
	parent.Env = append(os.Environ(), "CRABBRO_TEST_HELPER=parent", "CRABBRO_TEST_SOCKET="+uds)
	if out, err := parent.CombinedOutput(); err != nil {
		t.Fatalf("parent: %v: %s", err, out)
	}
	pidPath := filepath.Join(dir, "crabbrod.pid")
	var pid int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidPath); err == nil {
			pid, _ = strconv.Atoi(string(b))
			break
		}
	}
	if pid == 0 {
		t.Fatal("child did not start after parent exited")
	}
	defer func() {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	sid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if sid != pid || pgid != pid {
		t.Fatalf("session = %d, process group = %d, want %d", sid, pgid, pid)
	}

	cfg := &clientConfig{UDS: uds, Token: "test"}
	status, err := daemonStatus(cfg)
	if err != nil || status.PID != pid || status.Executable == "" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if err := stopDaemon(cfg); err != nil {
		t.Fatal(err)
	}
	pid = 0
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file remains: %v", err)
	}
}

func TestDaemonStopRejectsMismatchedPID(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "crabbro-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	uds := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, `{"pid":%d}`, os.Getpid()+1) })}
	defer server.Close()
	go server.Serve(ln)
	pidPath := filepath.Join(dir, "crabbrod.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stopDaemon(&clientConfig{UDS: uds, Token: "test"}); err == nil || !strings.Contains(err.Error(), "control API reports") {
		t.Fatalf("stop error = %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatal(err)
	}
}

func TestFindDaemonBinResolvesAppLink(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Crabbro.app", "Contents", "MacOS")
	local := filepath.Join(dir, "local", "bin")
	for _, path := range []string{app, local} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(app, "crabbro"), filepath.Join(app, "crabbrod"), filepath.Join(local, "crabbrod")} {
		if err := os.WriteFile(path, nil, 0755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(local, "crabbro")
	if err := os.Symlink(filepath.Join(app, "crabbro"), link); err != nil {
		t.Fatal(err)
	}
	got, err := findDaemonBinFrom(link)
	want, _ := filepath.EvalSymlinks(filepath.Join(app, "crabbrod"))
	if err != nil || got != want {
		t.Fatalf("daemon = %q, %v", got, err)
	}
}
