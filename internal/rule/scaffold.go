package rule

import (
	"fmt"
	"strings"

	"github.com/lidessen/chiling/internal/shared"
)

// Scaffold builds a JavaScript rule that matches a captured request.
// action is one of mock, block, header, rewrite, tag. The script uses the
// {type, params} shape the engine validates.
func Scaffold(req *shared.Request, action, responseBody, contentType string) string {
	if req == nil {
		req = &shared.Request{}
	}
	if contentType == "" {
		contentType = "application/json"
	}
	var b strings.Builder
	b.WriteString("export default function rule(ctx) {\n")
	if req.Host != "" {
		fmt.Fprintf(&b, "  if (ctx.request.host !== %q) return { match: false };\n", req.Host)
	}
	if req.Method != "" {
		fmt.Fprintf(&b, "  if (ctx.request.method !== %q) return { match: false };\n", req.Method)
	}
	if req.Path != "" {
		fmt.Fprintf(&b, "  if (ctx.request.path !== %q) return { match: false };\n", req.Path)
	}
	b.WriteString("  return {\n    match: true,\n    actions: [\n      ")
	switch action {
	case "block":
		b.WriteString(`{ type: "block", params: { status: 403, body: "blocked" } }`)
	case "header":
		b.WriteString(`{ type: "setRequestHeader", params: { name: "X-Chiling", value: "1" } }`)
	case "rewrite":
		target := req.URL
		if target == "" {
			target = "https://example.test/rewritten"
		}
		fmt.Fprintf(&b, `{ type: "rewriteURL", params: { url: %q } }`, target)
	case "tag":
		b.WriteString(`{ type: "tag", params: { tags: ["from-capture"] } }`)
	default:
		body := responseBody
		if body == "" {
			body = `{"ok":true}`
		}
		fmt.Fprintf(&b, `{ type: "mockResponse", params: { status: %d, headers: { "content-type": %q }, body: %q } }`, statusOr(req.Status, 200), contentType, body)
	}
	b.WriteString("\n    ]\n  };\n}\n")
	return b.String()
}

func statusOr(status, fallback int) int {
	if status < 100 || status > 599 {
		return fallback
	}
	return status
}
