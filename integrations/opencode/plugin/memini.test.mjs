// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, basename } from "node:path";
import {
  MeminiPlugin,
  resolveConfig,
  effectiveConfig,
  memoizeAsync,
  buildFacts,
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
  describeSettings,
  renderStatus,
  redactSecret,
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
  assert.equal(cfg.namespace_source, "local-worktree");
  assert.equal(cfg.recall, true);
  assert.equal(cfg.capture, true);
  assert.equal(cfg.recall_limit, 3);
  assert.equal(cfg.recall_max_tokens, 0);
  assert.equal(cfg.recall_min_score, 0);
  assert.equal(cfg.fallback_on_error, true);
});

test("env overrides defaults; options override env", () => {
  const env = { MEMINI_BASE_URL: "http://memini:9000", MEMINI_NAMESPACE: "team", MEMINI_RECALL: "0" };
  const fromEnv = resolveConfig(env, undefined, "/repo/ignored");
  assert.equal(fromEnv.base_url, "http://memini:9000");
  assert.equal(fromEnv.namespace, "team");
  assert.equal(fromEnv.namespace_source, "env");
  assert.equal(fromEnv.recall, false);

  const fromOpts = resolveConfig(env, { namespace: "explicit", base_url: "http://x" }, "/repo");
  assert.equal(fromOpts.namespace, "explicit");
  assert.equal(fromOpts.namespace_source, "option");
  assert.equal(fromOpts.base_url, "http://x");
});

test("namespace falls back to the default when nothing resolves", () => {
  const cfg = resolveConfig({}, undefined, "");
  assert.equal(cfg.namespace, "opencode");
  assert.equal(cfg.namespace_source, "local-default");
});

test("home resolves from MEMINI_HOME env; option wins over env; unset -> undefined", () => {
  assert.equal(resolveConfig({}, undefined, "/r").home, undefined);
  assert.equal(resolveConfig({ MEMINI_HOME: "personal/acme" }, undefined, "/r").home, "personal/acme");
  assert.equal(
    resolveConfig({ MEMINI_HOME: "personal/acme" }, { home: "personal/other" }, "/r").home,
    "personal/other",
  );
});

test("MEMINI_NAMESPACE is used raw-trimmed, not flattened", () => {
  // The server validates the header; a hierarchical value keeps its "/" so it
  // matches the other integrations instead of collapsing to team-eu.
  assert.equal(resolveConfig({ MEMINI_NAMESPACE: "  team/eu  " }, undefined, "/repo").namespace, "team/eu");
});

test("the local namespace fallback is the worktree basename only, never the git remote", async () => {
  // No config-file/tenant mechanism exists anymore: the LOCAL fallback
  // (absent option/env) is always the plain worktree basename, even inside a
  // git repo whose remote points at a differently-named project. Distinct
  // repo naming is now the server's job (via the handshake's facts.remote_url).
  const { execSync } = await import("node:child_process");
  const dir = mkdtempSync(join(tmpdir(), "memini-legacy-"));
  execSync("git init -q", { cwd: dir });
  execSync("git remote add origin https://github.com/eleboucher/other-name.git", { cwd: dir });
  const cfg = resolveConfig({}, undefined, dir);
  assert.equal(cfg.namespace, basename(dir));
  assert.equal(cfg.namespace_source, "local-worktree");
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
    3,
  );
  assert.equal(
    resolveConfig({}, { recall_limit: "garbage" }, "/r").recall_limit,
    3,
  );
});

// --- Facts (handshake request body) ---------------------------------------

