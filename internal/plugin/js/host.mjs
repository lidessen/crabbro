import net from "node:net";
import { Worker } from "node:worker_threads";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const sockPath = process.argv[2];
const heartbeatMs = Number(process.env.CHILING_PLUGIN_HEARTBEAT_MS || 2000);
const runtimeDir = new URL(".", import.meta.url);

const workers = new Map();

function encode(obj) {
  const body = Buffer.from(JSON.stringify(obj));
  const head = Buffer.alloc(4);
  head.writeUInt32BE(body.length);
  return Buffer.concat([head, body]);
}

let socket;
function send(obj) {
  if (!socket || socket.destroyed) return;
  socket.write(encode(obj), (err) => {
    if (err && socket) socket = null;
  });
}

function workerFor(msg) {
  return workers.get(msg.package);
}

function post(msg) {
  const slot = workerFor(msg);
  if (!slot || !slot.worker) {
    send({ type: "result", id: msg.id, error: "plugin is not active" });
    return;
  }
  if (slot.poisoned) {
    send({ type: "result", id: msg.id, error: "plugin is poisoned" });
    return;
  }
  slot.lastUsed = Date.now();
  if (slot.child) {
    slot.child.stdin.write(encode(msg));
    return;
  }
  slot.worker.postMessage(msg);
}

function startWorker(msg) {
  const existing = workers.get(msg.package);
  if (existing && !existing.poisoned) {
    send({ type: "result", id: msg.id, ok: true });
    return;
  }
  const heap = Number(msg.heapMb || 64);
  const preload = new URL("./preload.mjs", runtimeDir).href;
  if (msg.isolation === "process") {
    const child = spawn(process.execPath, ["--import", preload, fileURLToPath(new URL("./process.mjs", runtimeDir))], {
      stdio: ["pipe", "pipe", "pipe"],
      env: {
        PATH: process.env.PATH || "",
        HOME: process.env.HOME || "",
        CHILING_PLUGIN_ENTRY: msg.entry,
      },
    });
    const slot = { child, packageName: msg.package, lastUsed: Date.now(), poisoned: false, buf: Buffer.alloc(0) };
    workers.set(msg.package, slot);
    child.stdout.on("data", (chunk) => readChild(slot, chunk));
    child.on("exit", () => {
      if (workers.get(msg.package) === slot) workers.delete(msg.package);
    });
    send({ type: "result", id: msg.id, ok: true });
    return;
  }
  const worker = new Worker(fileURLToPath(new URL("./worker.mjs", runtimeDir)), {
    execArgv: ["--import", preload],
    resourceLimits: { maxOldGenerationSizeMb: heap },
    workerData: { entry: msg.entry, packageName: msg.package },
    env: { PATH: process.env.PATH || "", HOME: process.env.HOME || "" },
  });
  const slot = { worker, packageName: msg.package, lastUsed: Date.now(), poisoned: false };
  workers.set(msg.package, slot);
  worker.on("message", (fromWorker) => {
    if (!fromWorker) return;
    if (fromWorker.type === "broker") {
      send({
        type: "broker",
        id: fromWorker.id,
        package: msg.package,
        op: fromWorker.op,
        payload: fromWorker.payload,
      });
      return;
    }
    if (fromWorker.type === "result" || fromWorker.type === "ready") {
      send(fromWorker.type === "ready" ? { type: "log", package: msg.package, message: fromWorker.error || "ready" } : { ...fromWorker, package: msg.package });
    }
  });
  worker.on("error", (err) => {
    send({ type: "log", package: msg.package, level: "error", message: String(err && err.message ? err.message : err) });
  });
  worker.on("exit", (code) => {
    if (workers.get(msg.package) === slot) {
      slot.exited = true;
      if (slot.terminating) workers.delete(msg.package);
    }
    send({ type: "log", package: msg.package, message: "exit " + code });
  });
  send({ type: "result", id: msg.id, ok: true });
}

function readChild(slot, chunk) {
  slot.buf = Buffer.concat([slot.buf, chunk]);
  while (slot.buf.length >= 4) {
    const len = slot.buf.readUInt32BE(0);
    if (slot.buf.length < 4 + len) return;
    const body = slot.buf.subarray(4, 4 + len).toString("utf8");
    slot.buf = slot.buf.subarray(4 + len);
    try {
      const msg = JSON.parse(body);
      send(msg);
    } catch {
      /* ignore a partial log line */
    }
  }
}

function onDaemon(msg) {
  if (!msg || typeof msg !== "object") return;
  switch (msg.type) {
    case "hello":
      send({ type: "hello", id: msg.id, protocol: "1", api: "1.0.0" });
      return;
    case "activate":
      try {
        startWorker(msg);
      } catch (err) {
        send({ type: "result", id: msg.id, error: String(err && err.message ? err.message : err) });
      }
      return;
    case "uses":
    case "tool":
    case "hook":
    case "middleware":
    case "describe":
      post(msg);
      return;
    case "brokerResult": {
      const slot = workers.get(msg.package);
      if (slot && slot.worker) slot.worker.postMessage(msg);
      return;
    }
    case "terminate": {
      const slot = workers.get(msg.package);
      if (!slot) {
        send({ type: "result", id: msg.id, ok: true });
        return;
      }
      slot.terminating = true;
      if (slot.child) slot.child.kill("SIGKILL");
      else slot.worker.terminate();
      setTimeout(() => {
        if (slot.worker && !slot.exited) {
          slot.poisoned = true;
          send({ type: "log", package: msg.package, level: "error", message: "poisoned" });
        }
      }, 1000);
      send({ type: "result", id: msg.id, ok: true });
      return;
    }
    case "deactivate": {
      const slot = workers.get(msg.package);
      if (slot) {
        slot.terminating = true;
        if (slot.child) slot.child.kill("SIGTERM");
        else void slot.worker.terminate();
        workers.delete(msg.package);
      }
      send({ type: "result", id: msg.id, ok: true });
      return;
    }
    case "shutdown":
      for (const slot of workers.values()) {
        if (slot.child) slot.child.kill("SIGTERM");
        else void slot.worker.terminate();
      }
      send({ type: "result", id: msg.id, ok: true });
      setTimeout(() => process.exit(0), 20);
      return;
    default:
      return;
  }
}

const server = net.createServer((conn) => {
  socket = conn;
  let buf = Buffer.alloc(0);
  conn.on("error", () => {});
  conn.on("data", (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    while (buf.length >= 4) {
      const len = buf.readUInt32BE(0);
      if (len <= 0 || len > 8 * 1024 * 1024) {
        conn.destroy();
        return;
      }
      if (buf.length < 4 + len) return;
      const body = buf.subarray(4, 4 + len).toString("utf8");
      buf = buf.subarray(4 + len);
      try {
        onDaemon(JSON.parse(body));
      } catch (err) {
        send({ type: "log", level: "error", message: "bad frame" });
      }
    }
  });
});

server.listen(sockPath);
setInterval(() => {
  const rss = process.memoryUsage().rss;
  send({ type: "heartbeat", rssBytes: rss, workers: workers.size });
}, heartbeatMs).unref();

// Drop idle workers. The daemon also stops the host after its own grace period.
setInterval(() => {
  const now = Date.now();
  for (const [name, slot] of workers) {
    if (now - slot.lastUsed > 30_000) {
      slot.terminating = true;
      if (slot.child) slot.child.kill("SIGTERM");
      else void slot.worker.terminate();
      workers.delete(name);
      send({ type: "log", package: name, message: "idle" });
    }
  }
}, 5_000).unref();

void require;
