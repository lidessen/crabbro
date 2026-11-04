export interface PluginContext {
  config: Record<string, unknown>;
  secrets: Record<string, string>;
  request?: { method?: string; host?: string; path?: string; headers?: Record<string, string>; body?: string };
  response?: { status?: number; headers?: Record<string, string>; body?: string; bodyTruncated?: boolean };
  frame?: { direction?: string; opcode?: string; payload?: string; fin?: boolean };
  env?: Record<string, string | number | boolean>;
  decision: { ask(body: { type?: "noul" | "choice"; prompt?: string; options?: string[] }): Promise<Record<string, unknown>> };
  history: { list(query?: { limit?: number }): Promise<unknown>; get(id: string): Promise<unknown> };
  net: { fetch(url: string, opts?: { method?: string }): Promise<unknown> };
  fs: { read(path: string): Promise<{ data: string }>; write(path: string, data: string): Promise<unknown> };
}

export interface PluginResult {
  match?: boolean;
  outputs?: Record<string, unknown>;
  actions?: Array<{ type: string; params?: Record<string, unknown> }>;
}

export interface Capability {
  description?: string;
  effect?: "mutate" | "observe";
  inputSchema?: Record<string, unknown>;
  execute(ctx: PluginContext, params: Record<string, unknown>): Promise<PluginResult> | PluginResult;
}

export interface Plugin {
  match?: { hosts?: string[]; methods?: string[]; pathPrefix?: string };
  hooks?: {
    onRequest?(ctx: PluginContext): Promise<PluginResult> | PluginResult;
    onResponse?(ctx: PluginContext): Promise<PluginResult> | PluginResult;
    onWebSocketFrame?(ctx: PluginContext): Promise<PluginResult> | PluginResult;
  };
  uses?: Record<string, Capability>;
  tools?: Record<string, Capability>;
  middleware?: Record<string, Capability>;
}
