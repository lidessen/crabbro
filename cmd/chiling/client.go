package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// clientConfig holds the connection configuration for the control API.
type clientConfig struct {
	UDS string
	TCP string
}

// doRequest performs an HTTP request to the control API, trying UDS first then
// falling back to TCP.
func doRequest(cfg *clientConfig, method, path string, body io.Reader) (*http.Response, error) {
	// Try UDS first.
	if cfg.UDS != "" {
		client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				Dial: func(network, addr string) (net.Conn, error) {
					return net.Dial("unix", cfg.UDS)
				},
			},
		}
		req, err := http.NewRequest(method, "http://localhost"+path, body)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
	}

	// Fall back to TCP.
	if cfg.TCP != "" {
		client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
		url := fmt.Sprintf("http://%s%s", cfg.TCP, path)
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	return nil, fmt.Errorf("no control API connection available (tried UDS and TCP)")
}

// formatDuration formats seconds as a human-readable duration string.
func formatDuration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60

	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if secs > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	return strings.Join(parts, " ")
}

// expandPath expands ~ to the user's home directory.
func expandPath(path string) string {
	if len(path) > 1 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}
