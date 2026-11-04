package rule

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// expr is a small expression. Conditions use ==, !=, &&, ||, and parentheses.
// There are no calls and no arithmetic.

type exprKind int

const (
	exPath exprKind = iota
	exLit
	exNot
	exEq
	exNe
	exAnd
	exOr
)

type expr struct {
	kind  exprKind
	path  []string
	lit   any
	left  *expr
	right *expr
}

func parseExpr(src string) (*expr, error) {
	p := &exprParser{s: src}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("invalid expression")
	}
	return e, nil
}

type exprParser struct {
	s string
	i int
}

func (p *exprParser) skip() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n') {
		p.i++
	}
}

func (p *exprParser) parseOr() (*expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		p.skip()
		if !p.has("||") {
			return left, nil
		}
		p.i += 2
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &expr{kind: exOr, left: left, right: right}
	}
}

func (p *exprParser) parseAnd() (*expr, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for {
		p.skip()
		if !p.has("&&") {
			return left, nil
		}
		p.i += 2
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = &expr{kind: exAnd, left: left, right: right}
	}
}

func (p *exprParser) parseCmp() (*expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	p.skip()
	op := ""
	switch {
	case p.has("=="):
		op = "=="
	case p.has("!="):
		op = "!="
	default:
		return left, nil
	}
	p.i += 2
	right, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	kind := exEq
	if op == "!=" {
		kind = exNe
	}
	return &expr{kind: kind, left: left, right: right}, nil
}

func (p *exprParser) parseUnary() (*expr, error) {
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == '!' && !p.has("!=") {
		p.i++
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &expr{kind: exNot, left: inner}, nil
	}
	return p.parsePrimary()
}

func (p *exprParser) parsePrimary() (*expr, error) {
	p.skip()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("invalid expression")
	}
	if p.s[p.i] == '(' {
		p.i++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("invalid expression")
		}
		p.i++
		return inner, nil
	}
	if p.s[p.i] == '"' {
		lit, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &expr{kind: exLit, lit: lit}, nil
	}
	if p.s[p.i] == '-' || unicode.IsDigit(rune(p.s[p.i])) {
		return p.parseNumber()
	}
	if isIdentStart(p.s[p.i]) {
		return p.parsePathOrKeyword()
	}
	return nil, fmt.Errorf("invalid expression")
}