test("buildFacts sends the worktree basename, git remote/toplevel, and env_namespace", async () => {
  const { execSync } = await import("node:child_process");
  const dir = mkdtempSync(join(tmpdir(), "memini-facts-"));
  execSync("git init -q", { cwd: dir });
  execSync("git remote add origin https://github.com/eleboucher/widget.git", { cwd: dir });
  const facts = buildFacts(dir, { MEMINI_NAMESPACE: "team/eu" });
  assert.equal(facts.cwd_basename, basename(dir));
  assert.equal(facts.remote_url, "https://github.com/eleboucher/widget.git");
  assert.equal(typeof facts.toplevel_path, "string");
  assert.ok(facts.toplevel_path.length > 0);
  assert.equal(facts.env_namespace, "team/eu");
});

test("buildFacts omits remote/toplevel outside a git repo, and env_namespace when unset", () => {
  const dir = mkdtempSync(join(tmpdir(), "memini-facts-nogit-"));
  const facts = buildFacts(dir, {});
  assert.equal(facts.cwd_basename, basename(dir));
  assert.equal(facts.remote_url, undefined);
  assert.equal(facts.toplevel_path, undefined);
  assert.equal(facts.env_namespace, undefined);
});

// --- effectiveConfig: handshake precedence and settings fallback chain ----

test("effectiveConfig: an explicit option/env namespace beats a successful handshake", () => {
  const optionCfg = resolveConfig({}, { namespace: "explicit" }, "/repo");
  const withHandshake = effectiveConfig(optionCfg, {
    namespace: "server/pinned",
    namespace_source: "pin",
    settings: {},
  });
  assert.equal(withHandshake.namespace, "explicit");
  assert.equal(withHandshake.namespace_source, "option");

  const envCfg = resolveConfig({ MEMINI_NAMESPACE: "team" }, undefined, "/repo");
  const envWithHandshake = effectiveConfig(envCfg, {
    namespace: "server/pinned",
    namespace_source: "pin",
    settings: {},
  });
  assert.equal(envWithHandshake.namespace, "team");
  assert.equal(envWithHandshake.namespace_source, "env");
});

test("effectiveConfig: a successful handshake beats the local worktree/default fallback", () => {
  const localCfg = resolveConfig({}, undefined, "/home/me/dev/my-project");
  assert.equal(localCfg.namespace_source, "local-worktree");
  const merged = effectiveConfig(localCfg, {
    namespace: "acme/widget",
    namespace_source: "remote",
    settings: {},
  });
  assert.equal(merged.namespace, "acme/widget");
  assert.equal(merged.namespace_source, "server:remote");
});

test("effectiveConfig: a null/failed handshake falls back to the local resolution", () => {
  const localCfg = resolveConfig({}, undefined, "/home/me/dev/my-project");
  const merged = effectiveConfig(localCfg, null);
  assert.equal(merged.namespace, "my-project");
  assert.equal(merged.namespace_source, "local-worktree");
});

test("effectiveConfig settings fallback chain: option > env > server > built-in default", () => {
  // Built-in default only: no option, no env, server has no opinion either.
  const bare = resolveConfig({}, undefined, "/r");
  const withNothing = effectiveConfig(bare, { namespace: "r", namespace_source: "cwd", settings: {} });
  assert.equal(withNothing.recall, true); // built-in default
  assert.equal(withNothing.recall_limit, 3); // built-in default

  // Server fills in beneath the built-in default when nothing local is explicit.
  const withServer = effectiveConfig(bare, {
    namespace: "r",
    namespace_source: "cwd",
    settings: { recall: false, capture: false, recall_limit: 7, inject_recall_max_tok: 500, inject_recall_min_score: 0.3 },
  });
  assert.equal(withServer.recall, false);
  assert.equal(withServer.capture, false);
  assert.equal(withServer.recall_limit, 7);
  assert.equal(withServer.recall_max_tokens, 500);
  assert.equal(withServer.recall_min_score, 0.3);

  // An explicit env value beats the server's settings.
  const envExplicit = resolveConfig({ MEMINI_RECALL: "false" }, undefined, "/r");
  const envVsServer = effectiveConfig(envExplicit, {
    namespace: "r",
    namespace_source: "cwd",
    settings: { recall: true },
  });
  assert.equal(envVsServer.recall, false);

  // An explicit option value beats the server's settings.
  const optExplicit = resolveConfig({}, { recall_limit: 9 }, "/r");
  const optVsServer = effectiveConfig(optExplicit, {
    namespace: "r",
    namespace_source: "cwd",
    settings: { recall_limit: 2 },
  });
  assert.equal(optVsServer.recall_limit, 9);
});

