package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lidessen/crabbro/pkg/client"
)

// clientConfig holds the connection configuration for the control API.
type clientConfig struct {
	UDS   string
	TCP   string
	Token string
}

func (cfg *clientConfig) apiToken() string {
	if cfg.Token != "" {
		return cfg.Token
	}
	if env := os.Getenv("CRABBRO_API_TOKEN"); env != "" {
		return env
	}
	candidates := []string{}
	if cfg.UDS != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(cfg.UDS), "api.token"))
	}
	candidates = append(candidates, expandPath("~/.crabbro/api.token"))
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func (cfg *clientConfig) apiClient() *client.Client {
	return client.New(client.Options{
		UDS:     cfg.UDS,
		TCP:     cfg.TCP,
		Token:   cfg.apiToken(),
		Timeout: 10 * time.Second,
	})
}

// doRequest performs an HTTP request to the control API, trying UDS first then
// falling back to TCP.
func doRequest(cfg *clientConfig, method, path string, body io.Reader) (*http.Response, error) {
	return cfg.apiClient().Do(context.Background(), method, path, body)
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
