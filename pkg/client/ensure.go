package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lidessen/crabbro/pkg/api"
)

// EnsureDaemon returns when /v1/status answers. If the daemon is not running
// it starts crabbrod from beside the current executable, then from PATH, with
// the client's socket and TCP address. MITM and proxy-address flags are left
// at the daemon defaults; the CLI `daemon start` command remains the place
// for those.
func (c *Client) EnsureDaemon(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.ping(ctx); err == nil {
		return nil
	}
	bin, err := findDaemonBin()
	if err != nil {
		return err
	}
	uds := c.uds
	tcp := c.tcp
	if uds == "" {
		uds = expandPath(api.DefaultUDSPath)
	}
	if tcp == "" {
		tcp = api.DefaultTCPAddr
	}
	if err := os.MkdirAll(filepath.Dir(uds), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(filepath.Dir(uds), "crabbrod.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "--uds-path", uds, "--tcp-addr", tcp)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	_ = os.WriteFile(filepath.Join(filepath.Dir(uds), "crabbrod.pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	logf.Close()

	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.ping(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if last != nil {
		return fmt.Errorf("daemon did not become ready: %w", last)
	}
	return fmt.Errorf("daemon did not become ready")
}

func (c *Client) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err := c.Status(ctx)
	return err
}

func findDaemonBin() (string, error) {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "crabbrod")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("crabbrod"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("crabbrod not found beside the executable or on PATH")
}
