import { test } from "node:test";
import assert from "node:assert/strict";
import {
  applyTemplate,
  approxTokens,
  createPlaintextBearerAuthGuard,
  createSessionContext,
  detectSystemKind,
  effectiveConfig,
  effectiveNamespace,
  fitByTokens,
  gatewayFacts,
  memoizeAsync,
  meminiListPath,
  pinKeyFacts,
  registerMeminiCommands,
  registerMeminiTools,
  resolveBaseNamespace,
  resolveConfig,
  sessionIdentity,
  sessionLive,
  shouldSkipSystemTurn,
  startsWithNoisePrefix,
  stripRuntimePreambles,
  type ResolvedConfig,
} from "../src/index.ts";
import { execSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// A developer shell may export the real memini config; clear it so
// resolveConfig tests see the documented defaults (an exported MEMINI_NAMESPACE
// — the fish-universal-variable case this feature exists for — would
// otherwise fail every default-namespace assertion below).
for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_HOME", "MEMINI_NAMESPACE"]) {
  delete process.env[k];
}

type MeminiClient = Parameters<typeof registerMeminiTools>[1];

type RecordedCall = { method: string; path: string; body?: unknown; ns?: string };

function fakeClient(): MeminiClient & { calls: RecordedCall[] } {
  const calls: RecordedCall[] = [];
  return {
    calls,
    baseUrl: "http://localhost:8080",
    async postJson(path: string, body: unknown, ns?: string) {
      calls.push({ method: "POST", path, body, ns });
      return path.includes("search")
        ? { results: [{ memory: { content: "hit", summary: "", tier: "semantic" }, score: 0.9 }] }
        : { id: "m1" };
    },
    async postJsonResult(path: string, body: unknown, ns?: string) {
      calls.push({ method: "POST", path, body, ns });
      return { ok: true, data: { id: "m1" } };
    },
    async getJson(path: string, ns?: string) {
      calls.push({ method: "GET", path, ns });
      if (path.includes("briefing")) {
        return {
          namespace: "ns",
          scope_header: "Scope: ns ← acme(4)",
          pinned: [{ memory: { id: "p1", content: "pinned", tier: "semantic" } }],
          facts: [{ memory: { id: "f1", content: "org", tier: "semantic", namespace: "acme" }, from: "acme" }],
        };
      }
      return { memories: [{ id: "m1", content: "c", tier: "procedural", tags: ["auth"], metadata: { category: "bug_fixes" } }] };
    },
    async deleteJson(path: string, ns?: string) {
      calls.push({ method: "DELETE", path, ns });
      return {};
    },
  };
}

const baseCfg = (overrides: Partial<ResolvedConfig> = {}): ResolvedConfig => ({
  ...resolveConfig({ namespace_per_agent: false }),
  ...overrides,
});

function fakeHandshake(overrides: Record<string, any> = {}) {
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

// Build a SessionContext whose memo resolves instantly to `hs` without any
// network call — for tests that only care about how a given handshake result
// (or its absence) is merged, not about the handshake transport itself.
function fixedSessionContext(pluginConfig: any, hs: any, env: Record<string, string | undefined> = process.env): ReturnType<typeof createSessionContext> {
  const ctx = createSessionContext(pluginConfig, env, tmpdir());
  return {
    ...ctx,
    memo: { get: async () => hs, invalidate() {} },
  };
}

// --- resolveConfig: synchronous baseline (no handshake) -----------------------

test("resolveConfig: defaults match the documented contract", () => {
  const cfg: ResolvedConfig = resolveConfig(undefined);
  assert.equal(cfg.enabled, true);
  assert.equal(cfg.base_url, "http://localhost:8080");
  assert.equal(cfg.namespace, "openclaw");
  assert.equal(cfg.namespace_source, "default");
  assert.equal(cfg.namespace_per_agent, true);
  assert.equal(cfg.namespace_template, "{namespace}-{agent}");
  assert.equal(cfg.skip_without_agent, false);
  assert.equal(cfg.skip_system_turns, true);
  assert.deepEqual(cfg.system_kinds, ["heartbeat", "cron"]);
  assert.equal(cfg.fallback_on_error, true);
  assert.equal(cfg.timeout_ms, 5000);
  assert.equal(cfg.expose_tools, true);
  assert.equal(cfg.recall_limit, 3);
});

test("resolveConfig: base_url falls back to MEMINI_BASE_URL then MEMINI_URL env, config wins", () => {
  try {
    process.env.MEMINI_URL = "http://alias:8080";
    assert.equal(resolveConfig(undefined).base_url, "http://alias:8080");
    process.env.MEMINI_BASE_URL = "http://canonical:8080";
    assert.equal(resolveConfig(undefined).base_url, "http://canonical:8080");
    assert.equal(resolveConfig({ base_url: "http://cfg:9000" }).base_url, "http://cfg:9000");
  } finally {
    delete process.env.MEMINI_BASE_URL;
    delete process.env.MEMINI_URL;
  }
});

test("resolveConfig: explicit recall_limit survives, zero/negative/non-finite fall back", () => {
  assert.equal(resolveConfig({ recall_limit: 12 }).recall_limit, 12);
  assert.equal(resolveConfig({ recall_limit: 0 }).recall_limit, 3);
  assert.equal(resolveConfig({ recall_limit: Number.NaN }).recall_limit, 3);
});

test("effectiveNamespace: return type is `string | null`", () => {
  const a: string | null = effectiveNamespace(baseCfg({ namespace_per_agent: true, namespace_template: "{agent}" }), { agentId: "alice" });
  assert.equal(a, "alice");

  const b: string | null = effectiveNamespace(
    baseCfg({ namespace_per_agent: true, skip_without_agent: true, namespace_template: "{agent}" }),
    {},
  );
  assert.equal(b, null);

  const c: string | null = effectiveNamespace(baseCfg({ namespace_per_agent: false }), { agentId: "alice" });
  assert.equal(c, "openclaw");
});

test("effectiveNamespace: resolves ctx.agentId, else the sessionKey agent segment", () => {
  const cfg = baseCfg({ namespace_per_agent: true, namespace_template: "{agent}" });
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "alice");
  assert.equal(effectiveNamespace(cfg, { sessionKey: "agent:carol:main" }), "carol");
});

