// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, realpathSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, basename } from "node:path";
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
  readOverride,
  overrideKey,
  overridesPath,
  describeSettings,
  renderStatus,
  redactSecret,
} from "./memini.js";

// Point XDG_CONFIG_HOME at an empty temp dir so a developer's real
// ~/.config/memini/config.json can't leak tenant prefixes into these tests
// (resolveConfig reads it at call time). Tenant tests write their own.
process.env.XDG_CONFIG_HOME = mkdtempSync(join(tmpdir(), "memini-test-config-"));

// A temp XDG_CONFIG_HOME with the given memini/config.json contents.
function freshConfig(config) {
  const dir = mkdtempSync(join(tmpdir(), "memini-test-config-"));
  mkdirSync(join(dir, "memini"), { recursive: true });
  writeFileSync(join(dir, "memini", "config.json"), JSON.stringify(config));
  return dir;
}

// A temp XDG_CONFIG_HOME holding memini/overrides.json. `raw` writes the file
// verbatim, so a malformed one can be exercised.
function freshOverrides(overrides, raw) {
  const dir = mkdtempSync(join(tmpdir(), "memini-test-config-"));
  mkdirSync(join(dir, "memini"), { recursive: true });
  writeFileSync(
    join(dir, "memini", "overrides.json"),
    raw !== undefined ? raw : JSON.stringify({ version: 1, overrides }),
  );
  return dir;
}

// Swap XDG_CONFIG_HOME for the duration of `fn` (resolveConfig reads it at call
// time, like the config-file tests above).
function withXdg(dir, fn) {
  const prev = process.env.XDG_CONFIG_HOME;
  process.env.XDG_CONFIG_HOME = dir;
  try {
    return fn();
  } finally {
    process.env.XDG_CONFIG_HOME = prev;
  }
}

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

test("home resolves from MEMINI_HOME env; option wins over env; unset -> undefined", () => {
  assert.equal(resolveConfig({}, undefined, "/r").home, undefined);
  assert.equal(resolveConfig({ MEMINI_HOME: "personal/acme" }, undefined, "/r").home, "personal/acme");
  assert.equal(
    resolveConfig({ MEMINI_HOME: "personal/acme" }, { home: "personal/other" }, "/r").home,
    "personal/other",
  );
});

test("tenant config prefixes the namespace and derives {project} from git", async () => {
  const { execSync } = await import("node:child_process");
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "memini-fork");
  mkdirSync(dir);
  execSync("git init -q", { cwd: dir });
  execSync("git remote add origin https://github.com/eleboucher/memini.git", { cwd: dir });
  const prev = process.env.XDG_CONFIG_HOME;
  process.env.XDG_CONFIG_HOME = freshConfig({ tenantRoots: [{ path: parent, tenant: "work" }] });
  try {
    // The git remote name wins over the cwd basename, so the same repo lands
    // in the same namespace as the other integrations (work/memini, not
    // work/memini-fork), with the "/" separator preserved.
    assert.equal(resolveConfig({}, undefined, dir).namespace, "work/memini");
  } finally {
    process.env.XDG_CONFIG_HOME = prev;
  }
});

test("config present but no tenant match still uses the git project name", async () => {
  const { execSync } = await import("node:child_process");
  const parent = mkdtempSync(join(tmpdir(), "memini-notenant-"));
  const dir = join(parent, "checkout-dir");
  mkdirSync(dir);
  execSync("git init -q", { cwd: dir });
  execSync("git remote add origin https://github.com/eleboucher/widget.git", { cwd: dir });
  const prev = process.env.XDG_CONFIG_HOME;
  // A tenant root that does NOT contain `dir`, so no tenant matches.
  process.env.XDG_CONFIG_HOME = freshConfig({ tenantRoots: [{ path: "/nowhere", tenant: "work" }] });
  try {
    // Config present -> {project} is the git remote name (widget), not the cwd
    // basename (checkout-dir); tenant drops out of the default template.
    assert.equal(resolveConfig({}, undefined, dir).namespace, "widget");
  } finally {
    process.env.XDG_CONFIG_HOME = prev;
  }
});

