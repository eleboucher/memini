// Run: node --test (from this directory), after `pnpm run build`.
//
// Imports the built dist/index.js as a regression contract against the legacy
// plugin.legacy/plugin.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execSync } from "node:child_process";
import plugin, {
  createSessionContext,
  detectSystemKind,
  effectiveNamespace,
  gatewayFacts,
  meminiListPath,
  registerMeminiCommands,
  registerMeminiTools,
  resolveBaseNamespace,
  resolveConfig,
  sessionIdentity,
  sessionLive,
  shouldSkipSystemTurn,
  stripRuntimePreambles,
} from "../dist/index.js";

// The namespace chain now honors MEMINI_NAMESPACE only (no more per-project
// override file). A developer who exports it would otherwise fail every
// default-namespace assertion below.
delete process.env.MEMINI_NAMESPACE;

// Every `plugin.register()` call below now creates a SessionContext that
// attempts a live handshake (POST /v1/handshake) the first time a hook or tool
// actually runs. Unless a test cares about the handshake itself, its fetch
// mock should make that call fail fast so effectiveConfig falls back to the
// synchronous resolveConfig baseline these tests otherwise exercise —
// matching how this plugin behaved before the handshake existed at all.
function withHandshakeFailure(fetchImpl) {
  return async (url, init) => {
    if (String(url).endsWith("/v1/handshake")) {
      return { ok: false, status: 500, async json() { return {}; }, async text() { return ""; } };
    }
    return fetchImpl(url, init);
  };
}

function tmpProject(withGit = true) {
  const dir = mkdtempSync(join(tmpdir(), "openclaw-memini-regression-proj-"));
  if (withGit) {
    execSync("git init -q", { cwd: dir });
    execSync("git remote add origin https://example.com/acme/widget.git", { cwd: dir });
  }
  return dir;
}

function fakeHandshake(overrides = {}) {
  return {
    namespace: "acme/widget",
    namespace_source: "declared",
    identity: { authenticated: false },
    settings: {},
    settings_sources: {},
    read_set: [],
    server: { version: "test", default_namespace: "default" },
    ...overrides,
  };
}

// fakeClient records the last memini call and returns canned responses, so the
// tool handlers can be exercised without a running server.
function fakeClient() {
  const calls = [];
  return {
    calls,
    baseUrl: "http://localhost:8080",
    async postJson(path, body, ns) {
      calls.push({ method: "POST", path, body, ns });
      return path.includes("search")
        ? { results: [{ memory: { id: "m1", content: "hit", summary: "", tier: "semantic" }, score: 0.9 }] }
        : { id: "m1" };
    },
    // The write tools use postJsonResult so a rejected write reaches the model
    // with the server's own error text (see MeminiClient.postJsonResult).
    async postJsonResult(path, body, ns) {
      calls.push({ method: "POST", path, body, ns });
      if (body?.visibility === "widgets") {
        return { ok: false, error: 'remember: visibility "widgets" not in scope; valid: project, personal, acme' };
      }
      return { ok: true, data: { id: "m1" } };
    },
    async getJson(path, ns) {
      calls.push({ method: "GET", path, ns });
      if (path.includes("briefing")) {
        return {
          namespace: "ns",
          scope_header: "Scope: ns ← acme(4) ← personal(2)",
          pinned: [{ memory: { id: "p1", content: "pinned", tier: "semantic" } }],
          facts: [{ memory: { id: "f1", content: "org", tier: "semantic", namespace: "acme" }, from: "acme" }],
        };
      }
      return { memories: [{ id: "m1", content: "c", tier: "procedural", tags: ["auth"], metadata: { category: "bug_fixes" } }] };
    },
    async deleteJson(path, ns) {
      calls.push({ method: "DELETE", path, ns });
      return { ok: true };
    },
  };
}

