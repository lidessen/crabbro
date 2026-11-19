package inspect

import (
	"net"
	"net/http"

	"github.com/lidessen/crabbro/internal/shared"
)

// ScopeSource is the recording session's current scope. Nil means no session.
type ScopeSource interface {
	CurrentScope() *Scope
}

// Runtime is the in-process inspection state for one daemon.
type Runtime struct {
	Catalog *Catalog
	Durable *Durable
	Scopes  ScopeSource
}

func NewRuntime(scopeDir string) *Runtime {
	return &Runtime{Catalog: NewCatalog(), Durable: OpenDurable(scopeDir)}
}

// Annotate fills attribution and the hidden flag. A true return means the
// exchange matches an explicit bypass: do not record it and do not run rules.
func (rt *Runtime) Annotate(rec *shared.Request, hdr http.Header, remote net.Addr) bool {
	if rt == nil || rec == nil {
		return false
	}
	proc := LookupProcess(remote)
	class := Classify(hdr, rec.URL)
	src, page := rt.Catalog.Ensure(proc, class)
	rec.Attribution = &shared.Attribution{
		SourceID: src.ID, PageID: page.ID, InitiatorURL: class.InitiatorURL,
		ResourceType: class.ResourceType, Confidence: class.Confidence,
		App: src.Name, PID: src.PID, HelperPID: src.HelperPID, Executable: src.Executable,
		Kind: src.Kind, Origin: class.Origin, PageURL: class.PageURL,
	}
	sub := Subject{
		App: src.Name, PID: src.PID, SourceID: src.ID, PageID: page.ID,
		Origin: class.Origin, Host: rec.Host, Path: rec.Path,
		ResourceType: class.ResourceType, InitiatorURL: class.InitiatorURL,
	}
	v := Decide(sub, rt.scope(), rt.Durable.Snapshot())
	if v.Bypass {
		return true
	}
	rec.Hidden = v.Hidden
	rec.HiddenReason = v.Reasons
	rt.Catalog.Tally(src.ID, page.ID, v)
	return false
}

// BypassHost reports whether a new connection to host should skip MITM and
// recording. Resource type is not known yet, so type filters do not match.
func (rt *Runtime) BypassHost(remote net.Addr, host string) bool {
	if rt == nil {
		return false
	}
	proc := LookupProcess(remote)
	sub := Subject{App: proc.Name, PID: proc.PID, Host: host}
	return Decide(sub, rt.scope(), rt.Durable.Snapshot()).Bypass
}

func (rt *Runtime) scope() *Scope {
	if rt == nil || rt.Scopes == nil {
		return nil
	}
	return rt.Scopes.CurrentScope()
}

// Status is the JSON shape of GET /v1/scope.
func (rt *Runtime) Status(sessionID string, implicit bool) shared.ScopeState {
	var pick, omit, bypass []string
	if sc := rt.scope(); sc != nil {
		pick, omit, bypass = sc.raw()
	}
	snap := DurableSnapshot{}
	if rt != nil && rt.Durable != nil {
		snap = rt.Durable.Snapshot()
	}
	if pick == nil {
		pick = []string{}
	}
	if omit == nil {
		omit = []string{}
	}
	if bypass == nil {
		bypass = []string{}
	}
	allow := snap.AllowRaw
	deny := snap.DenyRaw
	if allow == nil {
		allow = []string{}
	}
	if deny == nil {
		deny = []string{}
	}
	return shared.ScopeState{
		SessionID:       sessionID,
		ImplicitSession: implicit,
		Pick:            pick,
		Omit:            omit,
		Bypass:          bypass,
		ConfigDeny:      deny,
		ConfigAllow:     allow,
		Restricting:     snap.AllowRestricts,
	}
}
