package router

import (
	"regexp"
	"strings"
)

var clientSuffix = regexp.MustCompile(`(?i)(\[[0-9]+[km]\]|:(minimal|low|medium|high|xhigh|max|off))$`)

// NormalizeModelName removes recognized client annotations from a model name.
func NormalizeModelName(raw string) string {
	s := strings.TrimSpace(raw)
	for {
		next := clientSuffix.ReplaceAllString(s, "")
		if next == s {
			return s
		}
		s = next
	}
}
