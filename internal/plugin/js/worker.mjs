import { parentPort, workerData } from "node:worker_threads";
import { pathToFileURL } from "node:url";

const pending = new Map();
let seq = 1;

function broker(op, payload) {
  const id = "b" + seq++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    parentPort.postMessage({ type: "broker", id, op, payload });
  });
}

function stripDecision(body) {
  const out = { ...(body || {}) };
  delete out.endpoint;
  delete out.baseUrl;
  delete out.apiKey;
  delete out.authorization;
  delete out.headers;
  delete out.model;
  return out;
}

function makeCtx(msg) {
  const payload = msg.payload || {};
  return {
    config: msg.config || {},
    secrets: msg.secrets || {},
    request: payload.request,
    response: payload.response,
    frame: payload.frame,
    env: msg.env || {},
    vars: payload.vars,
    decision: {
      ask: (body) => broker("decision.ask", stripDecision(body)),
    },
    history: {
      list: (q) => broker("history.list", q || {}),
      get: (id) => broker("history.get", { id }),
    },
    net: {
      fetch: (url, opts) => broker("net.fetch", { url, ...(opts || {}) }),
    },
    fs: {
      read: (path) => broker("fs.read", { path }),
      write: (path, data) => broker("fs.write", { path, data }),
    },
  };
}

function catalog(obj) {
  const out = {};
  if (!obj) return out;
  for (const [key, value] of Object.entries(obj)) {
    out[key] = {
      description: (value && (value.description || value.summary)) || "",
      effect: (value && value.effect) || "mutate",
      inputSchema: (value && (value.inputSchema || value.params)) || { type: "object" },
    };
  }
  return out;
}

function normalize(result) {
  if (!result || typeof result !== "object") return { match: true, outputs: {}, actions: [] };
  return {
    match: result.match !== false,
    outputs: result.outputs || {},
    actions: result.actions || [],
  };
}

let plugin = {};
try {
  const loaded = await import(pathToFileURL(workerData.entry).href);
  plugin = loaded.default || loaded;
} catch (err) {
  parentPort.postMessage({ type: "ready", error: String(err && err.message ? err.message : err) });
  process.exit(1);
}
parentPort.postMessage({ type: "ready" });

parentPort.on("message", async (msg) => {
  if (!msg || typeof msg !== "object") return;
  if (msg.type === "brokerResult") {
    const waiter = pending.get(msg.id);
    if (!waiter) return;
    pending.delete(msg.id);
    if (msg.error) waiter.reject(new Error(msg.error));
    else waiter.resolve(msg.payload);
    return;
  }
  try {
    if (msg.type === "describe") {
      parentPort.postMessage({
        type: "result",
        id: msg.id,
        describe: {
          uses: catalog(plugin.uses || plugin.actions),
          tools: catalog(plugin.tools || plugin.commands),
          hooks: Object.keys(plugin.hooks || {}),
          middleware: Object.keys(plugin.middleware || {}),
          match: plugin.match || null,
        },
      });
      return;
    }
    const ctx = makeCtx(msg);
    if (msg.type === "uses" || msg.type === "middleware") {
      const table = msg.type === "uses" ? plugin.uses || plugin.actions || {} : plugin.middleware || {};
      const cap = table[msg.capability];
      const fn = cap && (cap.execute || cap.handle || (typeof cap === "function" ? cap : null));
      if (!fn) throw new Error("unknown capability");
      const result = await fn(ctx, msg.with || {});
      parentPort.postMessage({
        type: "result",
        id: msg.id,
        result: normalize(result),
        effect: (cap && cap.effect) || "mutate",
      });
      return;
    }
    if (msg.type === "tool") {
      const table = plugin.tools || plugin.commands || {};
      const cap = table[msg.capability];
      const fn = cap && (cap.execute || cap.run);
      if (!fn) throw new Error("unknown tool");
      const result = await fn(ctx, msg.arguments || msg.with || {});
      parentPort.postMessage({ type: "result", id: msg.id, result: normalize(result), effect: "observe" });
      return;
    }
    if (msg.type === "hook") {
      const fn = plugin.hooks && plugin.hooks[msg.hook];
      if (!fn) {
        parentPort.postMessage({ type: "result", id: msg.id, result: { match: false, outputs: {}, actions: [] } });
        return;
      }
      const result = await fn(ctx);
      parentPort.postMessage({ type: "result", id: msg.id, result: normalize(result) });
      return;
    }
  } catch (err) {
    parentPort.postMessage({ type: "result", id: msg.id, error: String(err && err.message ? err.message : err) });
  }
});
