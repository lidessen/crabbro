package inspect

import "sync"

// Scope is the REPL filter for one recording session. It is not written to disk.
type Scope struct {
	mu        sync.Mutex
	Pick      []Expr
	Omit      []Expr
	Bypass    []Expr
	PickRaw   []string
	OmitRaw   []string
	BypassRaw []string
}

func (s *Scope) snapshot() (pick, omit, bypass []Expr) {
	if s == nil {
		return nil, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Expr(nil), s.Pick...), append([]Expr(nil), s.Omit...), append([]Expr(nil), s.Bypass...)
}

// ReplacePick replaces the positive filter.
func (s *Scope) ReplacePick(raw []string) error {
	exprs, err := ParseAll(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Pick = exprs
	s.PickRaw = append([]string(nil), raw...)
	return nil
}

// AddOmit appends exclusions.
func (s *Scope) AddOmit(raw []string) error {
	exprs, err := ParseAll(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Omit = append(s.Omit, exprs...)
	s.OmitRaw = append(s.OmitRaw, raw...)
	return nil
}

// AddBypass appends explicit bypass expressions.
func (s *Scope) AddBypass(raw []string) error {
	exprs, err := ParseAll(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Bypass = append(s.Bypass, exprs...)
	s.BypassRaw = append(s.BypassRaw, raw...)
	return nil
}

// Restore removes an expression from omit. If a pick list exists and the
// expression names a page or source id, it is added to the pick list.
func (s *Scope) Restore(raw string) error {
	e, err := ParseExpr(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Omit, s.OmitRaw = dropExpr(s.Omit, s.OmitRaw, raw)
	if len(s.Pick) > 0 && (e.Key == "page" || e.Key == "pid") {
		s.Pick = append(s.Pick, e)
		s.PickRaw = append(s.PickRaw, raw)
	}
	return nil
}

// Reset clears session filters. Durable lists are untouched.
func (s *Scope) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Pick, s.Omit, s.Bypass = nil, nil, nil
	s.PickRaw, s.OmitRaw, s.BypassRaw = nil, nil, nil
}

func (s *Scope) raw() (pick, omit, bypass []string) {
	if s == nil {
		return nil, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.PickRaw...), append([]string(nil), s.OmitRaw...), append([]string(nil), s.BypassRaw...)
}

func dropExpr(exprs []Expr, raw []string, token string) ([]Expr, []string) {
	var ne []Expr
	var nr []string
	for i, r := range raw {
		if r == token {
			continue
		}
		nr = append(nr, r)
		if i < len(exprs) {
			ne = append(ne, exprs[i])
		}
	}
	return ne, nr
}

// Verdict is how an exchange relates to the current view.
type Verdict struct {
	Hidden  bool
	Reasons []string
	Bypass  bool
	InScope bool
}

// Decide applies durable lists and the session scope.
// Bypass wins. A hidden exchange is still forwarded, recorded, and offered to rules.
func Decide(sub Subject, session *Scope, durable DurableSnapshot) Verdict {
	pick, omit, bypass := session.snapshot()
	if anyMatch(bypass, sub) || anyMatch(durable.Bypass, sub) {
		return Verdict{Bypass: true}
	}
	var reasons []string
	if anyMatch(durable.Deny, sub) {
		reasons = append(reasons, "config-deny")
	}
	if len(durable.Allow) > 0 && !anyMatch(durable.Allow, sub) {
		reasons = append(reasons, "config-allow")
	}
	if len(pick) > 0 && !anyMatch(pick, sub) {
		reasons = append(reasons, "session-pick")
	}
	if anyMatch(omit, sub) {
		reasons = append(reasons, "session-omit")
	}
	if len(reasons) > 0 {
		return Verdict{Hidden: true, Reasons: reasons}
	}
	return Verdict{InScope: true}
}
