// Run: node --test (from this directory). Imports the built dist/index.js as
// a regression contract against the legacy plugin.legacy/plugin.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import plugin, {
  detectSystemKind,
  effectiveNamespace,
  meminiListPath,
  registerMeminiTools,
  resolveConfig,
  sessionIdentity,
  shouldSkipSystemTurn,
  stripRuntimePreambles,
} from "../dist/index.js";

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

// --- skip_system_turns ------------------------------------------------------

test("skip_system_turns defaults off with the standard kinds", () => {
  const cfg = resolveConfig(undefined);
  assert.equal(cfg.skip_system_turns, false);
  assert.deepEqual(cfg.system_kinds, ["cron", "heartbeat", "scheduled", "schedule"]);
});

test("system_kinds can be overridden and is lowercased", () => {
  const cfg = resolveConfig({ system_kinds: ["Poll", "TICK"] });
  assert.deepEqual(cfg.system_kinds, ["poll", "tick"]);
});

test("detectSystemKind reads explicit fields, session keys, and bracket markers", () => {
  // explicit kind/trigger on ctx or event
  assert.equal(detectSystemKind({}, { kind: "scheduled" }), "scheduled");
  assert.equal(detectSystemKind({ trigger: "cron" }, {}), "cron");
  // session key segments — even when agent-attributed
  assert.equal(detectSystemKind({}, { sessionKey: "agent:carol:cron:daily" }), "cron");
  assert.equal(detectSystemKind({}, { sessionKey: "agent:alice:heartbeat:hourly" }), "heartbeat");
  // leading bracket marker on the turn text
  assert.equal(detectSystemKind({}, {}, "[OpenClaw heartbeat poll]"), "heartbeat");
  assert.equal(detectSystemKind({}, {}, "[cron:daily (status sweep)] do the thing"), "cron");
  // user-driven turns are not system turns
  assert.equal(detectSystemKind({}, { sessionKey: "agent:bob:b7d2-uuid" }, "fix the bug"), "");
  // an agent id that merely contains a kind substring is not a system turn
  assert.equal(detectSystemKind({}, { sessionKey: "agent:concord:b7d2" }), "");
  // a user quoting a marker mid-message is ignored (marker must lead)
  assert.equal(detectSystemKind({}, {}, "see the [cron] docs"), "");
});

test("shouldSkipSystemTurn gates only when enabled and matched", () => {
  const off = resolveConfig({});
  assert.equal(shouldSkipSystemTurn(off, {}, { sessionKey: "agent:carol:cron:daily" }), false);
  const on = resolveConfig({ skip_system_turns: true });
  assert.equal(shouldSkipSystemTurn(on, {}, { sessionKey: "agent:carol:cron:daily" }), true);
  assert.equal(shouldSkipSystemTurn(on, {}, {}, "[OpenClaw heartbeat poll]"), true);
  // normal agent turn is kept
  assert.equal(shouldSkipSystemTurn(on, {}, { agentId: "carol" }, "fix the bug"), false);
});

test("shouldSkipSystemTurn honors a custom system_kinds set", () => {
  const on = resolveConfig({ skip_system_turns: true, system_kinds: ["poll"] });
  assert.equal(shouldSkipSystemTurn(on, { kind: "poll" }, {}), true);
  // a default kind no longer matches once the set is overridden
  assert.equal(shouldSkipSystemTurn(on, {}, { sessionKey: "agent:carol:cron:daily" }), false);
});

// --- explicit tools (expose_tools) -----------------------------------------

test("expose_tools is off by default", () => {
  assert.equal(resolveConfig(undefined).expose_tools, false);
  assert.equal(resolveConfig({ expose_tools: true }).expose_tools, true);
});