test("MEMINI_NAMESPACE is used raw-trimmed, not flattened", () => {
  // The server validates the header; a hierarchical value keeps its "/" so it
  // matches the other integrations instead of collapsing to team-eu.
  assert.equal(resolveConfig({ MEMINI_NAMESPACE: "  team/eu  " }, undefined, "/repo").namespace, "team/eu");
});

test("without a config file the namespace stays the legacy cwd basename, even in a git repo", async () => {
  const { execSync } = await import("node:child_process");
  const dir = mkdtempSync(join(tmpdir(), "memini-legacy-"));
  execSync("git init -q", { cwd: dir });
  execSync("git remote add origin https://github.com/eleboucher/other-name.git", { cwd: dir });
  assert.equal(resolveConfig({}, undefined, dir).namespace, basename(dir));
});

test("tenant roots with an empty/missing path or a non-object entry are skipped", () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "proj");
  mkdirSync(dir);
  const outside = mkdtempSync(join(tmpdir(), "memini-outside-"));
  const prev = process.env.XDG_CONFIG_HOME;
  process.env.XDG_CONFIG_HOME = freshConfig({
    tenantRoots: [{ path: "", tenant: "evil" }, "junk", { tenant: "nopath" }, { path: parent, tenant: "work" }],
  });
  try {
    // The empty-path entry must not startsWith-match every cwd...
    assert.equal(resolveConfig({}, undefined, outside).namespace, basename(outside));
    // ...and bad entries must not abort the scan before the valid root.
    assert.equal(resolveConfig({}, undefined, dir).namespace, "work/proj");
  } finally {
    process.env.XDG_CONFIG_HOME = prev;
  }
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

test("chat.message does not re-inject memories already shown in the same session", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true,
    async json() {
      return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "prior note" } }] };
    },
    async text() { return ""; },
  });
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

    // Filter out the warmup /healthz pings plugin init fires.
    const searches = requests.filter((r) => r.url.endsWith("/v1/search"));
    assert.equal(searches.length, 2);
    assert.equal(searches[0].headers["X-Memini-Home"], "personal/acme");
    assert.equal(searches[1].headers["X-Memini-Home"], undefined);
  } finally {
    globalThis.fetch = realFetch;
  }
});

// --- Namespace override --------------------------------------------------

test("the project override beats MEMINI_NAMESPACE and the inline option", () => {
  const dir = mkdtempSync(join(tmpdir(), "memini-override-"));
  const xdg = freshOverrides({ [realpathSync(dir)]: { namespace: "acme/api", setAt: "2026-07-12T20:30:00Z" } });
  withXdg(xdg, () => {
    // The env pin is exactly the case the override exists to beat: a globally
    // exported MEMINI_NAMESPACE would otherwise pin every repo on the machine.
    const cfg = resolveConfig({ MEMINI_NAMESPACE: "pinned" }, { namespace: "from-option" }, dir);
    assert.equal(cfg.namespace, "acme/api");
    assert.equal(cfg.namespace_source, "override");
    assert.equal(cfg.override.setAt, "2026-07-12T20:30:00Z");
  });
});

test("the override is keyed on the git toplevel, so it applies from a subdirectory", async () => {
  const { execSync } = await import("node:child_process");
  const repo = realpathSync(mkdtempSync(join(tmpdir(), "memini-override-repo-")));
  execSync("git init -q", { cwd: repo });
  const nested = join(repo, "services", "api");
  mkdirSync(nested, { recursive: true });
  const xdg = freshOverrides({ [repo]: { namespace: "acme/api", setAt: "2026-07-12T20:30:00Z" } });
  withXdg(xdg, () => {
    assert.equal(overrideKey(nested), repo);
    assert.equal(resolveConfig({}, undefined, nested).namespace, "acme/api");
  });
});

test("a malformed or absent overrides file degrades to automatic resolution", () => {
  const dir = mkdtempSync(join(tmpdir(), "memini-override-"));
  // Absent: the module-level XDG temp holds no overrides.json.
  assert.equal(readOverride(dir), null);
  assert.equal(resolveConfig({}, undefined, dir).namespace, basename(dir));
  // Hand-edited into invalid JSON: never throw into opencode, just resolve.
  withXdg(freshOverrides(null, "{ not json"), () => {
    assert.equal(readOverride(dir), null);
    assert.equal(resolveConfig({}, undefined, dir).namespace, basename(dir));
  });
  // Right JSON, wrong shape.
  withXdg(freshOverrides(null, JSON.stringify({ version: 1, overrides: [] })), () => {
    assert.equal(readOverride(dir), null);
  });
  // Present, but with an empty namespace: not an override.
  withXdg(freshOverrides({ [realpathSync(dir)]: { namespace: "   " } }), () => {
    assert.equal(readOverride(dir), null);
    assert.equal(resolveConfig({}, undefined, dir).namespace, basename(dir));
  });
});