// collectTools runs registerMeminiTools against a fake api and materializes the
// tools by invoking the registered factory with `ctx` — the per-agent
// OpenClawPluginToolContext OpenClaw hands the factory. `hs` is what the
// session's memo resolves to instantly (no real handshake network call).
async function collectTools(client, pluginConfig, ctx = {}, hs = fakeHandshake()) {
  const registered = [];
  const api = { logger: { warn() {} }, registerTool: (factory, opts) => registered.push({ factory, opts }) };
  const sessionCtx = createSessionContext(pluginConfig, process.env, tmpdir());
  sessionCtx.memo = { get: async () => hs, invalidate() {} };
  await registerMeminiTools(api, client, sessionCtx);
  const reg = registered[0];
  const tools = reg ? [].concat(reg.factory(ctx)) : [];
  return {
    byName: Object.fromEntries(tools.map((t) => [t.name, t])),
    opts: reg?.opts,
    order: tools.map((t) => t.name),
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

// The namespace chain, from the shipped bundle. The default must not move: this
// is a gateway harness, and deriving a namespace from its cwd (or letting the
// default drift) would silently relocate every existing install's memory.
// There is no more per-project override file: MEMINI_NAMESPACE > config >
// "openclaw" is the whole synchronous chain. A live handshake, layered on top
// by effectiveConfig, can insert a server-side pin between env and config
// (full precedence: env > pin > declared/config > "openclaw").
test("the base namespace chain is MEMINI_NAMESPACE > config > openclaw", () => {
  assert.deepEqual(resolveBaseNamespace(undefined, {}), { namespace: "openclaw", source: "default" });
  assert.deepEqual(resolveBaseNamespace({ namespace: "cfg" }, {}), { namespace: "cfg", source: "config" });
  assert.deepEqual(resolveBaseNamespace({ namespace: "cfg" }, { MEMINI_NAMESPACE: "pin" }), {
    namespace: "pin",
    source: "env",
  });
});

// The pin loop, from the shipped bundle: memini:namespace PUTs a pin keyed by
// the same daemon-cwd toplevel_path every handshake sends, the write drops the
// memo, and the next handshake resolves server:pin over the declared value.
test("a pin written via memini:namespace is resolved by the gateway's own next handshake", async () => {
  const cwd = mkdtempSync(join(tmpdir(), "openclaw-memini-pin-"));
  const realFetch = globalThis.fetch;
  let pinned = false;
  globalThis.fetch = async (url, init) => {
    const u = String(url);
    if (u.endsWith("/v1/pins") && init?.method === "PUT") {
      const body = JSON.parse(init.body);
      assert.equal(body.toplevel_path, gatewayFacts({ namespace: "team" }, cwd).toplevel_path);
      assert.equal("remote_url" in body, false, "pins never key on a git remote here");
      pinned = true;
      return { ok: true, status: 200, async json() { return { namespace: "acme/api", key: `path:${body.toplevel_path}` }; } };
    }
    if (u.endsWith("/v1/handshake")) {
      const body = JSON.parse(init.body);
      const hs = pinned
        ? { namespace: "acme/api", namespace_source: "pin", pin: { key: `path:${body.project.toplevel_path}` }, identity: {}, settings: {}, settings_sources: {}, read_set: [], server: { version: "t", default_namespace: "default" } }
        : { namespace: body.project.declared_namespace, namespace_source: "declared", identity: {}, settings: {}, settings_sources: {}, read_set: [], server: { version: "t", default_namespace: "default" } };
      return { ok: true, status: 200, async json() { return hs; } };
    }
    return { ok: false, status: 404, async json() { return {}; } };
  };
  try {
    const ctx = createSessionContext({ namespace: "team" }, {}, cwd);
    const before = await sessionLive(ctx, {});
    assert.equal(before.namespace, "team");
    assert.equal(before.namespace_source, "server:declared");

    const commands = {};
    registerMeminiCommands({ logger: { warn() {} }, registerCommand(def) { commands[def.name] = def.handler; } }, ctx);
    const { text } = await commands["memini:namespace"]({ args: "acme/api" });
    assert.match(text, /namespace pinned: acme\/api/);

    // No TTL wait: the write invalidated the memo, so the next resolution
    // re-handshakes and lands on the pin.
    const after = await sessionLive(ctx, {});
    assert.equal(after.namespace, "acme/api");
    assert.equal(after.namespace_source, "server:pin");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("register wires memini:status and memini:namespace when the host supports commands", () => {
  const names = [];
  plugin.register({
    pluginConfig: { enabled: true },
    registerMemoryCapability() {}, registerHook() {},
    on() {},
    logger: { warn() {} },
    registerTool() {},
    registerCommand(def) {
      names.push(def.name);
    },
  });
  assert.deepEqual(names.sort(), ["memini:namespace", "memini:status"]);
});

test("register survives a host with no registerCommand at all", () => {
  const hooks = {};
  plugin.register({
    pluginConfig: { enabled: true },
    registerMemoryCapability() {}, registerHook() {},
    on(name, handler) { hooks[name] = handler; },
    logger: { warn() {} },
    registerTool() {},
  });
  // No commands is survivable; losing the memory slot is not.
  assert.equal(typeof hooks.before_prompt_build, "function");
  assert.equal(typeof hooks.agent_end, "function");
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
  assert.equal(effectiveNamespace(cfg, { agentId: "bob" }), "openclaw-bob");
});

test("resolves the agent from ctx.agentId or the sessionKey agent segment", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentId: "carol" }), "carol");
  // raw session ids are not identities (they would fragment namespaces);
  // only the agent segment of an agent-keyed sessionKey resolves
  assert.equal(effectiveNamespace(cfg, { sessionId: "sess1" }), "ns");
  assert.equal(effectiveNamespace(cfg, { sessionKey: "agent:bob:b7d2-uuid" }), "bob");
});

test("sanitizes the agent id", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentId: "My Agent/2!" }), "My-Agent-2");
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

test("agentId wins and session keys parse from ctx", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "alice");
  assert.equal(effectiveNamespace(cfg, { sessionKey: "agent:carol:cron:daily" }), "carol");
  assert.equal(effectiveNamespace(cfg, { sessionKey: "heartbeat:gateway" }), "ns");
});

test("skip_without_agent skips gateway-level sessions but keeps agent crons", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, { sessionKey: "agent:alice:heartbeat:hourly" }), "alice");
  assert.equal(effectiveNamespace(cfg, { sessionKey: "heartbeat:gateway" }), null);
  assert.equal(effectiveNamespace(cfg, {}), null);
});

