// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  MeminiPlugin,
  resolveConfig,
  deriveNamespace,
  extractPartsText,
  formatResults,
  extractLastTurn,
  lastAssistantFailed,
  createPlaintextBearerAuthGuard,
  intEnv,
  floatEnv,
  approxTokens,
  fitByTokens,
  truncate,
} from "./memini.js";

test("namespace derives from the git worktree basename", () => {
  assert.equal(deriveNamespace("/home/me/dev/memini"), "memini");
  assert.equal(deriveNamespace("/home/me/dev/memini/"), "memini");
  assert.equal(deriveNamespace(""), "");
});

test("config defaults: recall and capture on, project-scoped namespace", () => {
  const cfg = resolveConfig({}, undefined, "/home/me/dev/my-project");
  assert.equal(cfg.base_url, "http://localhost:8080");
  assert.equal(cfg.namespace, "my-project");
  assert.equal(cfg.recall, true);
  assert.equal(cfg.capture, true);
  assert.equal(cfg.recall_limit, 5);
  assert.equal(cfg.recall_max_tokens, 0);
  assert.equal(cfg.recall_min_score, 0);
  assert.equal(cfg.fallback_on_error, true);
});

test("env overrides defaults; options override env", () => {
  const env = { MEMINI_BASE_URL: "http://memini:9000", MEMINI_NAMESPACE: "team", MEMINI_RECALL: "0" };
  const fromEnv = resolveConfig(env, undefined, "/repo/ignored");
  assert.equal(fromEnv.base_url, "http://memini:9000");
  assert.equal(fromEnv.namespace, "team");
  assert.equal(fromEnv.recall, false);

  const fromOpts = resolveConfig(env, { namespace: "explicit", base_url: "http://x" }, "/repo");
  assert.equal(fromOpts.namespace, "explicit");
  assert.equal(fromOpts.base_url, "http://x");
});

test("base_url falls back to the MEMINI_URL alias; MEMINI_BASE_URL canonical wins", () => {
  assert.equal(resolveConfig({ MEMINI_URL: "http://alias:8080" }, undefined, "/r").base_url, "http://alias:8080");
  const both = { MEMINI_BASE_URL: "http://canonical:8080", MEMINI_URL: "http://alias:8080" };
  assert.equal(resolveConfig(both, undefined, "/r").base_url, "http://canonical:8080");
});

test("namespace falls back to the default when nothing resolves", () => {
  assert.equal(resolveConfig({}, undefined, "").namespace, "opencode");
});

test("capture can be disabled via env", () => {
  assert.equal(resolveConfig({ MEMINI_CAPTURE: "false" }, undefined, "/r").capture, false);
});

test("resolveConfig honours the MEMINI_INJECT_RECALL_* budget knobs", () => {
  // intEnv/floatEnv read from process.env, not the env arg, so mutate
  // process.env around the test (mirrors the intEnv / floatEnv test).
  process.env["MEMINI_INJECT_RECALL_MAX_TOK"] = "1500";
  process.env["MEMINI_INJECT_RECALL_MIN_SCORE"] = "0.25";
  try {
    const cfg = resolveConfig({}, undefined, "/r");
    assert.equal(cfg.recall_max_tokens, 1500);
    assert.equal(cfg.recall_min_score, 0.25);
  } finally {
    delete process.env["MEMINI_INJECT_RECALL_MAX_TOK"];
    delete process.env["MEMINI_INJECT_RECALL_MIN_SCORE"];
  }
});

test("resolveConfig: inline options win over MEMINI_INJECT_RECALL_* env", () => {
  process.env["MEMINI_INJECT_RECALL_MAX_TOK"] = "1000";
  try {
    const cfg = resolveConfig({}, { recall_max_tokens: 2500, recall_min_score: 0.5 }, "/r");
    assert.equal(cfg.recall_max_tokens, 2500);
    assert.equal(cfg.recall_min_score, 0.5);
  } finally {
    delete process.env["MEMINI_INJECT_RECALL_MAX_TOK"];
  }
});

test("resolveConfig rejects malformed recall_limit (NaN / negative) gracefully", () => {
  assert.equal(
    resolveConfig({ MEMINI_RECALL_LIMIT: "abc" }, undefined, "/r").recall_limit,
    5,
  );
  assert.equal(
    resolveConfig({}, { recall_limit: "garbage" }, "/r").recall_limit,
    5,
  );
});

test("extractPartsText skips synthetic and ignored parts", () => {
  const parts = [
    { type: "text", text: "real question", synthetic: false },
    { type: "text", text: "injected memory", synthetic: true },
    { type: "text", text: "muted", ignored: true },
    { type: "tool", text: "not text" },
  ];
  assert.equal(extractPartsText(parts), "real question");
  assert.equal(extractPartsText(undefined), "");
});

