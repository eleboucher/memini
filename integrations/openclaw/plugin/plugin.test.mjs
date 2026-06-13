// Run: npm install && node --test (from this directory). Not shipped by
// install.sh. The expose_tools tests load @sinclair/typebox, so install deps
// first.
import { test } from "node:test";
import assert from "node:assert/strict";
import plugin, { effectiveNamespace, meminiListPath, registerMeminiTools, resolveConfig } from "./plugin.mjs";

// fakeClient records the last memini call and returns canned responses, so the
// tool handlers can be exercised without a running server.
function fakeClient() {
  const calls = [];
  return {
    calls,
    namespace: "ns",
    baseUrl: "http://localhost:8080",
    async postJson(path, body, ns) {
      calls.push({ method: "POST", path, body, ns });
      return path.includes("search")
        ? { results: [{ memory: { content: "hit", summary: "", tier: "semantic" }, score: 0.9 }] }
        : { id: "m1" };
    },
    async getJson(path, ns) {
      calls.push({ method: "GET", path, ns });
      return { memories: [{ id: "m1", content: "c", tier: "procedural", tags: ["auth"], metadata: { category: "bug_fixes" } }] };
    },
  };
}

// collectTools runs registerMeminiTools against a fake api and returns the tools
// keyed by name, plus the options each was registered with.
async function collectTools(client, cfg) {
  const registered = [];
  const api = { registerTool: (def, opts) => registered.push({ def, opts }) };
  await registerMeminiTools(api, client, cfg);
  return {
    byName: Object.fromEntries(registered.map((r) => [r.def.name, r.def])),
    opts: Object.fromEntries(registered.map((r) => [r.def.name, r.opts])),
    order: registered.map((r) => r.def.name),
  };
}

test("per-agent isolation is on by default", () => {
  const cfg = resolveConfig(undefined);
  assert.equal(cfg.namespace_per_agent, true);
  assert.equal(cfg.namespace, "openclaw");
  // A named agent lands in its own namespace, not the shared pool.
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "openclaw-miso");
  // A session with no agent identity falls back to the shared base namespace
  // (so unattributable sessions still get memory rather than silently dropping).
  assert.equal(effectiveNamespace(cfg, {}), "openclaw");
});

test("namespace_per_agent can be explicitly disabled", () => {
  const cfg = resolveConfig({ namespace_per_agent: false });
  assert.equal(cfg.namespace_per_agent, false);
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "openclaw");
});

test("default per-agent template prefixes the configured base namespace", () => {
  const cfg = resolveConfig({ namespace: "team" });
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "team-miso");
});

test("namespace_per_agent off returns the base namespace", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: false };
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "openclaw");
});

test("default template is the bare agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "alice");
});

test("template can prefix and substitute {namespace}", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{namespace}-{agent}" };
  assert.equal(effectiveNamespace(cfg, { agent: { name: "bob" } }), "openclaw-bob");
});

test("resolves alternate event shapes", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentName: "carol" }), "carol");
  // raw session UUIDs are not identities (they would fragment namespaces);
  // only agent-keyed session keys resolve
  assert.equal(effectiveNamespace(cfg, { sessionId: "sess1" }), "ns");
  assert.equal(effectiveNamespace(cfg, { sessionId: "agent:bob:b7d2-uuid" }), "bob");
});

test("sanitizes the agent id", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentName: "My Agent/2!" }), "My-Agent-2");
});

test("falls back to base namespace when no agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, {}), "openclaw");
  assert.equal(effectiveNamespace(cfg, { agentId: "   " }), "openclaw");
});

test("skip_without_agent returns null when no agent id (per-agent mode)", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, {}), null);
});

test("skip_without_agent still resolves a present agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "alice");
});

test("ctx identity wins and session keys parse from ctx", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, {}, { agentId: "alice" }), "alice");
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "agent:carol:cron:daily" }), "carol");
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "heartbeat:gateway" }), "ns");
});

test("skip_without_agent skips gateway-level sessions but keeps agent crons", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "agent:alice:heartbeat:hourly" }), "alice");
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "heartbeat:gateway" }), null);
  assert.equal(effectiveNamespace(cfg, {}, {}), null);
});