// --- skip_system_turns ------------------------------------------------------

test("skip_system_turns defaults on with the OpenClaw trigger kinds", () => {
  const cfg = resolveConfig(undefined);
  assert.equal(cfg.skip_system_turns, true);
  assert.deepEqual(cfg.system_kinds, ["heartbeat", "cron"]);
  // Opt back out explicitly.
  assert.equal(resolveConfig({ skip_system_turns: false }).skip_system_turns, false);
});

test("system_kinds can be overridden and is lowercased", () => {
  const cfg = resolveConfig({ system_kinds: ["Poll", "TICK"] });
  assert.deepEqual(cfg.system_kinds, ["poll", "tick"]);
});

// Verified against OpenClaw source @3288291 (src/plugins/hook-before-agent-start.types.ts,
// src/auto-reply/reply/get-reply.ts): before_prompt_build/agent_end receive a
// PluginHookAgentContext whose `trigger` is "user" for a real message and
// "heartbeat"/"cron" for system polls. Heartbeat/cron runs reuse the agent's
// main session (sessionKey "agent:main:main") and carry the configured
// HEARTBEAT.md prompt — no marker, no kind segment — so ctx.trigger is the only
// reliable signal.
test("detectSystemKind matches ctx.trigger, case-insensitive and exact", () => {
  assert.equal(detectSystemKind({ trigger: "heartbeat" }), "heartbeat");
  assert.equal(detectSystemKind({ trigger: "CRON" }), "cron");
  // a real user turn and unknown/other triggers are not system turns
  assert.equal(detectSystemKind({ trigger: "user" }), "");
  assert.equal(detectSystemKind({ trigger: "budget" }), "");
  assert.equal(detectSystemKind({}), "");
});

test("shouldSkipSystemTurn gates on flag + ctx.trigger; default-on skips heartbeats", () => {
  const def = resolveConfig(undefined); // skip_system_turns on by default
  assert.equal(shouldSkipSystemTurn(def, { trigger: "heartbeat", sessionKey: "agent:main:main" }), true);
  assert.equal(shouldSkipSystemTurn(def, { trigger: "cron" }), true);
  assert.equal(shouldSkipSystemTurn(def, { trigger: "user" }), false);
  // flag off keeps everything
  const off = resolveConfig({ skip_system_turns: false });
  assert.equal(shouldSkipSystemTurn(off, { trigger: "heartbeat" }), false);
});

test("shouldSkipSystemTurn honors a custom system_kinds set", () => {
  const on = resolveConfig({ skip_system_turns: true, system_kinds: ["poll"] });
  assert.equal(shouldSkipSystemTurn(on, { trigger: "poll" }), true);
  // a default kind no longer matches once the set is overridden
  assert.equal(shouldSkipSystemTurn(on, { trigger: "heartbeat" }), false);
});

// --- explicit tools (expose_tools) -----------------------------------------

// On by default as of 0.6.9. The slot's automatic recall/capture cannot express
// scope, visibility, or the briefing's ancestor Scope line — without the tools an
// agent here does not have those capabilities at all. Opting out stays possible.
test("expose_tools is on by default and can still be turned off explicitly", () => {
  assert.equal(resolveConfig(undefined).expose_tools, true);
  assert.equal(resolveConfig({}).expose_tools, true);
  assert.equal(resolveConfig({ expose_tools: true }).expose_tools, true);
  assert.equal(resolveConfig({ expose_tools: false }).expose_tools, false);
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
    registerTool(_factory, opts) {
      names.push(...(opts?.names ?? []));
    },
  });
  assert.equal(result, undefined, "register must not return a Promise");
  assert.notEqual(
    Object.getPrototypeOf(plugin.register).constructor.name,
    "AsyncFunction",
    "register must not be an async function",
  );
  // Tools are wired before register returns — not deferred (the guarded api would drop them).
  assert.deepEqual(names.sort(), ["memory_briefing", "memory_forget", "memory_list", "memory_recall", "memory_remember"]);
});

test("register does not touch registerTool when expose_tools is explicitly off", async () => {
  let registered = 0;
  await plugin.register({
    pluginConfig: { enabled: true, expose_tools: false },
    registerMemoryCapability() {}, registerHook() {},
    on() {},
    logger: {},
    registerTool() {
      registered++;
    },
  });
  assert.equal(registered, 0, "no tools should register when expose_tools is off");
});

