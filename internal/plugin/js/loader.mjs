const blocked = new Set([
  "fs",
  "node:fs",
  "fs/promises",
  "node:fs/promises",
  "net",
  "node:net",
  "child_process",
  "node:child_process",
  "vm",
  "node:vm",
  "worker_threads",
  "node:worker_threads",
]);

function fromHost(parent) {
  if (!parent) return false;
  return parent.endsWith("/worker.mjs") || parent.endsWith("/process.mjs") || parent.endsWith("/host.mjs");
}

export async function resolve(specifier, context, nextResolve) {
  if (blocked.has(specifier) && !fromHost(context.parentURL || "")) {
    throw new Error("permission denied: " + specifier);
  }
  return nextResolve(specifier, context);
}