// --- explicit tools (expose_tools) -----------------------------------------

test("expose_tools is off by default", () => {
  assert.equal(resolveConfig(undefined).expose_tools, false);
  assert.equal(resolveConfig({ expose_tools: true }).expose_tools, true);
});

test("register does not touch registerTool when expose_tools is off", async () => {
  let registered = 0;
  await plugin.register({
    pluginConfig: { enabled: true },
    registerMemoryCapability() {},
    on() {},
    logger: {},
    registerTool() {
      registered++;
    },
  });
  assert.equal(registered, 0, "no tools should register when expose_tools is off");
});

test("register wires the three tools when expose_tools is on", async () => {
  const names = [];
  await plugin.register({
    pluginConfig: { enabled: true, expose_tools: true },
    registerMemoryCapability() {},
    on() {},
    logger: { warn() {} },
    registerTool(def) {
      names.push(def.name);
    },
  });
  assert.deepEqual(names.sort(), ["memory_list", "memory_recall", "memory_remember"]);
});

test("meminiListPath builds repeatable tier/tag and escaped meta params", () => {
  assert.equal(
    meminiListPath({ tiers: ["procedural"], tags: ["auth"], metadata: { category: "bug_fixes" }, limit: 20 }),
    "/v1/memories?tier=procedural&tag=auth&meta=category%3Dbug_fixes&limit=20",
  );
  assert.equal(meminiListPath({}), "/v1/memories");
  // limit <= 0 / non-integer is omitted.
  assert.equal(meminiListPath({ limit: 0 }), "/v1/memories");
});

test("tools register as optional", async () => {
  const { opts } = await collectTools(fakeClient(), { namespace: "ns", namespace_per_agent: false });
  for (const name of ["memory_recall", "memory_list", "memory_remember"]) {
    assert.deepEqual(opts[name], { optional: true }, `${name} should be optional`);
  }
});

test("memory_recall passes query + tag/metadata filters and formats results", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace: "ns", namespace_per_agent: false });
  const out = await byName.memory_recall.execute("id", {
    query: "auth race",
    tags: ["auth"],
    metadata: { category: "bug_fixes" },
  });
  const call = client.calls.at(-1);
  assert.equal(call.method, "POST");
  assert.equal(call.path, "/v1/search");
  assert.equal(call.ns, "ns");
  assert.deepEqual(call.body, { query: "auth race", limit: 5, tags: ["auth"], metadata: { category: "bug_fixes" } });
  assert.deepEqual(JSON.parse(out.content[0].text), {
    results: [{ content: "hit", summary: "", tier: "semantic", score: 0.9 }],
  });
});

test("memory_list issues a filtered GET and maps the rows", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace: "ns", namespace_per_agent: false });
  const out = await byName.memory_list.execute("id", { tiers: ["procedural"], metadata: { category: "bug_fixes" } });
  const call = client.calls.at(-1);
  assert.equal(call.method, "GET");
  assert.equal(call.path, "/v1/memories?tier=procedural&meta=category%3Dbug_fixes&limit=20");
  const { memories } = JSON.parse(out.content[0].text);
  assert.equal(memories.length, 1);
  assert.equal(memories[0].metadata.category, "bug_fixes");
});

test("memory_remember maps category to metadata and defaults the tier", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace: "ns", namespace_per_agent: false });
  const out = await byName.memory_remember.execute("id", { content: "fact", category: "bug_fixes", tags: ["x"] });
  const call = client.calls.at(-1);
  assert.equal(call.path, "/v1/memories");
  assert.deepEqual(call.body, { content: "fact", tier: "semantic", tags: ["x"], metadata: { category: "bug_fixes" } });
  assert.deepEqual(JSON.parse(out.content[0].text), { id: "m1", success: true });
});

test("tool namespace follows per-agent resolution from ctx", async () => {
  const client = fakeClient();
  const cfg = resolveConfig({ namespace: "team", expose_tools: true });
  const { byName } = await collectTools(client, cfg);
  await byName.memory_recall.execute("id", { query: "q" }, { agentId: "miso" });
  assert.equal(client.calls.at(-1).ns, "team-miso", "per-agent ctx should scope the tool call");
});