test("effectiveNamespace: per-agent mode falls back to base when no id", () => {
  const cfg = baseCfg({ namespace_per_agent: true, namespace_template: "{agent}" });
  assert.equal(effectiveNamespace(cfg, {}), "openclaw");
});

test("applyTemplate: substitutes namespace/agent and collapses dropped-segment slashes", () => {
  assert.equal(applyTemplate("{namespace}-{agent}", { namespace: "openclaw", agent: "alice" }), "openclaw-alice");
  assert.equal(applyTemplate("{agent}", { agent: "alice" }), "alice");
  assert.equal(applyTemplate("{tenant}/{project}/{agent}", { agent: "alice" }), "alice");
  assert.equal(applyTemplate("{namespace}/{agent}", { namespace: "work/openclaw", agent: "miso" }), "work/openclaw/miso");
});

test("detectSystemKind: reads ctx.trigger, case-insensitive, exact match", () => {
  assert.equal(detectSystemKind({ trigger: "heartbeat" }), "heartbeat");
  assert.equal(detectSystemKind({ trigger: "CRON" }), "cron");
  // "user" turns and unknown triggers are not system turns
  assert.equal(detectSystemKind({ trigger: "user" }), "");
  assert.equal(detectSystemKind({ trigger: "budget" }), "");
  assert.equal(detectSystemKind({}), "");
});

test("shouldSkipSystemTurn: gates only when both flags are on", () => {
  assert.equal(shouldSkipSystemTurn(baseCfg({ skip_system_turns: false }), { trigger: "heartbeat" }), false);
  assert.equal(shouldSkipSystemTurn(baseCfg({ skip_system_turns: true }), { trigger: "user" }), false);
  assert.equal(shouldSkipSystemTurn(baseCfg({ skip_system_turns: true }), { trigger: "heartbeat" }), true);
});

test("sessionIdentity: prefers sessionId over sessionKey/runId", () => {
  assert.equal(
    sessionIdentity({ sessionId: "sess-1", runId: "r-2", sessionKey: "agent:alice:cron:x" }),
    "sess-1",
  );
});

test("sessionIdentity: empty when no identifier resolves", () => {
  assert.equal(sessionIdentity({}), "");
  assert.equal(sessionIdentity({ runId: "" }), "");
});

test("sessionIdentity: never falls back to the agent id (that would be too coarse)", () => {
  assert.equal(sessionIdentity({ agentId: "alice" }), "");
});

test("stripRuntimePreambles: drops a single untrusted-metadata block, keeps the message", () => {
  const text = "Conversation info (untrusted metadata):\n```json\n{\"chat_id\":1}\n```\nhello there";
  assert.equal(stripRuntimePreambles(text), "hello there");
});

test("stripRuntimePreambles: drops multiple stacked blocks", () => {
  const text =
    "Chat info (untrusted metadata):\n```\n{}\n```\n" +
    "Sender info (untrusted metadata):\n```json\n{}\n```\n" +
    "actual message";
  assert.equal(stripRuntimePreambles(text), "actual message");
});

test("stripRuntimePreambles: leaves a normal message untouched", () => {
  assert.equal(stripRuntimePreambles("plain message"), "plain message");
  assert.equal(stripRuntimePreambles(""), "");
});

// Ground truth: the exact shape of a cron turn found captured in production —
// "User: [cron:<uuid> <name>] <task>". The role label defeats a bare
// startsWith("[cron:"), which is how this leaked into the corpus.
test("startsWithNoisePrefix: real production cron turn behind a User: label is noise", () => {
  const text =
    "User: [cron:b571428f-243c-4604-919e-effb800d44c0 homelab-peers-commits] " +
    "You are a homelab commit watcher. Check these repos and post to Discord.";
  assert.equal(startsWithNoisePrefix(text), true);
});

test("startsWithNoisePrefix: bare and role-labelled markers both match", () => {
  assert.equal(startsWithNoisePrefix("[cron:abc job] do a thing"), true);
  assert.equal(startsWithNoisePrefix("User: [Subagent Context] delegated task"), true);
  assert.equal(startsWithNoisePrefix("Assistant: [cron:x] ..."), true);
});

test("startsWithNoisePrefix: a real message that merely mentions a marker is kept", () => {
  assert.equal(startsWithNoisePrefix("User: how do I write a [cron: job]?"), false);
  assert.equal(startsWithNoisePrefix("please set up a cron for backups"), false);
});

