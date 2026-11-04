import { createHmac } from "node:crypto";

const plugin = {
  uses: {
    sign: {
      effect: "mutate",
      description: "HMAC sign of method and path",
      inputSchema: {
        type: "object",
        properties: { algorithm: { type: "string" } },
      },
      execute(ctx) {
        const secret = (ctx.secrets && ctx.secrets.partner_hmac) || "";
        const method = (ctx.request && ctx.request.method) || "";
        const path = (ctx.request && ctx.request.path) || "";
        const header = "v1=" + createHmac("sha256", secret).update(method + " " + path).digest("hex");
        return { outputs: { ok: true, header } };
      },
    },
  },
  tools: {
    ping: {
      description: "liveness",
      effect: "observe",
      async execute() {
        return { outputs: { ok: true } };
      },
    },
  },
};

export default plugin;
