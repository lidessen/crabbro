package inspect

import (
	"path/filepath"
	"strings"
)

func kindOf(name, exe string) string {
	n := strings.ToLower(name + " " + filepath.Base(exe))
	switch {
	case strings.TrimSpace(name) == "" && exe == "":
		return "unknown"
	case strings.Contains(n, "chrome") || strings.Contains(n, "chromium") || strings.Contains(n, "firefox") || strings.Contains(n, "msedge") || strings.Contains(n, "safari") || strings.Contains(n, "brave"):
		return "browser"
	case strings.Contains(n, "curl") || strings.Contains(n, "wget") || strings.Contains(n, "httpie"):
		return "cli"
	default:
		return "app"
	}
}

func isHelper(cmdline, comm string) bool {
	l := strings.ToLower(cmdline + " " + comm)
	return strings.Contains(l, "--type=") || strings.Contains(l, " helper")
}
