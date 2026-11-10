import { register } from "node:module";

register(new URL("./loader.mjs", import.meta.url).href, import.meta.url);

// What the loader cannot see: globals and process methods that reach the
// network, other built-ins, native code or other processes. A plugin gets
// the broker (ctx.net, ctx.fs) instead.
globalThis.fetch = undefined;
globalThis.WebSocket = undefined;
globalThis.EventSource = undefined;
for (const name of ["binding", "_linkedBinding", "dlopen", "getBuiltinModule", "kill", "abort", "reallyExit", "_kill"]) {
  try {
    Object.defineProperty(process, name, { value: undefined, configurable: false, writable: false });
  } catch {
    /* a property this Node does not have, or cannot be changed */
  }
}