test("resolveConfig: min_capture_chars off by default; config and env override", () => {
  assert.equal(resolveConfig({}).min_capture_chars, 0);
  assert.equal(resolveConfig({ min_capture_chars: 30 }).min_capture_chars, 30);
  process.env.MEMINI_MIN_CAPTURE_CHARS = "25";
  assert.equal(resolveConfig({}).min_capture_chars, 25);
  delete process.env.MEMINI_MIN_CAPTURE_CHARS;
});

test("resolveConfig: home is unset by default; config wins over MEMINI_HOME env", () => {
  assert.equal(resolveConfig({}).home, undefined);
  process.env.MEMINI_HOME = "personal/acme";
  assert.equal(resolveConfig({}).home, "personal/acme");
  assert.equal(resolveConfig({ home: "personal/other" }).home, "personal/other");
  delete process.env.MEMINI_HOME;
});

test("stripRuntimePreambles: returns empty when the turn is only metadata", () => {
  const text = "Conversation info (untrusted metadata):\n```\n{}\n```";
  assert.equal(stripRuntimePreambles(text), "");
});

// --- Flat-format tests (regression: original regex only matched fenced blocks) ---

test("stripRuntimePreambles: flat-format single block strips metadata, keeps message", () => {
  const text =
    "Conversation info (untrusted metadata):\n" +
    "chat_id=abc\n" +
    "message_id=def\n" +
    "sender_id=ghi\n" +
    "group_space=channel\n" +
    "is_group_chat=false\n" +
    "User: Hello world";
  assert.equal(stripRuntimePreambles(text), "User: Hello world");
});

test("stripRuntimePreambles: flat-format multiple consecutive blocks stripped", () => {
  const text =
    "Conversation info (untrusted metadata):\n" +
    "chat_id=abc\n" +
    "message_id=def\n" +
    "Sender (untrusted metadata):\n" +
    "sender_id=ghi\n" +
    "User: Hello world";
  assert.equal(stripRuntimePreambles(text), "User: Hello world");
});

test("stripRuntimePreambles: mixed format — one fenced block, one flat block", () => {
  const text =
    "Conversation info (untrusted metadata):\n" +
    "```json\n" +
    '{"chat_id": "abc"}\n' +
    "```\n" +
    "Sender (untrusted metadata):\n" +
    "sender_id=ghi\n" +
    "User: Hello world";
  assert.equal(stripRuntimePreambles(text), "User: Hello world");
});

test("stripRuntimePreambles: flat format, metadata only (no actual message) → empty", () => {
  const text =
    "Conversation info (untrusted metadata):\n" +
    "chat_id=abc\n" +
    "message_id=def";
  assert.equal(stripRuntimePreambles(text), "");
});

test("stripRuntimePreambles: user message containing `=` without metadata label is untouched", () => {
  assert.equal(stripRuntimePreambles("User: Set foo=bar please"), "User: Set foo=bar please");
});

test("stripRuntimePreambles: flat block, no blank line, following message contains `=` — kept (no over-strip)", () => {
  // Regression: a greedy flat matcher (`[^\n]*=[^\n]*`) would swallow the
  // "User: set FOO=bar" line too, because it also contains `=`. The key anchor
  // stops the run at the first line that isn't a bare key=value.
  const text =
    "Conversation info (untrusted metadata):\n" +
    "chat_id=abc\n" +
    "message_id=def\n" +
    "User: set FOO=bar please";
  assert.equal(stripRuntimePreambles(text), "User: set FOO=bar please");
});

test("stripRuntimePreambles: fenced format still works (regression)", () => {
  const text = "Conversation info (untrusted metadata):\n```json\n{\"chat_id\":1}\n```\nhello there";
  assert.equal(stripRuntimePreambles(text), "hello there");
});

// Production format: User: prefix, pretty-printed JSON, nested objects.

test("stripRuntimePreambles: production format — User: prefix + pretty-printed JSON", () => {
  const text = [
    "User: Conversation info (untrusted metadata):",
    "```json",
    "{",
    '  "chat_id": "C12345678",',
    '  "message_id": "M98765432",',
    '  "sender": {',
    '    "id": "U123456",',
    '    "name": "Alice",',
    '    "username": "alice"',
    "  },",
    '  "group_space": "general",',
    '  "is_group_chat": false',
    "}",
    "```",
    "",
    "User: Hello does memini work fine?",
  ].join("\n");
  assert.equal(stripRuntimePreambles(text), "User: Hello does memini work fine?");
});

test("stripRuntimePreambles: production format — multiple blocks", () => {
  const text = [
    "User: Conversation info (untrusted metadata):",
    "```json",
    "{",
    '  "chat_id": "C123",',
    '  "message_id": "M456",',
    "}",
    "```",
    "",
    "Location (untrusted metadata):",
    "```json",
    "{",
    '  "latitude": 37.7749,',
    '  "longitude": -122.4194',
    "}",
    "```",
    "",
    "User: What's the weather?",
  ].join("\n");
  assert.equal(stripRuntimePreambles(text), "User: What's the weather?");
});

test("stripRuntimePreambles: production format — metadata only → empty", () => {
  const text = [
    "User: Conversation info (untrusted metadata):",
    "```json",
    "{",
    '  "chat_id": "C123",',
    '  "is_group_chat": true',
    "}",
    "```",
  ].join("\n");
  assert.equal(stripRuntimePreambles(text), "");
});

