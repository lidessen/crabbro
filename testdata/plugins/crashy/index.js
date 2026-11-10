export default {
  uses: {
    die: { async execute() { setTimeout(() => { throw new Error("boom"); }, 0); return { outputs: {} }; } },
    slow: { async execute() { await new Promise((r) => setTimeout(r, 400)); return { outputs: { slow: true } }; } },
    ok: { async execute() { return { outputs: { ok: true } }; } },
  },
};