test("formatResults renders a bullet list with tier and truncation", () => {
  const results = [
    { memory: { summary: "uses postgres", tier: "semantic" } },
    { memory: { content: "fixed the race", tier: "episodic" } },
  ];
  const bullets = formatResults(results, 5);
  assert.deepEqual(bullets, ["- (semantic) uses postgres", "- (episodic) fixed the race"]);
  assert.deepEqual(formatResults([], 5), []);
  assert.deepEqual(formatResults(undefined, 5), []);
});

test("formatResults respects the limit", () => {
  const results = Array.from({ length: 8 }, (_, i) => ({ memory: { content: `m${i}`, tier: "t" } }));
  assert.equal(formatResults(results, 3).length, 3);
});

test("formatResults uses the labels prefix when MEMINI_INJECT_LABELS is non-empty", () => {
  // Mirrors the Claude Code plugin's formatMemory template in
  // plugin/scripts/session-start.mjs.
  const results = [
    { memory: { summary: "uses postgres", tier: "semantic", confidence: 0.91 } },
  ];
  const tierOnly = new Set(["tier"]);
  const withConf = new Set(["tier", "confidence"]);
  assert.equal(
    formatResults(results, 5, tierOnly)[0],
    "[semantic] uses postgres",
  );
  assert.equal(
    formatResults(results, 5, withConf)[0],
    "[semantic · conf=0.91] uses postgres",
  );
  // No labels set -> the bare "- (tier) text" shape is preserved.
  assert.equal(
    formatResults(results, 5, new Set())[0],
    "- (semantic) uses postgres",
  );
});

test("intEnv / floatEnv parse user input safely", () => {
  assert.equal(intEnv("MEMINI_INJECT_TEST_X", 5), 5);
  process.env["MEMINI_INJECT_TEST_X"] = "42";
  assert.equal(intEnv("MEMINI_INJECT_TEST_X", 5), 42);
  process.env["MEMINI_INJECT_TEST_X"] = "garbage";
  assert.equal(intEnv("MEMINI_INJECT_TEST_X", 5), 5);
  process.env["MEMINI_INJECT_TEST_X"] = "-3";
  assert.equal(intEnv("MEMINI_INJECT_TEST_X", 5), 5);
  delete process.env["MEMINI_INJECT_TEST_X"];

  process.env["MEMINI_INJECT_TEST_Y"] = "0.5";
  assert.equal(floatEnv("MEMINI_INJECT_TEST_Y", 0), 0.5);
  process.env["MEMINI_INJECT_TEST_Y"] = "junk";
  assert.equal(floatEnv("MEMINI_INJECT_TEST_Y", 0), 0);
  delete process.env["MEMINI_INJECT_TEST_Y"];
});

test("approxTokens / fitByTokens / truncate match the Claude Code plugin's contracts", () => {
  assert.equal(approxTokens(""), 0);
  // ceil(1 * 4/3) = 2; the floor of 1 only kicks in for zero-word strings.
  assert.equal(approxTokens("hello"), 2);
  assert.equal(approxTokens("a b c d e f g h i j k l"), Math.ceil((12 * 4) / 3));

  // max=0 is the "no cap" sentinel; fitByTokens returns the full list
  // (back-compat for callers that pass 0 to mean "unbounded").
  const all = fitByTokens(["one", "two", "three"], 0);
  assert.equal(all.dropped, 0);
  assert.equal(all.items.length, 3);

  const trimmed = fitByTokens(
    ["alpha beta gamma", "delta epsilon zeta", "eta theta iota"],
    5,
  );
  assert.ok(trimmed.dropped >= 1);
  assert.ok(trimmed.items.length <= 2);

  assert.ok(truncate("x".repeat(500), 10).endsWith("[...truncated]"));
  assert.equal(truncate("short", 100), "short");
});

test("extractLastTurn takes the latest user and assistant text plus assistant id", () => {
  const messages = [
    { info: { role: "user" }, parts: [{ type: "text", text: "first" }] },
    { info: { role: "assistant", id: "a1" }, parts: [{ type: "text", text: "ans1" }] },
    { info: { role: "user" }, parts: [{ type: "text", text: "second" }] },
    {
      info: { role: "assistant", id: "a2" },
      parts: [
        { type: "text", text: "injected", synthetic: true },
        { type: "text", text: "ans2" },
      ],
    },
  ];
  assert.deepEqual(extractLastTurn(messages), {
    userText: "second",
    assistantText: "ans2",
    assistantID: "a2",
  });
});

test("extractLastTurn handles empty input", () => {
  assert.deepEqual(extractLastTurn(undefined), { userText: "", assistantText: "", assistantID: "" });
});