func (p *exprParser) parsePathOrKeyword() (*expr, error) {
	name := p.parseIdent()
	switch name {
	case "true":
		return &expr{kind: exLit, lit: true}, nil
	case "false":
		return &expr{kind: exLit, lit: false}, nil
	case "null":
		return &expr{kind: exLit, lit: nil}, nil
	}
	path := []string{name}
	for {
		p.skip()
		if p.i < len(p.s) && p.s[p.i] == '.' {
			p.i++
			p.skip()
			if p.i >= len(p.s) || !isIdentStart(p.s[p.i]) {
				return nil, fmt.Errorf("invalid expression")
			}
			path = append(path, p.parseIdent())
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == '[' {
			p.i++
			p.skip()
			if p.i < len(p.s) && p.s[p.i] == '"' {
				s, err := p.parseString()
				if err != nil {
					return nil, err
				}
				path = append(path, s)
			} else if p.i < len(p.s) && unicode.IsDigit(rune(p.s[p.i])) {
				start := p.i
				for p.i < len(p.s) && unicode.IsDigit(rune(p.s[p.i])) {
					p.i++
				}
				path = append(path, p.s[start:p.i])
			} else {
				return nil, fmt.Errorf("invalid expression")
			}
			p.skip()
			if p.i >= len(p.s) || p.s[p.i] != ']' {
				return nil, fmt.Errorf("invalid expression")
			}
			p.i++
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == '(' {
			return nil, fmt.Errorf("invalid expression")
		}
		return &expr{kind: exPath, path: path}, nil
	}
}

func (p *exprParser) parseIdent() string {
	start := p.i
	p.i++
	for p.i < len(p.s) {
		c := p.s[p.i]
		if isIdentCont(c) {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

func (p *exprParser) parseString() (string, error) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", fmt.Errorf("invalid expression")
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		if c == '"' {
			return b.String(), nil
		}
		if c == '\\' {
			if p.i >= len(p.s) {
				return "", fmt.Errorf("invalid expression")
			}
			esc := p.s[p.i]
			p.i++
			switch esc {
			case '"', '\\':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			default:
				return "", fmt.Errorf("invalid expression")
			}
			continue
		}
		b.WriteByte(c)
	}
	return "", fmt.Errorf("invalid expression")
}

func (p *exprParser) parseNumber() (*expr, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i >= len(p.s) || !unicode.IsDigit(rune(p.s[p.i])) {
		return nil, fmt.Errorf("invalid expression")
	}
	for p.i < len(p.s) && unicode.IsDigit(rune(p.s[p.i])) {
		p.i++
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if p.i >= len(p.s) || !unicode.IsDigit(rune(p.s[p.i])) {
			return nil, fmt.Errorf("invalid expression")
		}
		for p.i < len(p.s) && unicode.IsDigit(rune(p.s[p.i])) {
			p.i++
		}
	}
	n, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		return nil, fmt.Errorf("invalid expression")
	}
	return &expr{kind: exLit, lit: n}, nil
}

func (p *exprParser) has(tok string) bool {
	return strings.HasPrefix(p.s[p.i:], tok)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}

func evalExpr(e *expr, scope map[string]any) (any, error) {
	switch e.kind {
	case exLit:
		return e.lit, nil
	case exPath:
		return lookupPath(scope, e.path)
	case exNot:
		v, err := evalExpr(e.left, scope)
		if err != nil {
			return nil, err
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("condition error")
		}
		return !b, nil
	case exAnd, exOr:
		l, err := evalExpr(e.left, scope)
		if err != nil {
			return nil, err
		}
		lb, ok := l.(bool)
		if !ok {
			return nil, fmt.Errorf("condition error")
		}
		if e.kind == exAnd && !lb {
			return false, nil
		}
		if e.kind == exOr && lb {
			return true, nil
		}
		r, err := evalExpr(e.right, scope)
		if err != nil {
			return nil, err
		}
		rb, ok := r.(bool)
		if !ok {
			return nil, fmt.Errorf("condition error")
		}
		return rb, nil
	case exEq, exNe:
		l, err := evalExpr(e.left, scope)
		if err != nil {
			return nil, err
		}
		r, err := evalExpr(e.right, scope)
		if err != nil {
			return nil, err
		}
		eq := valuesEqual(l, r)
		if e.kind == exNe {
			return !eq, nil
		}
		return eq, nil
	default:
		return nil, fmt.Errorf("invalid expression")
	}
}

func evalCond(src string, scope map[string]any) (bool, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return true, nil
	}
	if inner, ok := wholeExpr(src); ok {
		src = inner
	}
	e, err := parseExpr(src)
	if err != nil {
		return false, fmt.Errorf("condition error")
	}
	v, err := evalExpr(e, scope)
	if err != nil {
		return false, fmt.Errorf("condition error")
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("condition error")
	}
	return b, nil
}

func lookupPath(scope map[string]any, path []string) (any, error) {
	if len(path) == 0 || len(path) > 16 {
		return nil, fmt.Errorf("invalid expression")
	}
	if path[0] == "secrets" {
		if len(path) != 2 {
			return nil, fmt.Errorf("condition error")
		}
		fn, _ := scope["__secrets"].(func(string) (string, error))
		if fn == nil {
			return nil, fmt.Errorf("condition error")
		}
		return fn(path[1])
	}
	var cur any = scope
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("condition error")
		}
		next, ok := m[seg]
		if !ok {
			// Header names are matched case-insensitively.
			for k, v := range m {
				if strings.EqualFold(k, seg) {
					next = v
					ok = true
					break
				}
			}
		}
		if !ok {
			return nil, fmt.Errorf("condition error")
		}
		cur = next
	}
	return cur, nil
}

func valuesEqual(a, b any) bool {
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	if aok && bok {
		return af == bf
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && sameKind(a, b)
}

func sameKind(a, b any) bool {
	switch a.(type) {
	case string:
		_, ok := b.(string)
		return ok
	case bool:
		_, ok := b.(bool)
		return ok
	case nil:
		return b == nil
	default:
		return false
	}
}

func wholeExpr(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "${{") || !strings.HasSuffix(t, "}}") {
		return "", false
	}
	inner := strings.TrimSpace(t[len("${{") : len(t)-2])
	if strings.Contains(inner, "${{") {
		return "", false
	}
	return inner, true
}

func exprRefs(e *expr, out *[][]string) {
	if e == nil {
		return
	}
	if e.kind == exPath {
		cp := append([]string(nil), e.path...)
		*out = append(*out, cp)
	}
	exprRefs(e.left, out)
	exprRefs(e.right, out)
}

// expandValue resolves a with/env/var value. Whole-value expressions keep
// their type. A string with an embedded expression is text, and a list or
// object has no text form.
func expandValue(v any, scope map[string]any, allowEmbed bool) (any, error) {
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	if inner, whole := wholeExpr(s); whole {
		e, err := parseExpr(inner)
		if err != nil {
			return nil, fmt.Errorf("invalid expression")
		}
		return evalExpr(e, scope)
	}
	if strings.Contains(s, "${{") {
		if !allowEmbed {
			return nil, fmt.Errorf("embedded expression")
		}
		return expandText(s, scope)
	}
	return s, nil
}

func expandText(s string, scope map[string]any) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		b.WriteString(s[:i])
		s = s[i+3:]
		j := strings.Index(s, "}}")
		if j < 0 {
			return "", fmt.Errorf("invalid expression")
		}
		inner := strings.TrimSpace(s[:j])
		s = s[j+2:]
		e, err := parseExpr(inner)
		if err != nil {
			return "", fmt.Errorf("invalid expression")
		}
		v, err := evalExpr(e, scope)
		if err != nil {
			return "", err
		}
		switch v.(type) {
		case map[string]any, []any:
			return "", fmt.Errorf("invalid expression")
		case nil:
			return "", fmt.Errorf("invalid expression")
		}
		b.WriteString(fmt.Sprint(v))
	}
}
