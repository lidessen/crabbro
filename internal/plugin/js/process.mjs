import { pathToFileURL } from "node:url";

// A process-isolated plugin speaks the same frames as a worker, over stdio.
// The host is the parent. This file is the child.

const chunks = [];
let buffered = Buffer.alloc(0);

function send(obj) {
  const body = Buffer.from(JSON.stringify(obj));
  const head = Buffer.alloc(4);
  head.writeUInt32BE(body.length);
  process.stdout.write(Buffer.concat([head, body]));
}

const entry = process.env.CRABBRO_PLUGIN_ENTRY;
const loaded = await import(pathToFileURL(entry).href);
const plugin = loaded.default || loaded;
send({ type: "ready" });

function normalize(result) {
  if (!result || typeof result !== "object") return { match: true, outputs: {}, actions: [] };
  return { match: result.match !== false, outputs: result.outputs || {}, actions: result.actions || [] };
}

async function onMessage(msg) {
  try {
    if (msg.type === "provider.auth.prepare" || msg.type === "provider.auth.refresh") {
      const auth = plugin.providers?.[msg.providerId]?.auth?.[msg.kind];
      const fn = msg.type === "provider.auth.prepare" ? auth?.prepare : auth?.refresh;
      if (typeof fn !== "function") throw new Error("unknown provider auth method");
      const result = await fn({ credentialRef: msg.credentialRef, instance: msg.instance });
      send({ type: "result", id: msg.id, result });
      return;
    }
    if (msg.type === "describe") {
      const uses = plugin.uses || plugin.actions || {};
      send({
        type: "result",
        id: msg.id,
        describe: { uses: Object.fromEntries(Object.keys(uses).map((k) => [k, { effect: (uses[k] && uses[k].effect) || "mutate" }])) },
        effect: "observe",
      });
      return;
    }
    if (msg.type === "uses") {
      const table = plugin.uses || plugin.actions || {};
      const cap = table[msg.capability];
      const fn = cap && (cap.execute || cap.handle);
      if (!fn) throw new Error("unknown capability");
      const result = await fn({ config: msg.config || {}, secrets: msg.secrets || {}, request: msg.payload && msg.payload.request, env: msg.env || {} }, msg.with || {});
      send({ type: "result", id: msg.id, result: normalize(result), effect: (cap && cap.effect) || "mutate" });
      return;
    }
  } catch (err) {
    send({ type: "result", id: msg.id, error: String(err && err.message ? err.message : err) });
  }
}

process.stdin.on("data", (chunk) => {
  buffered = Buffer.concat([buffered, chunk]);
  while (buffered.length >= 4) {
    const len = buffered.readUInt32BE(0);
    if (buffered.length < 4 + len) return;
    const body = buffered.subarray(4, 4 + len);
    buffered = buffered.subarray(4 + len);
    try {
      const msg = JSON.parse(body.toString("utf8"));
      void onMessage(msg);
    } catch {
      send({ type: "log", message: "bad frame" });
    }
  }
});
void chunks;