// OpenClaw rejects a register() that returns a thenable ("plugin register must be
// synchronous"), so an async register fails the whole plugin — memory slot included
// (the >=0.2.8 regression, eleboucher/memini#17). Tools must register synchronously
// inside register(): the guarded api turns any deferred registerTool into a no-op.
test("register is synchronous even with expose_tools on (OpenClaw contract)", () => {
  const names = [];
  const result = plugin.register({
    pluginConfig: { enabled: true, expose_tools: true },
    registerMemoryCapability() {}, registerHook() {},
    on() {},
    logger: { warn() {} },
    registerTool(def) {
      names.push(def.name);
    },
  });
  assert.equal(result, undefined, "register must not return a Promise");
  assert.notEqual(
    Object.getPrototypeOf(plugin.register).constructor.name,
    "AsyncFunction",
    "register must not be an async function",
  );
  // Tools are wired before register returns — not deferred (the guarded api would drop them).
  assert.deepEqual(names.sort(), ["memory_list", "memory_recall", "memory_remember"]);
});

test("register does not touch registerTool when expose_tools is off", async () => {
  let registered = 0;
  await plugin.register({
    pluginConfig: { enabled: true },
    registerMemoryCapability() {}, registerHook() {},
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
    registerMemoryCapability() {}, registerHook() {},
    on() {},
    logger: { warn() {} },
    registerTool(def) {
      names.push(def.name);
    },
  });
  assert.deepEqual(names.sort(), ["memory_list", "memory_recall", "memory_remember"]);
});

// Production OpenClaw's api.registerHook(name, handler) rejects the positional
// form with "hook registration missing name". That throw must not abort
// register() and lose the memory hooks (eleboucher/memini#26) — they register
// via api.on, which production exposes.
test("register survives a throwing api.registerHook and still wires hooks via api.on", async () => {
  const hooks = {};
  let threw = false;
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {},
      registerHook() {
        throw new Error("hook registration missing name");
      },
      on(name, handler) {
        hooks[name] = handler;
      },
      logger: { warn() {} },
      registerTool() {},
    });
  } catch {
    threw = true;
  }
  assert.equal(threw, false, "register must not throw when registerHook rejects");
  assert.equal(typeof hooks.before_prompt_build, "function", "recall hook wired via api.on");
  assert.equal(typeof hooks.agent_end, "function", "capture hook wired via api.on");
});

// OpenClaw requires every runtime api.registerTool(...) name to be declared in
// the manifest's contracts.tools, or tool discovery can't route to this plugin.
// Keep the manifest and the registered tools in lockstep.
test("manifest contracts.tools matches the registered tool names", async () => {
  const manifest = JSON.parse(readFileSync(new URL("../openclaw.plugin.json", import.meta.url)));
  const declared = manifest.contracts?.tools ?? [];
  const { order } = await collectTools(fakeClient(), { namespace: "ns", namespace_per_agent: false });
  assert.deepEqual([...declared].sort(), [...order].sort(), "contracts.tools must list exactly the registered tools");
});

test("resolveConfig defaults recall_limit to 3", () => {
  const cfg = resolveConfig(undefined);
  assert.equal(cfg.recall_limit, 3);
});

test("resolveConfig honors explicit recall_limit", () => {
  assert.equal(resolveConfig({ recall_limit: 2 }).recall_limit, 2);
});

test("resolveConfig falls back when recall_limit is zero/negative", () => {
  assert.equal(resolveConfig({ recall_limit: 0 }).recall_limit, 3, "0 falls back to default");
  assert.equal(resolveConfig({ recall_limit: -1 }).recall_limit, 3, "negative falls back to default");
});