test("overridesPath honours XDG_CONFIG_HOME and falls back to ~/.config", () => {
  assert.equal(overridesPath({ XDG_CONFIG_HOME: "/x" }), join("/x", "memini", "overrides.json"));
  assert.ok(overridesPath({}).endsWith(join(".config", "memini", "overrides.json")));
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

test("describeSettings shows what an active override is masking", () => {
  const dir = mkdtempSync(join(tmpdir(), "memini-override-"));
  const xdg = freshOverrides({ [realpathSync(dir)]: { namespace: "acme/api", setAt: "2026-07-12T20:30:00Z" } });
  withXdg(xdg, () => {
    const report = describeSettings({ MEMINI_NAMESPACE: "pinned" }, undefined, dir);
    assert.equal(report.namespace.effective, "acme/api");
    assert.equal(report.namespace.source, "override");
    assert.equal(report.namespace.withoutOverride.namespace, "pinned");
    const codes = report.warnings.map((w) => w.code);
    assert.ok(codes.includes("override-active"));
    // An override IS the fix for a global pin, so it must not also nag about one.
    assert.ok(!codes.includes("global-namespace-pin"));
    assert.match(renderStatus(report), /without the override\s+pinned/);
  });
});

test("the memini_status tool is registered zero-arg and never throws", async () => {
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
    assert.equal(out.metadata.source, "worktree");
  } finally {
    if (prev !== undefined) process.env.MEMINI_NAMESPACE = prev;
  }
});

// --- Recall budget race + carryover ----------------------------------------

const okJson = (payload) => ({ ok: true, async json() { return payload; }, async text() { return ""; } });

test("resolveConfig parses recall_budget_ms: default, option > env, malformed falls back, 0 accepted", () => {
  assert.equal(resolveConfig({}, undefined, "/r").recall_budget_ms, 2000);
  assert.equal(resolveConfig({ MEMINI_RECALL_BUDGET_MS: "500" }, undefined, "/r").recall_budget_ms, 500);
  assert.equal(
    resolveConfig({ MEMINI_RECALL_BUDGET_MS: "500" }, { recall_budget_ms: 100 }, "/r").recall_budget_ms,
    100,
  );
  assert.equal(resolveConfig({ MEMINI_RECALL_BUDGET_MS: "junk" }, undefined, "/r").recall_budget_ms, 2000);
  // Number("") === 0, which would silently disable the race; an empty env var
  // must fall through to the default instead.
  assert.equal(resolveConfig({ MEMINI_RECALL_BUDGET_MS: "" }, undefined, "/r").recall_budget_ms, 2000);
  assert.equal(resolveConfig({}, { recall_budget_ms: 0 }, "/r").recall_budget_ms, 0);
});

test("a recall slower than the budget skips this turn and carries over to the next", async () => {
  const realFetch = globalThis.fetch;
  let release;
  const gate = new Promise((r) => { release = r; });
  let searchCalls = 0;
  globalThis.fetch = async (url) => {
    if (!String(url).endsWith("/v1/search")) return okJson({});
    searchCalls++;
    if (searchCalls === 1) {
      await gate;
      return okJson({ results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "late hit" } }] });
    }
    return okJson({ results: [] });
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080", recall_budget_ms: 10 },
    );
    const first = { parts: [{ type: "text", text: "q1", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, first);
    assert.equal(first.parts.length, 1, "a budget miss must not inject this turn");
    release();
    await new Promise((r) => setTimeout(r, 20)); // let the late fetch settle into the stash
    // The second turn's own search returns nothing, so an injection can only
    // come from the carryover.
    const second = { parts: [{ type: "text", text: "q2", sessionID: "s1", messageID: "m2" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, second);
    assert.equal(second.parts.length, 2, "late results should inject on the next turn");
    assert.match(second.parts[0].text, /late hit/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("recall_budget_ms: 0 restores blocking same-turn injection", async () => {
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    if (!String(url).endsWith("/v1/search")) return okJson({});
    await new Promise((r) => setTimeout(r, 30));
    return okJson({ results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "slow hit" } }] });
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080", recall_budget_ms: 0 },
    );
    const output = { parts: [{ type: "text", text: "q", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    assert.equal(output.parts.length, 2, "budget 0 must wait for the slow recall");
    assert.match(output.parts[0].text, /slow hit/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("carried-over hits still respect the seen-dedup and the score floor", async () => {
  const realFetch = globalThis.fetch;
  let release;
  const gate = new Promise((r) => { release = r; });
  let searchCalls = 0;
  const seenHit = { score: 0.9, memory: { id: "m1", tier: "semantic", summary: "already shown" } };
  globalThis.fetch = async (url) => {
    if (!String(url).endsWith("/v1/search")) return okJson({});
    searchCalls++;
    if (searchCalls === 1) return okJson({ results: [seenHit] });
    if (searchCalls === 2) {
      await gate;
      return okJson({
        results: [
          seenHit,
          { score: 0.1, memory: { id: "m2", tier: "episodic", summary: "below the floor" } },
          { score: 0.8, memory: { id: "m3", tier: "semantic", summary: "fresh carryover" } },
        ],
      });
    }
    return okJson({ results: [] });
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080", recall_budget_ms: 10, recall_min_score: 0.4 },
    );
    const t1 = { parts: [{ type: "text", text: "q1", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, t1);
    assert.equal(t1.parts.length, 2, "turn 1 injects the fast hit");
    const t2 = { parts: [{ type: "text", text: "q2", sessionID: "s1", messageID: "m2" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, t2);
    assert.equal(t2.parts.length, 1, "turn 2 misses the budget");
    release();
    await new Promise((r) => setTimeout(r, 20));
    const t3 = { parts: [{ type: "text", text: "q3", sessionID: "s1", messageID: "m3" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, t3);
    assert.equal(t3.parts.length, 2, "turn 3 injects the carried-over hit");
    assert.match(t3.parts[0].text, /fresh carryover/);
    assert.ok(!t3.parts[0].text.includes("already shown"), "seen memory must not re-inject");
    assert.ok(!t3.parts[0].text.includes("below the floor"), "floored memory must not inject");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("a late recall rejection is logged, never an unhandled rejection", async () => {
  const realFetch = globalThis.fetch;
  const realError = console.error;
  const logged = [];
  console.error = (m) => logged.push(String(m));
  const unhandled = [];
  const onUnhandled = (e) => unhandled.push(e);
  process.on("unhandledRejection", onUnhandled);
  let release;
  const gate = new Promise((r) => { release = r; });
  globalThis.fetch = async (url) => {
    if (!String(url).endsWith("/v1/search")) return okJson({});
    await gate;
    throw new Error("late boom");
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      // fallback_on_error:false makes postJson rethrow after the budget has
      // expired, when nothing awaits the promise anymore.
      { base_url: "http://localhost:8080", recall_budget_ms: 10, fallback_on_error: false },
    );
    const output = { parts: [{ type: "text", text: "q", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    assert.equal(output.parts.length, 1);
    release();
    await new Promise((r) => setTimeout(r, 20));
    assert.ok(
      logged.some((m) => m.includes("late boom")),
      `expected the late error to be logged, got: ${JSON.stringify(logged)}`,
    );
    assert.equal(unhandled.length, 0, "a late rejection must be caught");
  } finally {
    globalThis.fetch = realFetch;
    console.error = realError;
    process.removeListener("unhandledRejection", onUnhandled);
  }
});

test("plugin init fires a warmup /healthz ping and survives its failure", async () => {
  const realFetch = globalThis.fetch;
  const urls = [];
  globalThis.fetch = async (url) => {
    urls.push(String(url));
    throw new Error("down");
  };
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    assert.ok(urls.some((u) => u.endsWith("/healthz")), `expected a warmup ping, got: ${urls}`);
    assert.ok(hooks["chat.message"], "init must survive a failed warmup");
    await new Promise((r) => setTimeout(r, 5)); // let the rejected warmup settle
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