test("meminiListPath: tiers, tags, metadata, limit", () => {
  assert.equal(
    meminiListPath({ tiers: ["procedural"], tags: ["auth"], metadata: { category: "bug_fixes" }, limit: 20 }),
    "/v1/memories?tier=procedural&tag=auth&meta=category%3Dbug_fixes&limit=20",
  );
  assert.equal(meminiListPath({}), "/v1/memories");
  assert.equal(meminiListPath({ limit: 0 }), "/v1/memories");
  assert.equal(meminiListPath({ limit: 1.5 }), "/v1/memories");
});

test("createPlaintextBearerAuthGuard: no-op when no secret", () => {
  const warns: string[] = [];
  const guard = createPlaintextBearerAuthGuard((m: string) => warns.push(m));
  guard("http://example.com", undefined);
  assert.deepEqual(warns, []);
});

test("createPlaintextBearerAuthGuard: warns once on first call with secret over http", () => {
  const warns: string[] = [];
  const guard = createPlaintextBearerAuthGuard((m: string) => warns.push(m));
  guard("http://example.com", "tok");
  guard("http://example.com", "tok");
  guard("http://example.com", "tok");
  assert.equal(warns.length, 1);
  assert.match(warns[0], /plaintext HTTP/);
  assert.match(warns[0], /example\.com/);
});

test("createPlaintextBearerAuthGuard: loopback plaintext is allowed", () => {
  const warns: string[] = [];
  const guard = createPlaintextBearerAuthGuard((m: string) => warns.push(m));
  guard("http://localhost:8080", "tok");
  guard("http://127.0.0.1:8080", "tok");
  guard("http://[::1]:8080", "tok");
  assert.deepEqual(warns, []);
});

test("createPlaintextBearerAuthGuard: https never warns", () => {
  const warns: string[] = [];
  const guard = createPlaintextBearerAuthGuard((m: string) => warns.push(m));
  guard("https://example.com", "tok");
  assert.deepEqual(warns, []);
});

test("createPlaintextBearerAuthGuard: MEMINI_REQUIRE_HTTPS=1 throws", () => {
  const guard = createPlaintextBearerAuthGuard((_m: string) => {}, { MEMINI_REQUIRE_HTTPS: "1" });
  assert.throws(() => guard("http://example.com", "tok"), /plaintext HTTP/);
});

type ToolDef = {
  name: string;
  description: string;
  parameters: unknown;
  execute: (id: string, params: Record<string, unknown>) => Promise<{ content: { type: string; text: string }[] }>;
};

type ToolFactory = (ctx: Record<string, unknown>) => ToolDef | ToolDef[];
type RegisterOpts = { optional?: boolean; names?: string[] };

// collectTools materializes the registered factory with `ctx` — the per-agent
// OpenClawPluginToolContext OpenClaw hands the factory (identity lives here, not
// on the execute call). `hs` is the (possibly undefined) handshake this test's
// SessionContext's memo resolves to instantly.
function collectTools(
  hs: any = fakeHandshake(),
  pluginConfig: any = { namespace_per_agent: false },
  ctx: Record<string, unknown> = {},
) {
  let factory: ToolFactory | undefined;
  let opts: RegisterOpts | undefined;
  const api = {
    logger: { warn() {} },
    registerTool: (f: ToolFactory, o: RegisterOpts) => {
      factory = f;
      opts = o;
    },
  };
  const client = fakeClient();
  const sessionCtx = fixedSessionContext(pluginConfig, hs);
  registerMeminiTools(api, client, sessionCtx);
  const tools = factory ? ([] as ToolDef[]).concat(factory(ctx)) : [];
  return { byName: Object.fromEntries(tools.map((d) => [d.name, d])), opts, client, sessionCtx };
}

test("registerMeminiTools: registers one optional factory naming all tools", () => {
  const { opts } = collectTools();
  assert.equal(opts?.optional, true);
  assert.deepEqual(
    [...(opts?.names ?? [])].sort(),
    ["memory_briefing", "memory_forget", "memory_list", "memory_recall", "memory_remember"],
  );
});

test("registerMeminiTools: memory_recall shapes the request body from typed params", async () => {
  const { byName, client } = collectTools();
  await byName.memory_recall.execute("id", { query: "auth race", tags: ["auth"], metadata: { category: "bug_fixes" } });
  const call = client.calls.at(-1);
  assert.ok(call);
  assert.equal(call.method, "POST");
  assert.equal(call.path, "/v1/search");
  assert.deepEqual(call.body, {
    query: "auth race",
    limit: 3,
    tags: ["auth"],
    metadata: { category: "bug_fixes" },
  });
});

test("registerMeminiTools: memory_recall exposes scope and forwards only the known vocabulary", async () => {
  const { byName, client } = collectTools();
  const schema: any = byName.memory_recall.parameters;
  assert.deepEqual(schema.properties.scope.enum, ["project", "full", "everywhere"]);

  await byName.memory_recall.execute("id", { query: "q", scope: "everywhere" });
  assert.equal((client.calls.at(-1)?.body as any).scope, "everywhere");

  // Omitted: nothing on the wire, so the server's "full" default applies.
  await byName.memory_recall.execute("id", { query: "q" });
  assert.equal("scope" in (client.calls.at(-1)?.body as any), false);

  // The deprecated REST alias is not model-facing; forwarding it would 400.
  await byName.memory_recall.execute("id", { query: "q", scope: "subtree" });
  assert.equal("scope" in (client.calls.at(-1)?.body as any), false);
});

