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
  registerMeminiCommands,
  registerMeminiTools,
  resolveBaseNamespace,
  resolveConfig,
  sessionIdentity,
  shouldSkipSystemTurn,
  startsWithNoisePrefix,
  stripRuntimePreambles,
  type ResolvedConfig,
} from "../src/index.ts";
import { readOverride, writeOverride } from "@memini/client";
import { mkdtempSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// A developer shell may export the real memini config; clear it so resolveConfig
// tests see the documented defaults (the plugin now reads MEMINI_BASE_URL /
// MEMINI_URL as a fallback under the plugin config). MEMINI_NAMESPACE is in the
// list because the namespace chain now honors it — an exported one (the fish
// universal variable this feature exists for) would otherwise fail every
// default-namespace assertion below.
for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_HOME", "MEMINI_NAMESPACE"]) {
  delete process.env[k];
}
// The override file lives under $XDG_CONFIG_HOME/memini; point it at a temp dir
// so the developer's real overrides.json (keyed by THIS repo, which is the
// gateway's cwd when the tests run) cannot reach the defaults below.
process.env.XDG_CONFIG_HOME = mkdtempSync(join(tmpdir(), "openclaw-memini-test-"));

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

// --- namespace chain: override > MEMINI_NAMESPACE > config > "openclaw" ------

function tmpEnv(extra: Record<string, string> = {}): Record<string, string | undefined> {
  return { XDG_CONFIG_HOME: mkdtempSync(join(tmpdir(), "openclaw-memini-xdg-")), ...extra };
}

function tmpProject(): string {
  // macOS exposes /var through the /private/var symlink; process.cwd() returns
  // the real path, so keep the override key identical to the command runtime.
  return realpathSync(mkdtempSync(join(tmpdir(), "openclaw-memini-proj-")));
}

test("resolveBaseNamespace: the default is still the literal openclaw, with no cwd derivation", () => {
  // Load-bearing: this is a gateway harness where the cwd is usually meaningless.
  // Deriving from it would silently relocate every existing install's memory.
  const cwd = tmpProject();
  assert.deepEqual(resolveBaseNamespace(undefined, tmpEnv(), cwd), { namespace: "openclaw", source: "default" });
  assert.deepEqual(resolveBaseNamespace({}, tmpEnv(), undefined), { namespace: "openclaw", source: "default" });
});

test("resolveBaseNamespace: MEMINI_NAMESPACE is honored, and an explicit config value beats the default", () => {
  const cwd = tmpProject();
  // The bug: the plugin used to ignore MEMINI_NAMESPACE entirely.
  assert.deepEqual(resolveBaseNamespace({}, tmpEnv({ MEMINI_NAMESPACE: "team/eu" }), cwd), {
    namespace: "team/eu",
    source: "env",
  });
  // Config loses to the env pin, wins over the default.
  assert.equal(resolveBaseNamespace({ namespace: "cfg" }, tmpEnv({ MEMINI_NAMESPACE: "pinned" }), cwd).namespace, "pinned");
  assert.deepEqual(resolveBaseNamespace({ namespace: "cfg" }, tmpEnv(), cwd), { namespace: "cfg", source: "config" });
});

test("resolveBaseNamespace: the override beats MEMINI_NAMESPACE and the config value", () => {
  const env = tmpEnv({ MEMINI_NAMESPACE: "global-pin" });
  const cwd = tmpProject();
  writeOverride(cwd, "acme/api", { env });

  assert.deepEqual(resolveBaseNamespace({ namespace: "cfg" }, env, cwd), { namespace: "acme/api", source: "override" });
  // Without it, the env pin is what a caller would have been stuck with — the
  // counterfactual line describeSettings prints, and the only way to see past a
  // file-backed override.
  assert.deepEqual(resolveBaseNamespace({ namespace: "cfg" }, env, cwd, { ignoreOverride: true }), {
    namespace: "global-pin",
    source: "env",
  });
  // …and resolveConfig, which is what the plugin actually runs on.
  assert.equal(resolveConfig({ namespace: "cfg" }, env, cwd).namespace, "acme/api");
});

test("prefix / per-agent template still apply on top of a resolved namespace", () => {
  const env = tmpEnv({ MEMINI_NAMESPACE: "team" });
  const cwd = tmpProject();
  const cfg = resolveConfig({ namespace_prefix: "work", namespace_template: "{namespace}-{agent}" }, env, cwd);
  assert.equal(cfg.namespace, "team");
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "work/team-miso");
});

