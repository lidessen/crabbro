import { isBuiltin } from "node:module";

// A plugin may import Node built-ins that cannot touch the disk, the
// network or other processes, and nothing else. Anything not on the list is
// refused, so a built-in added in a later Node release is closed by default.
const allowed = new Set([
  "assert",
  "assert/strict",
  "buffer",
  "crypto",
  "events",
  "path",
  "path/posix",
  "process",
  "querystring",
  "stream",
  "stream/promises",
  "string_decoder",
  "timers",
  "timers/promises",
  "url",
  "util",
  "zlib",
]);

// The host's own files are trusted by exact URL, not by file name: a
// plugin's helper called worker.mjs must not inherit it.
const hostFiles = new Set(["worker.mjs", "process.mjs", "host.mjs"].map((n) => new URL(n, import.meta.url).href));

export async function resolve(specifier, context, nextResolve) {
  if (isBuiltin(specifier) && !hostFiles.has(context.parentURL || "")) {
    const name = specifier.startsWith("node:") ? specifier.slice(5) : specifier;
    if (!allowed.has(name)) {
      throw new Error("permission denied: " + specifier);
    }
  }
  return nextResolve(specifier, context);
}