test("registerMeminiTools: memory_remember forwards visibility verbatim — the server owns the chain", async () => {
  const { byName, client } = collectTools();
  await byName.memory_remember.execute("id", { content: "fact", visibility: "personal" });
  assert.equal((client.calls.at(-1)?.body as any).visibility, "personal");

  // An ancestor name is in no client-side enum: only the server knows this
  // namespace's chain, so the name goes through untouched.
  await byName.memory_remember.execute("id", { content: "fact", visibility: "acme" });
  assert.equal((client.calls.at(-1)?.body as any).visibility, "acme");

  await byName.memory_remember.execute("id", { content: "fact" });
  assert.equal("visibility" in (client.calls.at(-1)?.body as any), false);
});

test("registerMeminiTools: memory_briefing GETs the header-scoped briefing and keeps the Scope line", async () => {
  const { byName, client } = collectTools();
  const out = await byName.memory_briefing.execute("id", { scope: "full" });
  const call = client.calls.at(-1);
  // Header-scoped endpoint: the namespace is never in the path.
  assert.equal(call?.path, "/v1/namespaces/briefing?scope=full");
  const res = JSON.parse(out.content[0].text);
  assert.equal(res.scope_header, "Scope: ns ← acme(4)");
  assert.equal(res.pinned[0].id, "p1");
  // Provenance survives: an inherited fact says which ancestor it came from, and
  // a primary-namespace one carries no "from" at all.
  assert.equal(res.facts[0].from, "acme");
  assert.equal("from" in res.pinned[0], false);
});

test("registerMeminiTools: memory_remember drops invalid/omitted tier so the server classifies", async () => {
  const { byName, client } = collectTools();
  await byName.memory_remember.execute("id", { content: "fact", tier: "bogus" });
  let call = client.calls.at(-1);
  assert.ok(call);
  assert.deepEqual(call.body, { content: "fact" });
  await byName.memory_remember.execute("id", { content: "fact" });
  call = client.calls.at(-1);
  assert.ok(call);
  assert.deepEqual(call.body, { content: "fact" });
  await byName.memory_remember.execute("id", { content: "fact", tier: "episodic" });
  call = client.calls.at(-1);
  assert.ok(call);
  assert.deepEqual(call.body, { content: "fact", tier: "episodic" });
});

test("registerMeminiTools: tool namespace resolves per-agent from the factory ctx", async () => {
  const { byName, client } = collectTools(
    fakeHandshake({ namespace: "team" }),
    { namespace_per_agent: true, namespace_template: "{namespace}-{agent}" },
    { agentId: "miso" },
  );
  await byName.memory_list.execute("id", {});
  assert.equal(client.calls.at(-1)?.ns, "team-miso");
});

test("resolveConfig: recall_max_tokens — config wins, else MEMINI_INJECT_RECALL_MAX_TOK, else 0 (uncapped)", () => {
  const prev = process.env.MEMINI_INJECT_RECALL_MAX_TOK;
  try {
    delete process.env.MEMINI_INJECT_RECALL_MAX_TOK;
    assert.equal(resolveConfig({}).recall_max_tokens, 0);
    assert.equal(resolveConfig({ recall_max_tokens: 120 }).recall_max_tokens, 120);
    process.env.MEMINI_INJECT_RECALL_MAX_TOK = "80";
    assert.equal(resolveConfig({}).recall_max_tokens, 80);
    // config still wins over the env fallback
    assert.equal(resolveConfig({ recall_max_tokens: 200 }).recall_max_tokens, 200);
    // malformed env falls back to the default
    process.env.MEMINI_INJECT_RECALL_MAX_TOK = "nope";
    assert.equal(resolveConfig({}).recall_max_tokens, 0);
  } finally {
    if (prev === undefined) delete process.env.MEMINI_INJECT_RECALL_MAX_TOK;
    else process.env.MEMINI_INJECT_RECALL_MAX_TOK = prev;
  }
});

// --- namespace resolution: env > handshake > config > "openclaw" -------------

function tmpProject(withGit = true): string {
  const dir = mkdtempSync(join(tmpdir(), "openclaw-memini-proj-"));
  if (withGit) {
    execSync("git init -q", { cwd: dir });
    execSync("git remote add origin https://example.com/acme/widget.git", { cwd: dir });
  }
  return dir;
}

test("resolveBaseNamespace: the default is still the literal openclaw, with no cwd derivation", () => {
  // Load-bearing: this is a gateway harness where the cwd is usually meaningless.
  // Deriving from it would silently relocate every existing install's memory.
  assert.deepEqual(resolveBaseNamespace(undefined, {}), { namespace: "openclaw", source: "default" });
  assert.deepEqual(resolveBaseNamespace({}, {}), { namespace: "openclaw", source: "default" });
});

test("resolveBaseNamespace: MEMINI_NAMESPACE is honored, and an explicit config value beats the default", () => {
  assert.deepEqual(resolveBaseNamespace({}, { MEMINI_NAMESPACE: "team/eu" }), { namespace: "team/eu", source: "env" });
  // Config loses to the env pin, wins over the default.
  assert.equal(resolveBaseNamespace({ namespace: "cfg" }, { MEMINI_NAMESPACE: "pinned" }).namespace, "pinned");
  assert.deepEqual(resolveBaseNamespace({ namespace: "cfg" }, {}), { namespace: "cfg", source: "config" });
});

