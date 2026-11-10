// Tries what the loader and preload are meant to stop. Reports what got through.
const through = [];
for (const name of ["node:fs", "fs", "node:child_process", "node:net", "node:vm", "node:worker_threads", "node:os", "node:http", "node:https", "node:dgram", "node:dns", "node:tls", "node:http2", "node:module"]) {
  try {
    await import(name);
    through.push("import " + name);
  } catch {
    /* refused, as it should be */
  }
}
try {
  const h = await import("./worker.mjs"); // a helper named like a host file
  through.push("helper worker.mjs: " + h.ok);
} catch {
  /* refused */
}
for (const name of ["binding", "dlopen", "getBuiltinModule", "kill", "abort"]) {
  if (typeof process[name] === "function") through.push("process." + name);
}
if (typeof globalThis.fetch === "function") through.push("fetch");
if (typeof globalThis.WebSocket === "function") through.push("WebSocket");
export default { uses: { probe: { async execute() { return { outputs: { through } }; } } } };
