// Run: tsx --test test/helpers.test.ts
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  approxTokens,
  createPlaintextBearerAuthGuard,
  detectSystemKind,
  effectiveNamespace,
  fitByTokens,
  meminiListPath,
  registerMeminiTools,
  resolveConfig,
  sessionIdentity,
  shouldSkipSystemTurn,
  startsWithNoisePrefix,
  stripRuntimePreambles,
  type ResolvedConfig,
} from "../src/index.ts";

// A developer shell may export the real memini config; clear it so resolveConfig
// tests see the documented defaults (the plugin now reads MEMINI_BASE_URL /
// MEMINI_URL as a fallback under the plugin config).
for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN"]) delete process.env[k];

type MeminiClient = Parameters<typeof registerMeminiTools>[1];

type RecordedCall = { method: string; path: string; body?: unknown; ns?: string };

function fakeClient(): MeminiClient & { calls: RecordedCall[] } {
  const calls: RecordedCall[] = [];
  return {
    calls,
    namespace: "ns",
    baseUrl: "http://localhost:8080",
    async postJson(path: string, body: unknown, ns?: string) {
      calls.push({ method: "POST", path, body, ns });
      return path.includes("search")
        ? { results: [{ memory: { content: "hit", summary: "", tier: "semantic" }, score: 0.9 }] }
        : { id: "m1" };
    },
    async getJson(path: string, ns?: string) {
      calls.push({ method: "GET", path, ns });
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

test("resolveConfig: defaults match the documented contract", () => {
  const cfg: ResolvedConfig = resolveConfig(undefined);
  assert.equal(cfg.enabled, true);
  assert.equal(cfg.base_url, "http://localhost:8080");
  assert.equal(cfg.namespace, "openclaw");
  assert.equal(cfg.namespace_per_agent, true);
  assert.equal(cfg.namespace_template, "{namespace}-{agent}");
  assert.equal(cfg.skip_without_agent, false);
  assert.equal(cfg.skip_system_turns, true);
  assert.deepEqual(cfg.system_kinds, ["heartbeat", "cron"]);
  assert.equal(cfg.fallback_on_error, true);
  assert.equal(cfg.timeout_ms, 5000);
  assert.equal(cfg.expose_tools, false);
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
// on the execute call).
function collectTools(cfg: ResolvedConfig = baseCfg(), ctx: Record<string, unknown> = {}) {
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
  registerMeminiTools(api, client, cfg);
  const tools = factory ? ([] as ToolDef[]).concat(factory(ctx)) : [];
  return { byName: Object.fromEntries(tools.map((d) => [d.name, d])), opts, client };
}

test("registerMeminiTools: registers one optional factory naming all tools", () => {
  const { opts } = collectTools();
  assert.equal(opts?.optional, true);
  assert.deepEqual([...(opts?.names ?? [])].sort(), ["memory_forget", "memory_list", "memory_recall", "memory_remember"]);
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
  const cfg = baseCfg({ namespace: "team", namespace_per_agent: true, namespace_template: "{namespace}-{agent}" });
  const { byName, client } = collectTools(cfg, { agentId: "miso" });
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