// The default path: a config that never mentions expose_tools still gets the
// tools, because scope/visibility/briefing exist nowhere else on this harness.
test("register wires the memory tools with no expose_tools in the config at all", async () => {
  const names = [];
  await plugin.register({
    pluginConfig: { enabled: true },
    registerMemoryCapability() {}, registerHook() {},
    on() {},
    logger: { warn() {} },
    registerTool(_factory, opts) {
      names.push(...(opts?.names ?? []));
    },
  });
  assert.deepEqual(names.sort(), ["memory_briefing", "memory_forget", "memory_list", "memory_recall", "memory_remember"]);
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
  const { order } = await collectTools(fakeClient(), { namespace_per_agent: false });
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
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    return {
      ok: true,
      async json() {
        return { results: [{ memory: { summary: "x", tier: "semantic" }, score: 0.9 }] };
      },
      async text() { return ""; },
    };
  });
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
    assert.equal(search.exclude_turns_younger_than, undefined, "server-side guard is on by default; plugin does not opt in");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("requests carry X-Memini-Home when configured, omit it otherwise", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    return {
      ok: true,
      async json() { return { results: [] }; },
      async text() { return ""; },
    };
  });
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false, home: "personal/acme" },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    await hooks.before_prompt_build({ prompt: "q" }, {});
    const search = requests.find((r) => r.url.endsWith("/v1/search"));
    assert.equal(search.init.headers["X-Memini-Home"], "personal/acme");
  } finally {
    globalThis.fetch = realFetch;
  }

  const hooks2 = {};
  const requests2 = [];
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests2.push({ url: String(url), init });
    return { ok: true, async json() { return { results: [] }; }, async text() { return ""; } };
  });
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks2[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    await hooks2.before_prompt_build({ prompt: "q" }, {});
    const search2 = requests2.find((r) => r.url.endsWith("/v1/search"));
    assert.equal(search2.init.headers["X-Memini-Home"], undefined);
  } finally {
    globalThis.fetch = realFetch;
  }
});

// The MEMINI_URL/MEMINI_TOKEN aliases are retired everywhere: readBootstrap
// (the handshake/pins/status transport) never honored them, so the data plane
// ignoring them too is what keeps both paths pointed at one server with one
// credential.
test("retired MEMINI_TOKEN alias is IGNORED: no Authorization without MEMINI_API_KEY", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  const prevToken = process.env.MEMINI_TOKEN;
  const prevKey = process.env.MEMINI_API_KEY;
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    return { ok: true, async json() { return { results: [] }; }, async text() { return ""; } };
  });
  try {
    delete process.env.MEMINI_API_KEY;
    process.env.MEMINI_TOKEN = "sk-legacy-alias-token";
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    await hooks.before_prompt_build({ prompt: "q" }, {});
    const search = requests.find((r) => r.url.endsWith("/v1/search"));
    assert.equal(search.init.headers.Authorization, undefined, "MEMINI_TOKEN must not become a bearer");
  } finally {
    globalThis.fetch = realFetch;
    if (prevToken === undefined) delete process.env.MEMINI_TOKEN;
    else process.env.MEMINI_TOKEN = prevToken;
    if (prevKey === undefined) delete process.env.MEMINI_API_KEY;
    else process.env.MEMINI_API_KEY = prevKey;
  }
});

