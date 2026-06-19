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
  stripRuntimePreambles,
  type ResolvedConfig,
} from "../src/index.ts";

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
  assert.equal(cfg.skip_system_turns, false);
  assert.deepEqual(cfg.system_kinds, ["cron", "heartbeat", "scheduled", "schedule"]);
  assert.equal(cfg.fallback_on_error, true);
  assert.equal(cfg.timeout_ms, 5000);
  assert.equal(cfg.expose_tools, false);
  assert.equal(cfg.recall_limit, 5);
  assert.equal(cfg.recall_min_score, 0);
});

test("resolveConfig: explicit recall knobs survive, zero/negative/non-finite fall back", () => {
  const cfg = resolveConfig({ recall_limit: 12, recall_min_score: 0.4 });
  assert.equal(cfg.recall_limit, 12);
  assert.equal(cfg.recall_min_score, 0.4);

  const zeroed = resolveConfig({ recall_limit: 0, recall_min_score: 0 });
  assert.equal(zeroed.recall_limit, 5);
  assert.equal(zeroed.recall_min_score, 0);

  const nan = resolveConfig({ recall_limit: Number.NaN, recall_min_score: Number.POSITIVE_INFINITY });
  assert.equal(nan.recall_limit, 5);
  assert.equal(nan.recall_min_score, 0);
});

test("effectiveNamespace: return type is `string | null`", () => {
  const a: string | null = effectiveNamespace(baseCfg({ namespace_per_agent: true, namespace_template: "{agent}" }), { agentId: "alice" }, {});
  assert.equal(a, "alice");

  const b: string | null = effectiveNamespace(
    baseCfg({ namespace_per_agent: true, skip_without_agent: true, namespace_template: "{agent}" }),
    {},
    {},
  );
  assert.equal(b, null);

  const c: string | null = effectiveNamespace(baseCfg({ namespace_per_agent: false }), { agentId: "alice" }, {});
  assert.equal(c, "openclaw");
});

test("effectiveNamespace: ctx identity wins over event identity", () => {
  const cfg = baseCfg({ namespace_per_agent: true, namespace_template: "{agent}" });
  assert.equal(effectiveNamespace(cfg, { agentName: "carol" }, { agentId: "alice" }), "alice");
});

test("effectiveNamespace: per-agent mode falls back to base when no id", () => {
  const cfg = baseCfg({ namespace_per_agent: true, namespace_template: "{agent}" });
  assert.equal(effectiveNamespace(cfg, {}, {}), "openclaw");
});

test("detectSystemKind: explicit field wins over session-key segment", () => {
  const k = detectSystemKind({ kind: "scheduled" }, { sessionKey: "agent:alice:cron:abc" }, "");
  assert.equal(k, "scheduled");
});

test("detectSystemKind: session-key whole-segment match only (no substring)", () => {
  // An agent id that contains "cron" must not match.
  assert.equal(detectSystemKind({}, { sessionKey: "agent:cron-master:abc" }, ""), "");
  assert.equal(detectSystemKind({}, { sessionKey: "agent:alice:cron:abc" }, ""), "cron");
});

test("detectSystemKind: leading bracketed marker is recognized", () => {
  assert.equal(detectSystemKind({}, {}, "[OpenClaw heartbeat poll] do stuff"), "heartbeat");
  // A bracket mid-message is ignored.
  assert.equal(detectSystemKind({}, {}, "the user wrote [cron task] in chat"), "");
});

test("shouldSkipSystemTurn: gates only when both flags are on", () => {
  assert.equal(shouldSkipSystemTurn(baseCfg({ skip_system_turns: false }), { kind: "cron" }, {}, "hi"), false);
  assert.equal(shouldSkipSystemTurn(baseCfg({ skip_system_turns: true }), { kind: "user" }, {}, "hi"), false);
  assert.equal(shouldSkipSystemTurn(baseCfg({ skip_system_turns: true }), { kind: "scheduled" }, {}, "hi"), true);
});

test("sessionIdentity: prefers session ids over run/sessionKey", () => {
  assert.equal(
    sessionIdentity(
      { sessionId: "sess-1", runId: "r-2", sessionKey: "agent:alice:cron:x" },
      {},
    ),
    "sess-1",
  );
});

test("sessionIdentity: empty when no identifier resolves", () => {
  assert.equal(sessionIdentity({}, {}), "");
  assert.equal(sessionIdentity({ runId: "" }, {}), "");
});

test("sessionIdentity: never falls back to the agent id (that would be too coarse)", () => {
  assert.equal(sessionIdentity({ agentId: "alice" }, { agentId: "alice" }), "");
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

test("stripRuntimePreambles: returns empty when the turn is only metadata", () => {
  const text = "Conversation info (untrusted metadata):\n```\n{}\n```";
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
  execute: (id: string, params: Record<string, unknown>, ctx: Record<string, unknown>) => Promise<{ content: { type: string; text: string }[] }>;
};

async function collectTools(cfg: ResolvedConfig = baseCfg()) {
  const defs: ToolDef[] = [];
  const optsByName: Record<string, unknown> = {};
  const api = {
    registerTool: (def: ToolDef, opts: unknown) => {
      defs.push(def);
      optsByName[def.name] = opts;
    },
  };
  const client = fakeClient();
  await registerMeminiTools(api, client, cfg);
  return { byName: Object.fromEntries(defs.map((d) => [d.name, d])), opts: optsByName, client };
}

test("registerMeminiTools: registers three tools, all marked optional", async () => {
  const { opts } = await collectTools();
  for (const name of ["memory_recall", "memory_list", "memory_remember"]) {
    assert.deepEqual(opts[name], { optional: true }, `${name} should be optional`);
  }
});

test("registerMeminiTools: memory_recall shapes the request body from typed params", async () => {
  const { byName, client } = await collectTools();
  await byName.memory_recall.execute("id", { query: "auth race", tags: ["auth"], metadata: { category: "bug_fixes" } }, {});
  const call = client.calls.at(-1);
  assert.ok(call);
  assert.equal(call.method, "POST");
  assert.equal(call.path, "/v1/search");
  assert.deepEqual(call.body, {
    query: "auth race",
    limit: 5,
    tags: ["auth"],
    metadata: { category: "bug_fixes" },
  });
});

test("registerMeminiTools: memory_remember validates tier and falls back to semantic", async () => {
  const { byName, client } = await collectTools();
  await byName.memory_remember.execute("id", { content: "fact", tier: "bogus" }, {});
  let call = client.calls.at(-1);
  assert.ok(call);
  assert.deepEqual(call.body, { content: "fact", tier: "semantic" });
  await byName.memory_remember.execute("id", { content: "fact", tier: "episodic" }, {});
  call = client.calls.at(-1);
  assert.ok(call);
  assert.deepEqual(call.body, { content: "fact", tier: "episodic" });
});

test("resolveConfig: recall_max_tokens — config wins, else MEMINI_INJECT_RECALL_MAX_TOK, else 0", () => {
  const prev = process.env.MEMINI_INJECT_RECALL_MAX_TOK;
  try {
    delete process.env.MEMINI_INJECT_RECALL_MAX_TOK;
    assert.equal(resolveConfig({}).recall_max_tokens, 0);
    assert.equal(resolveConfig({ recall_max_tokens: 120 }).recall_max_tokens, 120);
    process.env.MEMINI_INJECT_RECALL_MAX_TOK = "80";
    assert.equal(resolveConfig({}).recall_max_tokens, 80);
    // config still wins over the env fallback
    assert.equal(resolveConfig({ recall_max_tokens: 200 }).recall_max_tokens, 200);
    // malformed env falls back to 0
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