// A minimal OpenClaw host: enough of the plugin api for the commands to register.
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
  registerMeminiCommands(api, resolveConfig({}), {});
  assert.deepEqual(Object.keys(commands).sort(), ["memini:namespace", "memini:status"]);
});

test("memini:namespace sets and clears the override; hooks and tools follow it live", async () => {
  const cwd = tmpProject();
  const prevXdg = process.env.XDG_CONFIG_HOME;
  const prevPwd = process.cwd();
  process.env.XDG_CONFIG_HOME = mkdtempSync(join(tmpdir(), "openclaw-memini-xdg-"));
  process.chdir(cwd); // the commands key the override off the gateway's cwd
  try {
    const { api, commands } = fakeApi();
    const cfg = resolveConfig({ namespace_per_agent: false });
    registerMeminiCommands(api, cfg, {});

    assert.match((await commands["memini:namespace"]({ args: "" })).text, /No override/);

    const set = await commands["memini:namespace"]({ args: "acme/api" });
    assert.match(set.text, /namespace override set: openclaw -> acme\/api/);
    assert.equal(readOverride(cwd, { env: process.env })?.namespace, "acme/api");
    // Every hook and tool re-reads cfg.namespace per call, so the next turn already
    // targets the override — no gateway restart, no split brain.
    assert.equal(cfg.namespace, "acme/api");
    assert.equal(effectiveNamespace(cfg, {}), "acme/api");

    const cleared = await commands["memini:namespace"]({ args: "--clear" });
    assert.match(cleared.text, /namespace override cleared: acme\/api -> openclaw/);
    assert.equal(cfg.namespace, "openclaw");
    assert.match((await commands["memini:namespace"]({ args: "--clear" })).text, /nothing to clear/);

    // The namespace rides on the X-Memini-Namespace header: CR/LF would split it.
    const bad = await commands["memini:namespace"]({ args: "evil\r\nX-Evil: 1" });
    assert.match(bad.text, /invalid namespace/);
    assert.equal(readOverride(cwd, { env: process.env }), undefined);
    assert.equal(cfg.namespace, "openclaw", "a rejected namespace must not be applied");
  } finally {
    process.chdir(prevPwd);
    if (prevXdg === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = prevXdg;
  }
});

test("memini:status reports the read set, redacts the bearer, and reads a 404 /healthz as not-exposed", async () => {
  const realFetch = globalThis.fetch;
  const prevKey = process.env.MEMINI_API_KEY;
  const requests: { url: string; headers: any }[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    requests.push({ url: String(url), headers: init?.headers });
    if (String(url).includes("/v1/namespaces/read-set")) {
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
    registerMeminiCommands(api, resolveConfig({}), {});
    const { text } = await commands["memini:status"]({ agentId: "miso" });

    assert.match(text, /reachable\s+yes/);
    assert.match(text, /\/healthz not routed/);
    assert.match(text, /READ SET/);
    // The per-agent template is applied to what this surface actually sends.
    assert.match(text, /this surface sends\s+openclaw-miso/);
    assert.match(text, /base\s+openclaw\s+<- default/);
    // A settings dump is the likeliest place a token gets pasted into an issue.
    assert.doesNotMatch(text, /sk-abcdefghijklmnop4f2a/);
    assert.match(text, /sk-…4f2a/);
    const readSet = requests.find((r) => r.url.includes("read-set"))!;
    assert.equal(readSet.headers["X-Memini-Namespace"], "openclaw-miso");
    assert.equal(readSet.headers.Authorization, "Bearer sk-abcdefghijklmnop4f2a");
  } finally {
    globalThis.fetch = realFetch;
    if (prevKey === undefined) delete process.env.MEMINI_API_KEY;
    else process.env.MEMINI_API_KEY = prevKey;
  }
});

test("memini:status reports an unreachable server rather than throwing into the host", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => {
    throw new Error("ECONNREFUSED");
  }) as any;
  try {
    const { api, commands } = fakeApi();
    registerMeminiCommands(api, resolveConfig({}), {});
    const { text } = await commands["memini:status"]({});
    assert.match(text, /reachable\s+NO/);
  } finally {
    globalThis.fetch = realFetch;
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