// before_prompt_build fires on every step of a turn; an unchanged query returns
// the same memories, so without dedup the same block is re-injected on every
// tool call (eleboucher/memini#21). The same session must only be shown a given
// memory once; a different session is unaffected.
test("recall does not re-inject memories already shown in the same session (#21)", async () => {
  const hooks = {};
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async () => ({
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
  }));
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    const ev = { prompt: "q" };
    const ctx = (sessionId) => ({ sessionId });
    const first = await hooks.before_prompt_build(ev, ctx("s1"));
    assert.match(first.prependContext, /alpha/);
    assert.match(first.prependContext, /beta/);
    // Same session, same query (a later tool-call step): both already shown -> no re-injection.
    const second = await hooks.before_prompt_build(ev, ctx("s1"));
    assert.equal(second, undefined, "already-shown memories must not be re-injected");
    // A different session still sees them.
    const other = await hooks.before_prompt_build(ev, ctx("s2"));
    assert.match(other.prependContext, /alpha/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

// The per-session dedupe window is capped: oldest ids age out (and may
// re-inject); recent ids stay suppressed.
test("per-session injected-id window is bounded (oldest ids age out)", async () => {
  const hooks = {};
  const realFetch = globalThis.fetch;
  let nextResults = [];
  globalThis.fetch = async () => ({
    ok: true,
    async json() { return { results: nextResults }; },
    async text() { return ""; },
  });
  const hit = (id) => ({ memory: { id, summary: `memory ${id}`, tier: "semantic" }, score: 0.9 });
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    const ctx = { sessionId: "s1" };
    // Push 56 distinct ids through the window (cap is 50): m0..m55.
    for (let i = 0; i < 56; i++) {
      nextResults = [hit(`m${i}`)];
      const res = await hooks.before_prompt_build({ prompt: `q${i}` }, ctx);
      // Each id is new to the session, so each call injects.
      assert.match(res.prependContext, new RegExp(`m${i}\\b`));
    }
    // m0 was evicted from the 50-id window -> allowed to re-inject.
    nextResults = [hit("m0")];
    const oldAgain = await hooks.before_prompt_build({ prompt: "old" }, ctx);
    assert.ok(oldAgain, "an id evicted from the window must be allowed to re-inject");
    assert.match(oldAgain.prependContext, /m0/);
    // m55 is still inside the window -> suppressed.
    nextResults = [hit("m55")];
    const recentAgain = await hooks.before_prompt_build({ prompt: "recent" }, ctx);
    assert.equal(recentAgain, undefined, "a recent id must stay suppressed");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Already-shown ids ride to the server as exclude_ids; an older server that
// 400s on the unknown field gets one retry without it, then it stops.
test("recall sends exclude_ids and falls back when the server rejects them", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  let rejectExcludeIds = false;
  globalThis.fetch = async (url, init) => {
    const body = init && init.body ? JSON.parse(init.body) : {};
    requests.push({ url: String(url), body });
    if (rejectExcludeIds && body.exclude_ids) {
      return { ok: false, status: 400, async json() { return {}; }, async text() { return 'unknown field "exclude_ids"'; } };
    }
    return {
      ok: true,
      async json() {
        return { results: [{ memory: { id: "m1", summary: "alpha", tier: "semantic" }, score: 0.9 }] };
      },
      async text() { return ""; },
    };
  };
  const searches = () => requests.filter((r) => r.url.endsWith("/v1/search"));
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    const ctx = { sessionId: "s1" };
    // First recall: nothing shown yet, so no exclude_ids on the wire.
    await hooks.before_prompt_build({ prompt: "q" }, ctx);
    assert.equal(searches()[0].body.exclude_ids, undefined);
    // Second recall: m1 was shown, so it must ride along as exclude_ids.
    await hooks.before_prompt_build({ prompt: "q" }, ctx);
    assert.deepEqual(searches()[1].body.exclude_ids, ["m1"]);

    // Old server: 400 on exclude_ids -> one retry without it, then never again.
    rejectExcludeIds = true;
    await hooks.before_prompt_build({ prompt: "q" }, ctx);
    const [, , withField, retry] = searches();
    assert.deepEqual(withField.body.exclude_ids, ["m1"], "first attempt still carries exclude_ids");
    assert.equal(retry.body.exclude_ids, undefined, "the retry must drop exclude_ids");
    await hooks.before_prompt_build({ prompt: "q" }, ctx);
    assert.equal(searches().length, 5, "after the fallback each recall is a single request");
    assert.equal(searches()[4].body.exclude_ids, undefined, "exclude_ids is never sent again");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// The capture echo guard is time-based: a fresh burst wider than any count cap
// is fully suppressed, and captures age back into recall by time.
test("capture echo guard suppresses a fresh burst and ages out by time", async () => {
  const hooks = {};
  const realFetch = globalThis.fetch;
  const realNow = Date.now;
  let skew = 0;
  Date.now = () => realNow.call(Date) + skew;
  let writes = 0;
  globalThis.fetch = async (url) => {
    const body = String(url).endsWith("/v1/search")
      ? {
          results: Array.from({ length: 10 }, (_, k) => ({
            memory: { id: `cap-${k}`, summary: `turn ${k}`, tier: "episodic", metadata: { format: "turn" } },
            score: 0.9,
          })),
        }
      : { id: `cap-${writes++}` };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  };
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    // A burst of 10 captures across sessions of the same agent.
    for (let k = 0; k < 10; k++) {
      await hooks.agent_end(
        { success: true, messages: [{ role: "user", content: `q${k}` }, { role: "assistant", content: `a${k}` }] },
        { sessionId: `sess-${k}` },
      );
    }
    // All 10 are fresh: none may echo, even beyond a small count cap.
    const during = await hooks.before_prompt_build({ prompt: "q" }, {});
    assert.equal(during, undefined, "a fresh capture burst must not echo");
    // Past the window they are long-term memory again.
    skew = 6 * 60_000;
    const after = await hooks.before_prompt_build({ prompt: "q" }, {});
    assert.ok(after, "aged-out captures must be recallable again");
    assert.match(after.prependContext, /turn 0/);
  } finally {
    globalThis.fetch = realFetch;
    Date.now = realNow;
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
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    const body = String(url).endsWith("/v1/search")
      ? { results: [{ memory: { summary: "prior fact", tier: "semantic" }, score: 0.9 }] }
      : { id: "m1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  });
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
    assert.equal(JSON.parse(search.init.body).exclude_turns_younger_than, undefined, "server-side guard is on by default; plugin does not opt in");

    // agent_end is the raw-conversation hook: when the host grants conversation
    // access, event.messages is present and the turn is captured as episodic.
    // The ctx must identify the session — untagged captures are skipped.
    await hooks.agent_end(
      {
        success: true,
        messages: [
          { role: "user", content: "q" },
          { role: "assistant", content: "a" },
        ],
      },
      { sessionId: "sess-1" },
    );
    const write = requests.find((r) => r.url.endsWith("/v1/memories"));
    assert.ok(write, "capture should POST /v1/memories");
    const body = JSON.parse(write.init.body);
    assert.equal(body.tier, undefined, "capture omits the tier so the server intakes it as working");
    assert.match(body.content, /q/);
    assert.match(body.content, /a/);
    assert.doesNotMatch(body.content, /User:|Assistant:/);
    assert.equal(body.metadata.format, "turn");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("sessionIdentity prefers session ids and sanitizes them; empty without one", () => {
  assert.equal(sessionIdentity({ sessionId: "sess-abc" }), "sess-abc");
  assert.equal(sessionIdentity({ sessionKey: "agent:bob:run/42" }), "agent-bob-run-42");
  // runId is per-run, so it is NOT a session identity: a capture tagged with
  // it could never be excluded by the next run's recall guard.
  assert.equal(sessionIdentity({ runId: "r1" }), "");
  assert.equal(sessionIdentity({}), "");
});

test("auto-recall excludes the current session's own captures; capture tags the session", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    const body = String(url).endsWith("/v1/search") ? { results: [] } : { id: "m1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  });
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

test("without a session id, auto-recall stays unscoped and capture is skipped", async () => {
  const hooks = {};
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    const body = String(url).endsWith("/v1/search") ? { results: [] } : { id: "m1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  });
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
    // An untagged capture could never be excluded by the recall guard, so no
    // session identity means no capture at all.
    assert.equal(
      requests.find((r) => r.url.endsWith("/v1/memories")),
      undefined,
      "capture without a session identity must be skipped",
    );
  } finally {
    globalThis.fetch = realFetch;
  }
});

// The session-id exclusion misses when the session id is absent/rolled at
// recall. The message-ID guard (keyed by namespace) survives that asymmetry.
test("recall drops a just-captured turn by ID (message-ID echo guard)", async () => {
  const hooks = {};
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async (url) => {
    const body = String(url).endsWith("/v1/search")
      ? {
          results: [
            { memory: { id: "echo-1", summary: "just-captured turn", tier: "episodic", metadata: { format: "turn", session_id: "sess-other" } }, score: 0.95 },
            { memory: { id: "prior", summary: "prior fact", tier: "semantic", metadata: {} }, score: 0.8 },
          ],
        }
      : { id: "echo-1" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  });
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    await hooks.agent_end(
      { success: true, messages: [{ role: "user", content: "q" }, { role: "assistant", content: "a" }] },
      { sessionId: "sess-capture" },
    );
    // No session id at recall: exclude_metadata absent, so the server returns
    // the just-captured turn. The message-ID guard must still drop it.
    const recall = await hooks.before_prompt_build({ prompt: "q" }, {});
    assert.ok(recall, "recall should still inject real memories");
    assert.doesNotMatch(recall.prependContext, /just-captured turn/, "the just-captured turn must not be echoed back");
    assert.match(recall.prependContext, /prior fact/, "older long-term memories must still surface");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("the echo guard is scoped per namespace — another agent's captures don't suppress recall", async () => {
  const hooks = {};
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async (url) => {
    const body = String(url).endsWith("/v1/search")
      ? {
          results: [
            { memory: { id: "alice-cap", summary: "alice's turn", tier: "episodic", metadata: { format: "turn" } }, score: 0.9 },
          ],
        }
      : { id: "alice-cap" };
    return { ok: true, async json() { return body; }, async text() { return ""; } };
  });
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: true, namespace_template: "{namespace}-{agent}" },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) { hooks[name] = handler; },
      logger: { warn() {} },
      registerTool() {},
    });
    await hooks.agent_end(
      { success: true, messages: [{ role: "user", content: "q" }, { role: "assistant", content: "a" }] },
      { sessionId: "sess-alice", agentId: "alice" },
    );
    const recall = await hooks.before_prompt_build({ prompt: "q" }, { agentId: "bob" });
    assert.ok(recall, "bob's recall should not be suppressed by alice's capture");
    assert.match(recall.prependContext, /alice's turn/, "a different namespace's captures must still surface");
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

test("tools register as a single optional factory naming all tools", async () => {
  const { opts } = await collectTools(fakeClient(), { namespace_per_agent: false });
  assert.equal(opts.optional, true, "factory must register optional");
  assert.deepEqual(
    [...opts.names].sort(),
    ["memory_briefing", "memory_forget", "memory_list", "memory_recall", "memory_remember"],
    "opts.names must list every tool so the host can match the factory by name",
  );
});

test("memory_recall passes query + tag/metadata filters and formats results", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_recall.execute("id", {
    query: "auth race",
    tags: ["auth"],
    metadata: { category: "bug_fixes" },
  });
  const call = client.calls.at(-1);
  assert.equal(call.method, "POST");
  assert.equal(call.path, "/v1/search");
  assert.equal(call.ns, "acme/widget");
  assert.deepEqual(call.body, { query: "auth race", limit: 3, tags: ["auth"], metadata: { category: "bug_fixes" } });
  assert.deepEqual(JSON.parse(out.content[0].text), {
    results: [{ id: "m1", content: "hit", summary: "", tier: "semantic", score: 0.9 }],
  });
});

test("memory_forget DELETEs the memory by id", async () => {
  const client = fakeClient();
  const { byName, order } = await collectTools(client, { namespace_per_agent: false });
  assert.ok(order.includes("memory_forget"), "memory_forget is registered");
  const out = await byName.memory_forget.execute("id", { id: "mem 1/x" });
  const call = client.calls.at(-1);
  assert.equal(call.method, "DELETE");
  assert.equal(call.path, "/v1/memories/mem%201%2Fx", "id is percent-encoded into the path");
  assert.deepEqual(JSON.parse(out.content[0].text), { forgotten: true });
  // Missing id → no request, explicit error.
  client.calls.length = 0;
  const bad = await byName.memory_forget.execute("id", {});
  assert.equal(client.calls.length, 0, "no DELETE without an id");
  assert.equal(JSON.parse(bad.content[0].text).forgotten, false);
});

test("memory_list issues a filtered GET and maps the rows", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_list.execute("id", { tiers: ["procedural"], metadata: { category: "bug_fixes" } });
  const call = client.calls.at(-1);
  assert.equal(call.method, "GET");
  assert.equal(call.path, "/v1/memories?tier=procedural&meta=category%3Dbug_fixes&limit=20");
  const { memories } = JSON.parse(out.content[0].text);
  assert.equal(memories.length, 1);
  assert.equal(memories[0].metadata.category, "bug_fixes");
});

test("memory_remember maps category to metadata and omits the tier for the server to classify", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_remember.execute("id", { content: "fact", category: "bug_fixes", tags: ["x"] });
  const call = client.calls.at(-1);
  assert.equal(call.path, "/v1/memories");
  assert.deepEqual(call.body, { content: "fact", tags: ["x"], metadata: { category: "bug_fixes" } });
  assert.deepEqual(JSON.parse(out.content[0].text), { id: "m1", success: true });
});

test("tool namespace follows per-agent resolution from the factory ctx", async () => {
  const client = fakeClient();
  // The agent identity is delivered to the factory, not the execute call.
  const { byName } = await collectTools(
    client,
    { namespace_per_agent: true, namespace_template: "{namespace}-{agent}" },
    { agentId: "miso" },
    fakeHandshake({ namespace: "team" }),
  );
  await byName.memory_recall.execute("id", { query: "q" });
  assert.equal(client.calls.at(-1).ns, "team-miso", "per-agent ctx should scope the tool call");
});

test("tool namespace resolves the agent from a sessionKey too", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(
    client,
    { namespace_per_agent: true, namespace_template: "{namespace}-{agent}" },
    { sessionKey: "agent:carol:main" },
    fakeHandshake({ namespace: "team" }),
  );
  await byName.memory_remember.execute("id", { content: "fact" });
  assert.equal(client.calls.at(-1).ns, "team-carol");
});

test("tool falls back to base namespace and warns once when no agent resolves", async () => {
  const client = fakeClient();
  const warns = [];
  const registered = [];
  const api = { logger: { warn: (m) => warns.push(m) }, registerTool: (factory, opts) => registered.push({ factory, opts }) };
  const sessionCtx = createSessionContext({ namespace_per_agent: true, namespace_template: "{namespace}-{agent}" }, process.env, tmpdir());
  sessionCtx.memo = { get: async () => fakeHandshake({ namespace: "team" }), invalidate() {} };
  await registerMeminiTools(api, client, sessionCtx);
  // Materialize twice with an agentless ctx: base namespace, warned exactly once.
  const a = [].concat(registered[0].factory({}));
  const b = [].concat(registered[0].factory({}));
  await a.find((t) => t.name === "memory_recall").execute("id", { query: "q" });
  await b.find((t) => t.name === "memory_recall").execute("id", { query: "q" });
  assert.equal(client.calls.at(-1).ns, "team", "agentless tool call uses the base namespace");
  assert.equal(warns.length, 1, "the missing-agent fallback warns once, not per call");
  assert.match(warns[0], /base namespace "team"/);
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
  globalThis.fetch = withHandshakeFailure(async (url, init) => {
    requests.push({ url: String(url), init });
    return { ok: true, async json() { return { id: "m1" }; }, async text() { return ""; } };
  });
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
      { sessionId: "sess-fail" },
    );
    const write = requests.find((r) => r.url.endsWith("/v1/memories"));
    assert.ok(write, "should still capture failed runs");
    const body = JSON.parse(write.init.body);
    assert.equal(body.tier, undefined, "capture omits the tier so the server intakes it as working");
    assert.equal(body.metadata.failed, true, "failed runs must be tagged");
    assert.equal(body.metadata.session_id, "sess-fail", "captures must carry the session identity");

    // No session identity → no capture: an untagged turn could never be
    // excluded by the pre-turn recall guard and would echo back forever.
    requests.length = 0;
    await hooks.agent_end(
      {
        success: true,
        messages: [
          { role: "user", content: "q2" },
          { role: "assistant", content: "a2" },
        ],
      },
      {},
    );
    assert.equal(
      requests.find((r) => r.url.endsWith("/v1/memories")),
      undefined,
      "capture without a session identity must be skipped",
    );
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memory_remember drops an unknown tier so the server classifies", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false, expose_tools: true });
  await byName.memory_remember.execute("id", { content: "fact", tier: "bogus" });
  const call = client.calls.at(-1);
  assert.equal(call.body.tier, undefined, "unknown tier must be dropped, not defaulted");
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

test("an HTTP error is logged even when fallback_on_error degrades it", async () => {
  const hooks = {};
  const warned = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = withHandshakeFailure(async () => ({
    ok: false,
    status: 500,
    async json() { return {}; },
    async text() { return "boom"; },
  }));
  try {
    await plugin.register({
      pluginConfig: { enabled: true, namespace_per_agent: false },
      registerMemoryCapability() {}, registerHook() {},
      on(name, handler) {
        hooks[name] = handler;
      },
      logger: { warn(m) { warned.push(String(m)); } },
      registerTool() {},
    });
    // A swallowed 401/500 on a capture or recall looks like "memory isn't
    // working"; the degrade path must still say why.
    const recall = await hooks.before_prompt_build({ prompt: "q" }, { sessionId: "sess-1" });
    assert.equal(recall, undefined, "recall failure degrades to no injection");
    assert.ok(
      warned.some((m) => m.includes("failed: 500")),
      `expected a failed-status warn, got: ${JSON.stringify(warned)}`,
    );
  } finally {
    globalThis.fetch = realFetch;
  }
});

// --- scope / visibility / briefing (the cascade vocabulary, PR #36) ----------
//
// These tools are this harness's whole model-facing surface — it does not proxy
// MCP — so a capability missing from the schema here is a capability the model
// simply does not have.

test("memory_recall exposes scope and forwards only the model-facing vocabulary", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  assert.deepEqual(byName.memory_recall.parameters.properties.scope.enum, ["project", "full", "everywhere"]);

  await byName.memory_recall.execute("id", { query: "q", scope: "project" });
  assert.equal(client.calls.at(-1).body.scope, "project");

  // Omitted: nothing on the wire, so the server's "full" default applies.
  await byName.memory_recall.execute("id", { query: "q" });
  assert.equal("scope" in client.calls.at(-1).body, false);

  // "exact"/"subtree" are deprecated REST aliases, not part of the model's
  // vocabulary — forwarding one would be a 400.
  await byName.memory_recall.execute("id", { query: "q", scope: "exact" });
  assert.equal("scope" in client.calls.at(-1).body, false);
});

test("memory_recall passes read provenance through so the model can learn the topology", async () => {
  const client = fakeClient();
  client.postJson = async (path, body, ns) => {
    client.calls.push({ method: "POST", path, body, ns });
    return {
      results: [
        { memory: { id: "m1", content: "own", tier: "semantic", namespace: "ns" }, score: 0.9 },
        { memory: { id: "m2", content: "inherited", tier: "semantic", namespace: "acme" }, score: 0.5, from: "acme" },
      ],
    };
  };
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_recall.execute("id", { query: "q" });
  const { results } = JSON.parse(out.content[0].text);
  // A primary-namespace hit carries no "from" at all — its absence is what tells
  // the model "this project's own memory".
  assert.equal("from" in results[0], false);
  assert.equal(results[0].namespace, "ns");
  assert.equal(results[1].from, "acme");
});

test("memory_remember forwards visibility verbatim; the server owns the ancestor vocabulary", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  await byName.memory_remember.execute("id", { content: "fact", visibility: "personal" });
  assert.deepEqual(client.calls.at(-1).body, { content: "fact", visibility: "personal" });

  // An ancestor name is in no client-side enum: only the server can resolve this
  // namespace's chain, so the name goes through untouched.
  await byName.memory_remember.execute("id", { content: "fact", visibility: "acme" });
  assert.equal(client.calls.at(-1).body.visibility, "acme");

  await byName.memory_remember.execute("id", { content: "fact" });
  assert.equal("visibility" in client.calls.at(-1).body, false);
});

test("a rejected visibility reaches the model with the valid chain, not a bare failure", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_remember.execute("id", { content: "fact", visibility: "widgets" });
  const res = JSON.parse(out.content[0].text);
  assert.equal(res.success, false);
  // Without the server's error text the model has nothing to correct against —
  // it would just retry the same bad name.
  assert.match(res.error, /valid: project, personal, acme/);
});

test("memory_briefing GETs the header-scoped briefing and keeps the Scope line", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_briefing.execute("id", { scope: "everywhere" });
  const call = client.calls.at(-1);
  assert.equal(call.method, "GET");
  // Header-scoped: the namespace is never in the path — the model never names one.
  assert.equal(call.path, "/v1/namespaces/briefing?scope=everywhere");
  const res = JSON.parse(out.content[0].text);
  assert.equal(res.scope_header, "Scope: ns ← acme(4) ← personal(2)");
  assert.equal(res.pinned[0].id, "p1");
  assert.equal(res.facts[0].from, "acme");
});

test("memory_briefing answers rather than throwing when memini is unreachable", async () => {
  const client = fakeClient();
  client.getJson = async () => null; // the client's degrade path
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_briefing.execute("id", {});
  assert.equal(JSON.parse(out.content[0].text).briefing, null);
});

test("memory_remember surfaces reinforced so a no-op write is not reported as a new save", async () => {
  const client = fakeClient();
  // The fact was already known: the server strengthened the existing memory and
  // returned its id. Dropping the flag would let the model claim a fresh save.
  client.postJsonResult = async (path, body, ns) => {
    client.calls.push({ method: "POST", path, body, ns });
    return { ok: true, data: { id: "existing-1", reinforced: true } };
  };
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_remember.execute("id", { content: "known fact" });
  assert.deepEqual(JSON.parse(out.content[0].text), { id: "existing-1", success: true, reinforced: true });
});

test("a genuinely new write carries no reinforced flag", async () => {
  const client = fakeClient();
  const { byName } = await collectTools(client, { namespace_per_agent: false });
  const out = await byName.memory_remember.execute("id", { content: "novel fact" });
  assert.equal("reinforced" in JSON.parse(out.content[0].text), false);
});