test("recall sends recall_limit and no min_score on /v1/search", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    return {
      ok: true,
      async json() {
        return { results: [{ memory: { summary: "x", tier: "semantic" }, score: 0.9 }] };
      },
      async text() { return ""; },
    };
  };
  try {
    await plugin.register({
      pluginConfig: {
        enabled: true,
        namespace_per_agent: false,
        recall_limit: 2,
      },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    await hooks.before_prompt_build({ prompt: "q" }, {});
    const search = JSON.parse(requests.find((r) => r.url.endsWith("/v1/search")).init.body);
    assert.equal(search.limit, 2);
    assert.equal(search.min_score, undefined, "the plugin no longer sends a relevance-score floor");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// before_prompt_build fires on every step of a turn; an unchanged query returns
// the same memories, so without dedup the same block is re-injected on every
// tool call (eleboucher/memini#21). The same session must only be shown a given
// memory once; a different session is unaffected.
test("recall does not re-inject memories already shown in the same session (#21)", async () => {
  const hooks = {};
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    async json() {
      return {
        results: [
          { memory: { id: "m1", summary: "alpha", tier: "semantic" }, score: 0.9 },
          { memory: { id: "m2", summary: "beta", tier: "semantic" }, score: 0.8 },
        ],
      };
    },
    async text() { return ""; },
  });
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    const ev = (sessionId) => ({ prompt: "q", session: { id: sessionId } });
    const first = await hooks.before_prompt_build(ev("s1"), {});
    assert.match(first.prependContext, /alpha/);
    assert.match(first.prependContext, /beta/);
    // Same session, same query (a later tool-call step): both already shown -> no re-injection.
    const second = await hooks.before_prompt_build(ev("s1"), {});
    assert.equal(second, undefined, "already-shown memories must not be re-injected");
    // A different session still sees them.
    const other = await hooks.before_prompt_build(ev("s2"), {});
    assert.match(other.prependContext, /alpha/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("recall uses before_prompt_build, not the deprecated before_agent_start", async () => {
  const hooks = {};
  await plugin.register({
    pluginConfig: { enabled: true, namespace_per_agent: false },
    registerMemoryCapability() {}, registerHook() {},
    on(name, handler) {
      hooks[name] = handler;
    },
    logger: {},
    registerTool() {},
  });
  assert.ok(hooks.before_prompt_build, "recall should register on before_prompt_build");
  assert.ok(hooks.agent_end, "capture should register on agent_end");
  assert.equal(hooks.before_agent_start, undefined, "must not use the deprecated before_agent_start hook");
});

test("recall searches memini and prepends results; capture writes the episodic turn", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    const body = String(url).endsWith("/v1/search")
      ? { results: [{ memory: { summary: "prior fact", tier: "semantic" }, score: 0.9 }] }
      : { id: "m1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  };
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) {
        hooks[name] = handler;
      },
      logger: { warn() {} },
      registerTool() {},
    });

    const recall = await hooks.before_prompt_build({ prompt: "how did we fix auth?" }, {});
    assert.match(recall.prependContext, /prior fact/);
    const search = requests.find((r) => r.url.endsWith("/v1/search"));
    assert.equal(JSON.parse(search.init.body).query, "how did we fix auth?");

    // agent_end is the raw-conversation hook: when the host grants conversation
    // access, event.messages is present and the turn is captured as episodic.
    await hooks.agent_end(
      {
        success: true,
        messages: [
          { role: "user", content: "q" },
          { role: "assistant", content: "a" },
        ],
      },
      {},
    );
    const write = requests.find((r) => r.url.endsWith("/v1/memories"));
    assert.ok(write, "capture should POST /v1/memories");
    const body = JSON.parse(write.init.body);
    assert.equal(body.tier, "episodic");
    assert.match(body.content, /q/);
    assert.match(body.content, /a/);
    assert.doesNotMatch(body.content, /User:|Assistant:/);
    assert.equal(body.metadata.format, "turn");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("sessionIdentity prefers session ids and sanitizes them; empty without one", () => {
  assert.equal(sessionIdentity({}, { sessionId: "sess-abc" }), "sess-abc");
  assert.equal(sessionIdentity({ sessionKey: "agent:bob:run/42" }, {}), "agent-bob-run-42");
  assert.equal(sessionIdentity({}, { runId: "r1" }), "r1");
  assert.equal(sessionIdentity({}, {}), "");
});

test("auto-recall excludes the current session's own captures; capture tags the session", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    const body = String(url).endsWith("/v1/search") ? { results: [] } : { id: "m1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  };
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) {
        hooks[name] = handler;
      },
      logger: { warn() {} },
      registerTool() {},
    });
    const ctx = { sessionId: "sess-abc" };

    await hooks.before_prompt_build({ prompt: "how did we fix auth?" }, ctx);
    const search = JSON.parse(requests.find((r) => r.url.endsWith("/v1/search")).init.body);
    assert.deepEqual(search.exclude_metadata, { session_id: "sess-abc" });

    await hooks.agent_end(
      {
        success: true,
        messages: [
          { role: "user", content: "q" },
          { role: "assistant", content: "a" },
        ],
      },
      ctx,
    );
    const write = JSON.parse(requests.find((r) => r.url.endsWith("/v1/memories")).init.body);
    assert.equal(write.metadata.session_id, "sess-abc");
    assert.equal(write.metadata.source, "openclaw");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("without a session id, auto-recall and capture stay unscoped (back-compat)", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    const body = String(url).endsWith("/v1/search") ? { results: [] } : { id: "m1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  };
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) {
        hooks[name] = handler;
      },
      logger: { warn() {} },
      registerTool() {},
    });

    await hooks.before_prompt_build({ prompt: "anything" }, {});
    const search = JSON.parse(requests.find((r) => r.url.endsWith("/v1/search")).init.body);
    assert.equal(search.exclude_metadata, undefined);

    await hooks.agent_end(
      { success: true, messages: [{ role: "user", content: "q" }, { role: "assistant", content: "a" }] },
      {},
    );
    const write = JSON.parse(requests.find((r) => r.url.endsWith("/v1/memories")).init.body);
    assert.equal(write.metadata.session_id, undefined);
  } finally {
    globalThis.fetch = realFetch;
  }
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