test("gatewayFacts: cwd_basename + daemon-cwd toplevel_path + declared_namespace, no git derivation", () => {
  const facts = gatewayFacts({ namespace: "team" }, "/some/gateway/dir");
  assert.deepEqual(facts, {
    cwd_basename: "dir",
    toplevel_path: "/some/gateway/dir",
    declared_namespace: "team",
  });
  // declared_namespace comes from CONFIG, never from MEMINI_NAMESPACE.
  const noConfig = gatewayFacts({}, "/x/y");
  assert.deepEqual(noConfig, { cwd_basename: "y", toplevel_path: "/x/y", declared_namespace: "openclaw" });
});

test("gatewayFacts: toplevel_path is the RESOLVED raw cwd, never a git toplevel", () => {
  // A gateway running in a subdirectory of some repo must report ITS cwd, not
  // the repo root: the pin identity is this install, not whatever checkout it
  // happens to sit inside.
  const repo = tmpProject(true);
  const sub = join(repo, "some", "sub");
  const facts = gatewayFacts({}, sub);
  assert.equal(facts.toplevel_path, sub);
  assert.equal("remote_url" in facts, false, "never send a git remote");
});

test("pinKeyFacts: keys on the SAME toplevel_path the handshake facts carry", () => {
  // Write and lookup must agree on the identity: the pin key comes straight
  // off the session's own facts object.
  assert.deepEqual(pinKeyFacts({ cwd_basename: "d", toplevel_path: "/gw/dir", declared_namespace: "x" }), {
    toplevel_path: "/gw/dir",
  });
  assert.deepEqual(pinKeyFacts({ cwd_basename: "d" }), {});
});

test("effectiveConfig: MEMINI_NAMESPACE wins outright over a successful handshake", () => {
  const cfg = resolveConfig({}, { MEMINI_NAMESPACE: "pinned" });
  assert.equal(cfg.namespace_source, "env");
  const hs = fakeHandshake({ namespace: "acme/widget", namespace_source: "pin" });
  const live = effectiveConfig(cfg, hs, {});
  assert.equal(live.namespace, "pinned");
  assert.equal(live.namespace_source, "env");
  assert.equal(live.degraded, false, "the handshake DID succeed, even though env wins the namespace");
});

test("effectiveConfig: with no env pin, a successful handshake wins over config/default", () => {
  const cfg = resolveConfig({ namespace: "cfg" }, {});
  const hs = fakeHandshake({ namespace: "acme/widget", namespace_source: "declared" });
  const live = effectiveConfig(cfg, hs, {});
  assert.equal(live.namespace, "acme/widget");
  assert.equal(live.namespace_source, "server:declared");
  assert.equal(live.degraded, false);
});

test("effectiveConfig: fail-soft — no handshake falls back to the config/default value", () => {
  const cfg = resolveConfig({ namespace: "cfg" }, {});
  const live = effectiveConfig(cfg, undefined, {});
  assert.equal(live.namespace, "cfg");
  assert.equal(live.namespace_source, "config");
  assert.equal(live.degraded, true);

  const noConfig = resolveConfig({}, {});
  const liveDefault = effectiveConfig(noConfig, undefined, {});
  assert.equal(liveDefault.namespace, "openclaw");
  assert.equal(liveDefault.namespace_source, "default");
});

test("effectiveConfig: behavior knobs — config explicit wins, else env-override beats server beats default", () => {
  const cfg = resolveConfig({}, {});
  const hs = fakeHandshake({ settings: { recall_limit: 8, inject_recall_max_tok: 500, min_capture_chars: 40 } });
  const server = effectiveConfig(cfg, hs, {});
  assert.equal(server.recall_limit, 8);
  assert.equal(server.recall_max_tokens, 500);
  assert.equal(server.min_capture_chars, 40);

  // A config-explicit value is never overridden by the server.
  const cfgExplicit = resolveConfig({ recall_limit: 2 }, {});
  const liveExplicit = effectiveConfig(cfgExplicit, hs, {});
  assert.equal(liveExplicit.recall_limit, 2);

  // A local env override still wins over the server.
  const liveEnvOverride = effectiveConfig(cfg, hs, { MEMINI_RECALL_LIMIT: "1" });
  assert.equal(liveEnvOverride.recall_limit, 1);
});

test("prefix / per-agent template still apply on top of a resolved namespace", () => {
  const env = { MEMINI_NAMESPACE: "team" };
  const cfg = resolveConfig({ namespace_prefix: "work", namespace_template: "{namespace}-{agent}" }, env);
  assert.equal(cfg.namespace, "team");
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "work/team-miso");
});

// --- memoizeAsync: TTL + invalidate --------------------------------------------

test("memoizeAsync: calls fn once within the TTL window, refreshes after expiry", async () => {
  let calls = 0;
  let t = 1000;
  const memo = memoizeAsync(async () => { calls++; return calls; }, 1000, () => t);
  assert.equal(await memo.get(), 1);
  t += 500;
  assert.equal(await memo.get(), 1);
  t += 1000;
  assert.equal(await memo.get(), 2);
});

test("memoizeAsync: invalidate() forces the very next get() to refresh", async () => {
  let calls = 0;
  const memo = memoizeAsync(async () => { calls++; return calls; }, 60_000);
  assert.equal(await memo.get(), 1);
  assert.equal(await memo.get(), 1);
  memo.invalidate();
  assert.equal(await memo.get(), 2);
});

