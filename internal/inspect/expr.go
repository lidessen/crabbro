package inspect

import (
	"fmt"
	"strconv"
	"strings"
)

// Expr is one scope token such as type:xhr or origin:https://app.example.com.
type Expr struct {
	Key   string
	Value string
}

// ParseExpr parses one expression. The error does not include the value.
func ParseExpr(raw string) (Expr, error) {
	raw = strings.TrimSpace(raw)
	i := strings.Index(raw, ":")
	if i <= 0 || i == len(raw)-1 {
		return Expr{}, fmt.Errorf("invalid expression")
	}
	key := strings.ToLower(raw[:i])
	val := raw[i+1:]
	switch key {
	case "app", "pid", "origin", "page", "host", "pathprefix", "type", "initiator":
	default:
		return Expr{}, fmt.Errorf("unknown key")
	}
	if key == "pid" {
		if _, err := strconv.Atoi(val); err != nil {
			return Expr{}, fmt.Errorf("invalid expression")
		}
	}
	if strings.TrimSpace(val) == "" {
		return Expr{}, fmt.Errorf("invalid expression")
	}
	return Expr{Key: key, Value: val}, nil
}

// ParseAll parses a list and reports the index of the first bad token.
func ParseAll(raw []string) ([]Expr, error) {
	out := make([]Expr, 0, len(raw))
	for i, s := range raw {
		e, err := ParseExpr(s)
		if err != nil {
			return nil, fmt.Errorf("expr[%d]: %s", i, err.Error())
		}
		out = append(out, e)
	}
	return out, nil
}

// Subject is the part of an exchange expressions can see.
type Subject struct {
	App          string
	PID          int
	SourceID     string
	PageID       string
	Origin       string
	Host         string
	Path         string
	ResourceType string
	InitiatorURL string
}

// Match reports whether the expression matches the subject.
// An expression whose field is empty on the subject does not match.
func (e Expr) Match(s Subject) bool {
	switch e.Key {
	case "app":
		return s.App != "" && strings.Contains(strings.ToLower(s.App), strings.ToLower(e.Value))
	case "pid":
		n, _ := strconv.Atoi(e.Value)
		return s.PID != 0 && s.PID == n
	case "origin":
		return s.Origin != "" && strings.EqualFold(s.Origin, e.Value)
	case "page":
		return s.PageID != "" && s.PageID == e.Value
	case "host":
		return s.Host != "" && hostMatch(e.Value, s.Host)
	case "pathprefix":
		return s.Path != "" && strings.HasPrefix(s.Path, e.Value)
	case "type":
		return s.ResourceType != "" && strings.EqualFold(s.ResourceType, e.Value)
	case "initiator":
		return s.InitiatorURL != "" && strings.EqualFold(s.InitiatorURL, e.Value)
	default:
		return false
	}
}

func anyMatch(exprs []Expr, s Subject) bool {
	for _, e := range exprs {
		if e.Match(s) {
			return true
		}
	}
	return false
}

func hostMatch(pattern, host string) bool {
	host = stripPort(host)
	pattern = stripPort(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		suf := pattern[1:]
		return strings.HasSuffix(strings.ToLower(host), strings.ToLower(suf)) && len(host) > len(suf)
	}
	return strings.EqualFold(pattern, host)
}

func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && strings.Count(host, ":") == 1 {
		return host[:i]
	}
	return host
}