test("effectiveConfig tolerates a handshake response with no settings/namespace fields", () => {
  const cfg = resolveConfig({}, undefined, "/r");
  const merged = effectiveConfig(cfg, {});
  assert.equal(merged.namespace, cfg.namespace);
  assert.equal(merged.recall, cfg.recall);
  assert.equal(merged.recall_limit, cfg.recall_limit);
});

// --- memoizeAsync: TTL memo -------------------------------------------------

test("memoizeAsync caches the result until the TTL expires, using an injectable clock", async () => {
  let calls = 0;
  let time = 0;
  const memo = memoizeAsync(async () => {
    calls++;
    return calls;
  }, 100, () => time);

  assert.equal(await memo(), 1);
  assert.equal(await memo(), 1, "still within the TTL window");
  assert.equal(calls, 1);

  time = 50;
  assert.equal(await memo(), 1, "still cached");
  assert.equal(calls, 1);

  time = 150; // past the 100ms TTL
  assert.equal(await memo(), 2, "refreshed after expiry");
  assert.equal(calls, 2);
});

test("memoizeAsync calls the underlying fn again immediately after first use expires the cache at t=ttl", async () => {
  let calls = 0;
  let time = 0;
  const memo = memoizeAsync(async () => ++calls, 10, () => time);
  await memo();
  time = 10; // exactly at expiry: "now >= expiresAt" must refresh, not off-by-one
  assert.equal(await memo(), 2);
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

// --- MeminiPlugin: wiring, fail-soft, handshake namespace/settings --------

// A HandshakeResponse-shaped fetch mock, discriminated by URL so a single
// mock can stand in for both POST /v1/handshake and POST /v1/search|/v1/memories.
function mockFetchWithHandshake({ handshake, search, memories } = {}) {
  return async (url, init) => {
    const u = String(url);
    if (u.endsWith("/v1/handshake")) {
      return {
        ok: true,
        async json() {
          return handshake || { namespace: "server/ns", namespace_source: "remote", settings: {} };
        },
        async text() { return ""; },
      };
    }
    if (u.endsWith("/v1/search")) {
      return {
        ok: true,
        async json() { return search || { results: [] }; },
        async text() { return ""; },
      };
    }
    if (u.endsWith("/v1/memories")) {
      return {
        ok: true,
        async json() { return memories || {}; },
        async text() { return ""; },
      };
    }
    throw new Error(`unexpected fetch: ${u}`);
  };
}

test("chat.message uses the server-resolved namespace from a successful handshake", async () => {
  const requests = [];
  const realFetch = globalThis.fetch;
  const base = mockFetchWithHandshake({
    handshake: { namespace: "acme/widget", namespace_source: "remote", settings: {} },
  });
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    return base(url, init);
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    await hooks["chat.message"](
      { sessionID: "s1" },
      { parts: [{ type: "text", text: "hello", sessionID: "s1", messageID: "m1" }] },
    );
    const search = requests.find((r) => r.url.endsWith("/v1/search"));
    assert.ok(search, "should POST /v1/search");
    assert.equal(search.init.headers["X-Memini-Namespace"], "acme/widget");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("chat.message falls back to the local namespace when the handshake fails (fail-soft)", async () => {
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), init });
    if (String(url).endsWith("/v1/handshake")) throw new Error("connection refused");
    return { ok: true, async json() { return { results: [] }; }, async text() { return ""; } };
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    await hooks["chat.message"](
      { sessionID: "s1" },
      { parts: [{ type: "text", text: "hello", sessionID: "s1", messageID: "m1" }] },
    );
    const search = requests.find((r) => r.url.endsWith("/v1/search"));
    assert.ok(search, "recall must still work when the handshake fails");
    assert.equal(search.init.headers["X-Memini-Namespace"], "proj");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("the handshake is memoized across calls within the same plugin instance", async () => {
  let handshakeCalls = 0;
  const realFetch = globalThis.fetch;
  const base = mockFetchWithHandshake();
  globalThis.fetch = async (url, init) => {
    if (String(url).endsWith("/v1/handshake")) handshakeCalls++;
    return base(url, init);
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    await hooks["chat.message"](
      { sessionID: "s1" },
      { parts: [{ type: "text", text: "one", sessionID: "s1", messageID: "m1" }] },
    );
    await hooks["chat.message"](
      { sessionID: "s1" },
      { parts: [{ type: "text", text: "two", sessionID: "s1", messageID: "m2" }] },
    );
    assert.equal(handshakeCalls, 1, "the second call must reuse the memoized handshake");
  } finally {
    globalThis.fetch = realFetch;
  }
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

test("chat.message does not re-inject memories already shown in the same session", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) {
      return { ok: true, async json() { return {}; }, async text() { return ""; } };
    }
    return {
      ok: true,
      async json() {
        return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "prior note" } }] };
      },
      async text() { return ""; },
    };
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    // The injected synthetic part persists in the session, so an unchanged
    // match must not be re-injected on the next message.
    const first = { parts: [{ type: "text", text: "what did we decide?", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, first);
    assert.equal(first.parts.length, 2, "first message should get the recall part");
    assert.match(first.parts[0].text, /prior note/);
    const second = { parts: [{ type: "text", text: "and what else?", sessionID: "s1", messageID: "m2" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, second);
    assert.equal(second.parts.length, 1, "already-shown memory must not re-inject");
    // A different session has not been shown it yet.
    const other = { parts: [{ type: "text", text: "what did we decide?", sessionID: "s2", messageID: "m3" }] };
    await hooks["chat.message"]({ sessionID: "s2" }, other);
    assert.equal(other.parts.length, 2, "other sessions still get the memory");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("an HTTP error is logged even when fallback_on_error degrades it", async () => {
  const realFetch = globalThis.fetch;
  const realError = console.error;
  const logged = [];
  console.error = (m) => logged.push(String(m));
  globalThis.fetch = async () => ({
    ok: false,
    status: 500,
    async json() { return {}; },
    async text() { return "boom"; },
  });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    // A swallowed 500 looks like "memory isn't working"; the degrade path
    // must still say why.
    const output = { parts: [{ type: "text", text: "q", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    assert.equal(output.parts.length, 1, "recall failure degrades to no injection");
    assert.ok(
      logged.some((m) => m.includes("failed: 500")),
      `expected a failed-status warn, got: ${JSON.stringify(logged)}`,
    );
  } finally {
    globalThis.fetch = realFetch;
    console.error = realError;
  }
});

test("chat.message caps the recall block by MEMINI_INJECT_RECALL_MAX_TOK", async () => {
  // Four short bullets (~12 words each ≈ 16 tokens) + max=20: only the head
  // bullet fits, the tail is dropped with the truncation footer. Budget is
  // passed as a plugin option to avoid process.env mutation.
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) {
      return { ok: true, async json() { return {}; }, async text() { return ""; } };
    }
    return {
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
    };
  };
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
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) {
      return { ok: true, async json() { return {}; }, async text() { return ""; } };
    }
    return {
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
    };
  };
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

test("requests carry X-Memini-Home when configured, omit it otherwise", async () => {
  const requests = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url: String(url), headers: init.headers });
    return { ok: true, async json() { return { results: [] }; }, async text() { return ""; } };
  };
  try {
    const withHome = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080", home: "personal/acme" },
    );
    await withHome["chat.message"](
      { sessionID: "s1" },
      { parts: [{ type: "text", text: "hello", sessionID: "s1", messageID: "m1" }] },
    );

    const withoutHome = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    await withoutHome["chat.message"](
      { sessionID: "s2" },
      { parts: [{ type: "text", text: "hello again", sessionID: "s2", messageID: "m2" }] },
    );

    const search = requests.filter((r) => r.url.endsWith("/v1/search"));
    assert.equal(search.length, 2);
    assert.equal(search[0].headers["X-Memini-Home"], "personal/acme");
    assert.equal(search[1].headers["X-Memini-Home"], undefined);
  } finally {
    globalThis.fetch = realFetch;
  }
});

// --- Status ---------------------------------------------------------------

test("redactSecret fingerprints a token and elides a short one", () => {
  assert.equal(redactSecret("sk-0123456789abcd4f2a"), "sk-…4f2a");
  assert.equal(redactSecret("short"), "***");
  assert.equal(redactSecret(""), "");
});

test("describeSettings reports the provenance that exposes a global env pin", () => {
  const report = describeSettings(
    {
      MEMINI_NAMESPACE: "pinned",
      MEMINI_API_KEY: "sk-0123456789abcd4f2a",
      MEMINI_BASE_URL: "http://memini.example.com",
    },
    undefined,
    "/tmp/proj-x",
  );
  assert.equal(report.namespace.effective, "pinned");
  assert.equal(report.namespace.source, "env");
  // The line that turns "your namespace is pinned" into a diagnosis.
  assert.equal(report.namespace.derived.namespace, "proj-x");

  const codes = report.warnings.map((w) => w.code);
  assert.ok(codes.includes("global-namespace-pin"), `got: ${codes}`);
  assert.ok(codes.includes("plaintext-bearer"), `got: ${codes}`);

  const text = renderStatus(report);
  assert.ok(!text.includes("0123456789"), "the token must never be printed in full");
  assert.match(text, /sk-…4f2a/);
  assert.match(text, /git\/cwd would give\s+proj-x/);
});

test("the memini_status tool is registered zero-arg and never throws", async () => {
  const realFetch = globalThis.fetch;
  // Hermetic: the handshake attempt must fail-soft, not actually hit the
  // network from a unit test.
  globalThis.fetch = async () => {
    throw new Error("no server in this test");
  };
  const hooks = await MeminiPlugin(
    { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
    { base_url: "http://localhost:8080" },
  );
  const status = hooks.tool.memini_status;
  // Zero-arg on purpose: a declared parameter would need a zod schema, and this
  // plugin ships dependency-free (see the comment on the tool).
  assert.deepEqual(status.args, {});
  // The tool reads process.env at call time (so an override set mid-session is
  // visible); a developer's exported MEMINI_NAMESPACE must not decide the
  // assertion — which is, fittingly, the very pin the report exists to expose.
  const prev = process.env.MEMINI_NAMESPACE;
  delete process.env.MEMINI_NAMESPACE;
  try {
    const out = await status.execute({}, {});
    assert.match(out.output, /memini — effective settings/);
    assert.match(out.output, /NAMESPACE/);
    assert.equal(out.metadata.namespace, "proj");
    assert.equal(out.metadata.source, "local-worktree");
  } finally {
    if (prev !== undefined) process.env.MEMINI_NAMESPACE = prev;
    globalThis.fetch = realFetch;
  }
});

test("event never rejects, even when client.session.messages throws", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => {
    throw new Error("no server in this test");
  };
  const client = {
    session: {
      messages: async () => {
        throw new Error("opencode server error");
      },
    },
  };
  try {
    const hooks = await MeminiPlugin(
      { client, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    await assert.doesNotReject(
      hooks.event({ event: { type: "session.idle", properties: { sessionID: "s1" } } }),
    );
  } finally {
    globalThis.fetch = realFetch;
  }
});