test("lastAssistantFailed reads the latest assistant turn's error", () => {
  assert.equal(
    lastAssistantFailed([
      { info: { role: "user" }, parts: [] },
      { info: { role: "assistant", id: "a1", error: { name: "boom" } }, parts: [] },
    ]),
    true,
  );
  assert.equal(lastAssistantFailed([{ info: { role: "assistant", id: "a1" }, parts: [] }]), false);
  assert.equal(lastAssistantFailed(undefined), false);
});

test("plaintext bearer guard warns once for http to a non-loopback host", () => {
  const warnings = [];
  const guard = createPlaintextBearerAuthGuard((m) => warnings.push(m), {});
  guard("http://memini.example.com", "secret");
  guard("http://memini.example.com", "secret");
  assert.equal(warnings.length, 1);
});

test("plaintext bearer guard is silent for loopback and for https", () => {
  const warnings = [];
  const guard = createPlaintextBearerAuthGuard((m) => warnings.push(m), {});
  guard("http://localhost:8080", "secret");
  guard("https://memini.example.com", "secret");
  guard("http://memini.example.com", ""); // no secret
  assert.equal(warnings.length, 0);
});

test("plaintext bearer guard throws when MEMINI_REQUIRE_HTTPS=1", () => {
  const guard = createPlaintextBearerAuthGuard(() => {}, { MEMINI_REQUIRE_HTTPS: "1" });
  assert.throws(() => guard("http://memini.example.com", "secret"), /plaintext HTTP/);
});

test("chat.message recall excludes this session's own captures via exclude_metadata", async () => {
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    return { ok: true, async json() { return { results: [] }; }, async text() { return ""; } };
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    await hooks["chat.message"](
      { sessionID: "s1" },
      { parts: [{ type: "text", text: "how did we fix auth?", sessionID: "s1", messageID: "m1" }] },
    );
    const search = requests.find((r) => r.url.endsWith("/v1/search"));
    assert.ok(search, "should POST /v1/search");
    assert.deepEqual(JSON.parse(search.init.body).exclude_metadata, { session_id: "s1" });
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("chat.message caps the recall block by MEMINI_INJECT_RECALL_MAX_TOK", async () => {
  // Four short bullets (~12 words each ≈ 16 tokens) + max=20: only the head
  // bullet fits, the tail is dropped with the truncation footer. Budget is
  // passed as a plugin option to avoid process.env mutation.
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    async json() {
      return {
        results: Array.from({ length: 4 }, (_, i) => ({
          score: 1 - i * 0.05,
          memory: { tier: "semantic", summary: "bullet number " + i + " is here" },
        })),
      };
    },
    async text() { return ""; },
  });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080", recall_max_tokens: 20 },
    );
    const output = {
      parts: [{ type: "text", text: "user prompt", sessionID: "s1", messageID: "m1" }],
    };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    assert.equal(output.parts.length, 2, "synthetic part should be unshifted");
    const injected = output.parts[0];
    assert.equal(injected.synthetic, true);
    assert.match(injected.text, /\[\.\.\. \d+ item\(s\) truncated by token budget\]/);
    assert.ok(injected.text.includes("bullet number 0"));
    assert.ok(!injected.text.includes("bullet number 3"));
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("chat.message drops hits below MEMINI_INJECT_RECALL_MIN_SCORE", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    async json() {
      return {
        results: [
          { score: 0.9, memory: { tier: "semantic", summary: "high" } },
          { score: 0.1, memory: { tier: "episodic", summary: "low — should be filtered" } },
          { score: 0.5, memory: { tier: "procedural", summary: "mid" } },
        ],
      };
    },
    async text() { return ""; },
  });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080", recall_min_score: 0.4 },
    );
    const output = {
      parts: [{ type: "text", text: "user prompt", sessionID: "s1", messageID: "m1" }],
    };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    if (output.parts.length === 1) return; // no hits passed the floor
    const injected = output.parts[0];
    assert.ok(!injected.text.includes("low — should be filtered"));
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Neither hook may ever reject: opencode aborts the turn on a chat.message throw
// and raises an unhandled rejection on an event throw.
test("chat.message never rejects, even when the memini call throws", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => {
    throw new Error("network down");
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      // fallback_on_error:false makes postJson rethrow; the hook guard must still swallow.
      { base_url: "http://localhost:8080", fallback_on_error: false },
    );
    await assert.doesNotReject(
      hooks["chat.message"](
        { sessionID: "s1" },
        { parts: [{ type: "text", text: "q", sessionID: "s1", messageID: "m1" }] },
      ),
    );
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("event never rejects, even when client.session.messages throws", async () => {
  const client = {
    session: {
      messages: async () => {
        throw new Error("opencode server error");
      },
    },
  };
  const hooks = await MeminiPlugin(
    { client, worktree: "/tmp/proj", directory: "/tmp/proj" },
    { base_url: "http://localhost:8080" },
  );
  await assert.doesNotReject(
    hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } }),
  );
});