test("plugin.yaml version matches package.json", () => {
  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
  const yaml = readFileSync(new URL("../plugin.yaml", import.meta.url), "utf8");
  const m = yaml.match(/^version:\s*(.+)$/m);
  assert.ok(m, "plugin.yaml must contain a version key");
  assert.equal(m[1].trim().replace(/["']/g, ""), pkg.version, "plugin.yaml version must match package.json");
});

test("agent_end still captures when success is false, tagging metadata.failed", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    return { ok: true, async json() { return { id: "m1" }; }, async text() { return ""; } };
  };
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: {},
      registerTool() {},
    });

    await hooks.agent_end(
      {
        success: false,
        messages: [
          { role: "user", content: "q" },
          { role: "assistant", content: "error output" },
        ],
      },
      {},
    );
    const write = requests.find((r) => r.url.endsWith("/v1/memories"));
    assert.ok(write, "should still capture failed runs");
    const body = JSON.parse(write.init.body);
    assert.equal(body.tier, "episodic");
    assert.equal(body.metadata.failed, true, "failed runs must be tagged");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memory_remember rejects unknown tier and falls back to semantic", async () => {
  const client = fakeClient();
  const cfg = resolveConfig({ namespace: "ns", expose_tools: true });
  const { byName } = await collectTools(client, cfg);
  await byName.memory_remember.execute("id", { content: "fact", tier: "bogus" });
  const call = client.calls.at(-1);
  assert.equal(call.body.tier, "semantic", "unknown tier must fall back to semantic");
});

test("stripRuntimePreambles drops a leading untrusted-metadata block, keeps the message", () => {
  const input = [
    "Conversation info (untrusted metadata):",
    "```json",
    '{ "chat_id": "c1", "sender": "alice" }',
    "```",
    "",
    "What is the deploy status?",
  ].join("\n");
  assert.equal(stripRuntimePreambles(input), "What is the deploy status?");
});

test("stripRuntimePreambles drops multiple stacked metadata blocks", () => {
  const input = [
    "Conversation info (untrusted metadata):",
    "```json",
    '{ "chat_id": "c1" }',
    "```",
    "",
    "Sender (untrusted metadata):",
    "```json",
    '{ "name": "alice" }',
    "```",
    "",
    "Real question here",
  ].join("\n");
  assert.equal(stripRuntimePreambles(input), "Real question here");
});

test("stripRuntimePreambles leaves a normal message untouched", () => {
  assert.equal(stripRuntimePreambles("Just a normal question"), "Just a normal question");
});

test("stripRuntimePreambles returns empty when the turn is only metadata", () => {
  const input = [
    "Conversation info (untrusted metadata):",
    "```json",
    '{ "chat_id": "c1" }',
    "```",
  ].join("\n");
  assert.equal(stripRuntimePreambles(input), "");
});
