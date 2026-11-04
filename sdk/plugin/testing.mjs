// runHook evaluates a plugin in-process. It does not start the daemon.
export async function runHook(plugin, fixture, phase) {
  const ctx = {
    config: {},
    secrets: {},
    request: fixture && fixture.request,
    response: fixture && fixture.response,
    frame: fixture && fixture.frame,
    decision: { ask: async () => ({}) },
    history: { list: async () => [], get: async () => null },
    net: { fetch: async () => { throw new Error("net is brokered"); } },
    fs: { read: async () => { throw new Error("fs is brokered"); }, write: async () => { throw new Error("fs is brokered"); } },
  };
  const hook = plugin.hooks && plugin.hooks[phase];
  if (!hook) return { match: false, actions: [] };
  return hook(ctx);
}