// --- fail-soft: a guard throw honors fallback_on_error ------------------------

test("sessionLive: MEMINI_REQUIRE_HTTPS guard throw degrades when fallback is on (default)", async () => {
  const cwd = tmpProject();
  const env = {
    MEMINI_BASE_URL: "http://example.com",
    MEMINI_API_KEY: "sk-test-token",
    MEMINI_REQUIRE_HTTPS: "1",
  };
  const ctx = createSessionContext({}, env, cwd);
  const live = await sessionLive(ctx, env);
  assert.equal(live.degraded, true);
});

test("sessionLive: fallback_on_error:false lets the guard throw propagate", async () => {
  const cwd = tmpProject();
  const env = {
    MEMINI_BASE_URL: "http://example.com",
    MEMINI_API_KEY: "sk-test-token",
    MEMINI_REQUIRE_HTTPS: "1",
  };
  const ctx = createSessionContext({ fallback_on_error: false }, env, cwd);
  await assert.rejects(() => sessionLive(ctx, env), /plaintext HTTP/);
});

test("sessionLive: a network failure always degrades regardless of fallback_on_error", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
  try {
    const env = { MEMINI_BASE_URL: "http://localhost:19999" };
    const ctx = createSessionContext({ fallback_on_error: false }, env, cwd);
    const live = await sessionLive(ctx, env);
    assert.equal(live.degraded, true);
  } finally {
    globalThis.fetch = realFetch;
  }
});

// --- registerMeminiCommands: memini:status / memini:namespace -----------------

function fakeApi() {
  const commands: Record<string, (ctx: any) => Promise<{ text: string }>> = {};
  const api = {
    logger: { warn() {} },
    registerCommand(def: any) {
      commands[def.name] = def.handler;
    },
  };
  return { api, commands };
}

test("registerMeminiCommands: registers memini:status and memini:namespace", () => {
  const { api, commands } = fakeApi();
  registerMeminiCommands(api, fixedSessionContext({}, undefined));
  assert.deepEqual(Object.keys(commands).sort(), ["memini:namespace", "memini:status"]);
});

test("memini:namespace (no args): shows the live namespace + pin details", async () => {
  const { api, commands } = fakeApi();
  const hs = fakeHandshake({ namespace: "acme/widget", namespace_source: "pin", pin: { key: "remote:x", created_by: "kit" } });
  registerMeminiCommands(api, fixedSessionContext({}, hs));
  const { text } = await commands["memini:namespace"]({});
  assert.match(text, /namespace: acme\/widget\s+\(source: server:pin\)/);
  assert.match(text, /pin:\s+key remote:x, set by kit/);
});

