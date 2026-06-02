// Package rule provides a sandboxed JavaScript rule execution engine for
// evaluating user-defined proxy rules against captured HTTP requests.
//
// The engine uses Goja (github.com/dop251/goja) to execute ECMAScript 5.1
// code in an isolated runtime per evaluation. Each rule receives an immutable
// snapshot of the request context and returns a declarative result.
//
// # Security
//
// The runtime is constrained to prevent access to the filesystem, network,
// or host environment. Only standard ECMAScript built-ins are available.
// A per-rule execution timeout (default 500ms) prevents runaway scripts.
//
// # Rule Format
//
// Rules are JavaScript modules that export a default function:
//
//	export default function(ctx) {
//	  if (ctx.request.host === "example.com" && ctx.request.path === "/v1/chat") {
//	    return {
//	      match: true,
//	      actions: [
//	        {
//	          type: "mockResponse",
//	          params: { status: 200, body: JSON.stringify({ ok: true }) }
//	        }
//	      ]
//	    };
//	  }
//	  return { match: false };
//	}
//
// The engine preprocesses ES module export syntax so it works with Goja's
// ES5.1 runtime. Rules may also define a top-level function named "rule"
// instead of using export default.
//
// # Context
//
// The ctx.request object contains the following fields derived from the
// HTTP exchange:
//
//   - method (string)
//   - scheme (string)
//   - host (string)
//   - path (string)
//   - query (string)
//   - url (string)
//   - headers (object)
//   - clientAddr (string)
//   - body (string, optional — empty when not available)
//
// # Output Contract
//
// Rules must return an object with:
//
//	{ match: boolean, actions?: Array<{ type: string, params: object }> }
//
// When match is true, at least one action must be provided. Supported action
// types are defined in the shared package.
package rule
