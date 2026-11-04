package api

import "time"

// APIVersion is the control API major version on the wire. Paths are /v1/...
const APIVersion = "v1"

// DefaultUDSPath is the owner-only Unix socket for the control API.
const DefaultUDSPath = "~/.chiling/proxy.sock"

// DefaultTCPAddr is the localhost TCP fallback for the control API.
const DefaultTCPAddr = "localhost:16379"

// DaemonStatus is GET /v1/status.
type DaemonStatus = StatusResponse

// HistoryStatus is GET /v1/history.
type HistoryStatus struct {
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path,omitempty"`
	Requests  int    `json:"requests"`
	Retention string `json:"retention,omitempty"`
	MaxRows   int    `json:"maxRows,omitempty"`
}

// Plugin is one installed plugin package.
type Plugin struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Path        string      `json:"path"`
	Entry       string      `json:"entry"`
	API         string      `json:"api"`
	Enabled     bool        `json:"enabled"`
	Priority    int         `json:"priority"`
	FailureMode string      `json:"failureMode"`
	Isolation   string      `json:"isolation"`
	Permissions []string    `json:"permissions"`
	Activation  []string    `json:"activation"`
	Digest      string      `json:"digest"`
	Match       PluginMatch `json:"match,omitempty"`
}

// PluginMatch is the static prefilter from a plugin manifest.
type PluginMatch struct {
	Hosts      []string `json:"hosts,omitempty"`
	Methods    []string `json:"methods,omitempty"`
	PathPrefix string   `json:"pathPrefix,omitempty"`
}

// Source is one local app the proxy has seen.
type Source struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Executable string    `json:"executable,omitempty"`
	PID        int       `json:"pid,omitempty"`
	HelperPID  int       `json:"helperPid,omitempty"`
	Platform   string    `json:"platform,omitempty"`
	Seen       int       `json:"seen"`
	InScope    int       `json:"inScope"`
	Hidden     int       `json:"hidden"`
	LastSeen   time.Time `json:"lastSeen"`
	PageIDs    []string  `json:"pageIds,omitempty"`
}

// Page is a header-derived document grouped under a source.
type Page struct {
	ID         string    `json:"id"`
	SourceID   string    `json:"sourceId"`
	Origin     string    `json:"origin,omitempty"`
	URL        string    `json:"url,omitempty"`
	Confidence string    `json:"confidence,omitempty"`
	Seen       int       `json:"seen"`
	InScope    int       `json:"inScope"`
	Hidden     int       `json:"hidden"`
	LastSeen   time.Time `json:"lastSeen"`
}

// SourceDetail is GET /v1/sources/{id}.
type SourceDetail struct {
	Source Source `json:"source"`
	Pages  []Page `json:"pages"`
}

// SourceList is GET /v1/sources.
type SourceList struct {
	Sources []Source `json:"sources"`
}

// ScopeState is GET /v1/scope and the body of a successful scope change.
type ScopeState struct {
	SessionID       string   `json:"sessionId"`
	ImplicitSession bool     `json:"implicitSession"`
	Pick            []string `json:"pick"`
	Omit            []string `json:"omit"`
	Bypass          []string `json:"bypass"`
	ConfigDeny      []string `json:"configDeny"`
	ConfigAllow     []string `json:"configAllow"`
	Restricting     bool     `json:"restricting"`
}

// TrajectoryPage is one page of the timeline for the main screen.
type TrajectoryPage struct {
	Items      []Activity `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ActivityList is GET /v1/activity (the tail of the timeline).
type ActivityList struct {
	Activity []Activity `json:"activity"`
}

// Diagnostic is one rule or scope validation message.
type Diagnostic struct {
	File    string `json:"file,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message,omitempty"`
}

// RulePreview is POST /v1/rules/preview.
type RulePreview struct {
	Matched     []string     `json:"matched"`
	Disabled    []string     `json:"disabled"`
	Complete    bool         `json:"complete"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// ValidateResult is POST /v1/config/validate.
type ValidateResult struct {
	OK          bool         `json:"ok"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// ConfigAck is the body of a draft, commit, or discard.
type ConfigAck struct {
	ID        string `json:"id"`
	Draft     bool   `json:"draft,omitempty"`
	Committed bool   `json:"committed,omitempty"`
	Discarded bool   `json:"discarded,omitempty"`
}

// PluginList is GET /v1/plugins.
type PluginList struct {
	Plugins []Plugin `json:"plugins"`
}

// SessionList is GET /v1/sessions.
type SessionList struct {
	Sessions []Session `json:"sessions"`
}

// CurrentSession is GET /v1/sessions/current.
type CurrentSession struct {
	Session *Session `json:"session"`
}