test("memini:namespace <ns>: PUTs a pin keyed by the daemon-cwd toplevel_path and invalidates the memo", async () => {
  // A plain non-git directory on purpose: the pin identity is the daemon's
  // cwd path, so no git repo is needed to pin a gateway install.
  const cwd = tmpProject(false);
  const realFetch = globalThis.fetch;
  const calls: any[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    const u = String(url);
    calls.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
    if (u.endsWith("/v1/pins")) {
      return { ok: true, status: 200, async json() { return { namespace: "acme/api", key: `path:${JSON.parse(init.body).toplevel_path}` }; } };
    }
    return { ok: false, status: 404, async json() { return {}; } };
  }) as any;
  try {
    let invalidated = 0;
    const ctx = createSessionContext({}, process.env, cwd);
    const originalInvalidate = ctx.memo.invalidate.bind(ctx.memo);
    ctx.memo.invalidate = () => { invalidated++; originalInvalidate(); };

    const { api, commands } = fakeApi();
    registerMeminiCommands(api, ctx);
    const { text } = await commands["memini:namespace"]({ args: "acme/api" });
    assert.match(text, /namespace pinned: acme\/api/);
    const put = calls.find((c) => c.method === "PUT");
    assert.ok(put, "expected a PUT /v1/pins call");
    assert.equal(put.body.namespace, "acme/api");
    // The pin key IS the handshake identity: the same toplevel_path
    // gatewayFacts sends on every handshake.
    assert.equal(put.body.toplevel_path, ctx.facts.toplevel_path);
    assert.equal("remote_url" in put.body, false, "pins never key on a git remote here");
    assert.equal(invalidated, 1, "pin write must invalidate the in-memory memo");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("a pin written via memini:namespace is resolved by this gateway's own next handshake", async () => {
  // The end-to-end loop the pin exists for: PUT the pin, memo invalidated,
  // and the very next handshake (same facts, matched by path:<daemon-cwd>)
  // resolves it as server:pin, beating the declared/config namespace.
  const cwd = tmpProject(false);
  const realFetch = globalThis.fetch;
  let pinned = false;
  const handshakeBodies: any[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    const u = String(url);
    if (u.endsWith("/v1/pins") && init?.method === "PUT") {
      pinned = true;
      return { ok: true, status: 200, async json() { return { namespace: "acme/api", key: "path:x" }; } };
    }
    if (u.endsWith("/v1/handshake")) {
      const body = JSON.parse(init.body);
      handshakeBodies.push(body);
      // The server's view: a pin (once written) matches the facts' toplevel_path
      // and beats the declared_namespace.
      const hs = pinned
        ? fakeHandshake({ namespace: "acme/api", namespace_source: "pin", pin: { key: `path:${body.project.toplevel_path}` } })
        : fakeHandshake({ namespace: body.project.declared_namespace, namespace_source: "declared" });
      return { ok: true, status: 200, async json() { return hs; } };
    }
    return { ok: false, status: 404, async json() { return {}; } };
  }) as any;
  try {
    const ctx = createSessionContext({ namespace: "team" }, {}, cwd);
    // Before the pin: the declared/config namespace stands.
    const before = await sessionLive(ctx, {});
    assert.equal(before.namespace, "team");
    assert.equal(before.namespace_source, "server:declared");

    const { api, commands } = fakeApi();
    registerMeminiCommands(api, ctx);
    await commands["memini:namespace"]({ args: "acme/api" });

    // After the pin: no TTL wait needed (the write invalidated the memo), and
    // the fresh handshake resolves the pin.
    const after = await sessionLive(ctx, {});
    assert.equal(after.namespace, "acme/api");
    assert.equal(after.namespace_source, "server:pin");
    assert.equal(after.degraded, false);
    // Every handshake carried the pin identity the write keyed on.
    assert.ok(handshakeBodies.every((b) => b.project.toplevel_path === ctx.facts.toplevel_path));
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace <ns>: refuses a header-injecting namespace instead of normalizing it", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  let putCalled = false;
  globalThis.fetch = (async (url: any, init: any) => {
    if (String(url).endsWith("/v1/pins") && init?.method === "PUT") putCalled = true;
    return { ok: false, status: 404, async json() { return {}; } };
  }) as any;
  try {
    const ctx = createSessionContext({}, process.env, cwd);
    const { api, commands } = fakeApi();
    registerMeminiCommands(api, ctx);
    const { text } = await commands["memini:namespace"]({ args: "evil\r\nX-Evil: 1" });
    assert.match(text, /invalid namespace/);
    assert.equal(putCalled, false);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace --clear: 404 reports nothing to clear; success invalidates the memo", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => ({ ok: false, status: 404, async json() { return {}; } })) as any;
  try {
    const ctx = createSessionContext({}, process.env, cwd);
    const { api, commands } = fakeApi();
    registerMeminiCommands(api, ctx);
    const { text } = await commands["memini:namespace"]({ args: "--clear" });
    assert.match(text, /nothing to clear/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace: an unreachable server on a pin write points at the config namespace value", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
  try {
    const ctx = createSessionContext({}, process.env, cwd);
    const { api, commands } = fakeApi();
    registerMeminiCommands(api, ctx);
    const { text } = await commands["memini:namespace"]({ args: "acme/api" });
    assert.match(text, /Could not reach the memini server/);
    assert.match(text, /config `namespace`/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:status reports an unreachable server rather than throwing into the host", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
  try {
    const { api, commands } = fakeApi();
    registerMeminiCommands(api, fixedSessionContext({}, undefined));
    const { text } = await commands["memini:status"]({});
    assert.match(text, /reachable\s+NO/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:status reports the read set, redacts the bearer, and reads a 404 /healthz as not-exposed", async () => {
  const realFetch = globalThis.fetch;
  const prevKey = process.env.MEMINI_API_KEY;
  const requests: { url: string; headers: any }[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    requests.push({ url: String(url), headers: init?.headers });
    if (String(url).includes("/v1/namespaces/readset")) {
      return {
        ok: true,
        status: 200,
        async json() {
          return { entries: [{ namespace: "openclaw-miso", origin: "self", tiers: [] }] };
        },
      };
    }
    // A remote memini behind an ingress routes only /v1 and /mcp: /healthz 404s
    // while the server is perfectly healthy. That is "not exposed", not "down".
    return { ok: false, status: 404, async json() { return {}; } };
  }) as any;
  try {
    process.env.MEMINI_API_KEY = "sk-abcdefghijklmnop4f2a";
    const { api, commands } = fakeApi();
    registerMeminiCommands(api, fixedSessionContext({}, fakeHandshake()));
    const { text } = await commands["memini:status"]({ agentId: "miso" });

    assert.match(text, /reachable\s+yes/);
    assert.match(text, /\/healthz not routed/);
    assert.match(text, /READ SET/);
    // A settings dump is the likeliest place a token gets pasted into an issue.
    assert.doesNotMatch(text, /sk-abcdefghijklmnop4f2a/);
    assert.match(text, /sk-…4f2a/);
    const readSet = requests.find((r) => r.url.includes("readset"))!;
    assert.equal(readSet.headers.Authorization, "Bearer sk-abcdefghijklmnop4f2a");
  } finally {
    globalThis.fetch = realFetch;
    if (prevKey === undefined) delete process.env.MEMINI_API_KEY;
    else process.env.MEMINI_API_KEY = prevKey;
  }
});

test("fitByTokens: maxTokens<=0 keeps everything; a tight cap drops the tail", () => {
  const items = ["- alpha beta", "- gamma delta", "- epsilon zeta"];
  // Unbounded: nothing dropped.
  assert.deepEqual(fitByTokens(items, 0), { items, dropped: 0 });
  // Each two-word bullet ≈ approxTokens; a cap that fits only the first.
  const oneCap = approxTokens(items[0]);
  const fit = fitByTokens(items, oneCap);
  assert.deepEqual(fit.items, [items[0]]);
  assert.equal(fit.dropped, 2);
  // Empty input is safe.
  assert.deepEqual(fitByTokens([], 50), { items: [], dropped: 0 });
});
