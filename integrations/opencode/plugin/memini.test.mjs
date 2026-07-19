// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { join, basename, dirname } from "node:path";
import {
  MeminiPlugin,
  resolveConfig,
  effectiveConfig,
  truncateForCapture,
  buildTurnCapture,
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
  parseVersion,
  compareVersions,
  injectedSuppressed,
  injectedIdentity,
  resolveInstallContext,
  resolveInstallContextFrom,
  prepareCacheUpdate,
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

// --- Auto-update ------------------------------------------------------------

test("parseVersion extracts major.minor.patch", () => {
  assert.deepEqual(parseVersion("1.2.3"), { major: 1, minor: 2, patch: 3 });
  assert.deepEqual(parseVersion("0.7.2"), { major: 0, minor: 7, patch: 2 });
  assert.deepEqual(parseVersion("^0.5.2"), { major: 0, minor: 5, patch: 2 });
  assert.equal(parseVersion("not-a-version"), null);
  assert.equal(parseVersion(""), null);
});

test("compareVersions returns -1, 0, or 1", () => {
  assert.equal(compareVersions("0.5.2", "0.7.2"), -1);
  assert.equal(compareVersions("0.7.2", "0.7.2"), 0);
  assert.equal(compareVersions("0.7.2", "0.5.2"), 1);
  assert.equal(compareVersions("1.0.0", "0.9.9"), 1);
  assert.equal(compareVersions("0.9.9", "1.0.0"), -1);
  // Same major, different minor
  assert.equal(compareVersions("0.5.0", "0.7.0"), -1);
});

test("resolveConfig includes auto_update defaulting to true", () => {
  assert.equal(resolveConfig({}, undefined, "/repo").auto_update, true);
  assert.equal(resolveConfig({ MEMINI_AUTO_UPDATE: "0" }, undefined, "/repo").auto_update, false);
  assert.equal(resolveConfig({ MEMINI_AUTO_UPDATE: "false" }, undefined, "/repo").auto_update, false);
  assert.equal(resolveConfig({}, { auto_update: false }, "/repo").auto_update, false);
  assert.equal(resolveConfig({ MEMINI_AUTO_UPDATE: "1" }, { auto_update: false }, "/repo").auto_update, false);
  assert.equal(resolveConfig({ MEMINI_AUTO_UPDATE: "0" }, { auto_update: true }, "/repo").auto_update, true);
});

// --- auto-update: resolveInstallContext / prepareCacheUpdate ----------------
//
// opencode installs each npm plugin spec into its own isolated wrapper dir
// under ~/.cache/opencode/packages/<spec>/ — e.g. opencode-memini@latest/ —
// holding a package.json listing the plugin as a dependency plus a
// node_modules/ tree. The plugin file lives at
// <wrapper>/node_modules/@eleboucher/opencode-memini/memini.js, so the wrapper
// is the first ancestor whose child is a `node_modules` directory. These tests
// build that layout in a temp dir and confirm the walker finds it.

function buildWrapperLayout(root, { withPackageJson = true, withLockfiles = [] } = {}) {
  // <root>/opencode-memini@latest/node_modules/@eleboucher/opencode-memini/memini.js
  const wrapper = join(root, "opencode-memini@latest");
  const pkgDir = join(wrapper, "node_modules", "@eleboucher", "opencode-memini");
  mkdirSync(pkgDir, { recursive: true });
  writeFileSync(join(pkgDir, "memini.js"), "// plugin entry\n");
  if (withPackageJson) {
    writeFileSync(
      join(wrapper, "package.json"),
      JSON.stringify({ dependencies: { "@eleboucher/opencode-memini": "0.7.5" } }, null, 2),
    );
  }
  for (const name of withLockfiles) {
    writeFileSync(join(wrapper, name), "{}\n");
  }
  return { wrapper, pkgDir, pluginFile: join(pkgDir, "memini.js") };
}

test("resolveInstallContextFrom finds the wrapper dir by walking up to node_modules", () => {
  const root = mkdtempSync(join(tmpdir(), "memini-ctx-"));
  try {
    const { wrapper, pluginFile } = buildWrapperLayout(root);
    const ctx = resolveInstallContextFrom(pluginFile);
    assert.ok(ctx, "should resolve the wrapper context");
    assert.equal(ctx.installDir, wrapper);
    assert.equal(ctx.packageJsonPath, join(wrapper, "package.json"));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("resolveInstallContextFrom returns null when node_modules exists but package.json is absent", () => {
  const root = mkdtempSync(join(tmpdir(), "memini-ctx-"));
  try {
    const { pluginFile } = buildWrapperLayout(root, { withPackageJson: false });
    const ctx = resolveInstallContextFrom(pluginFile);
    assert.equal(ctx, null);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("resolveInstallContextFrom returns null when no node_modules ancestor exists", () => {
  const root = mkdtempSync(join(tmpdir(), "memini-ctx-"));
  try {
    // A loose file with no node_modules anywhere above it.
    const loose = join(root, "lonely", "memini.js");
    mkdirSync(dirname(loose), { recursive: true });
    writeFileSync(loose, "// nope\n");
    const ctx = resolveInstallContextFrom(loose);
    assert.equal(ctx, null);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("prepareCacheUpdate rewrites the version pin, drops node_modules, and removes both lockfile formats", () => {
  const root = mkdtempSync(join(tmpdir(), "memini-ctx-"));
  try {
    const { wrapper, pkgDir } = buildWrapperLayout(root, {
      withLockfiles: ["package-lock.json", "bun.lock"],
    });
    const log = { warn: () => {} };
    const ctx = { installDir: wrapper, packageJsonPath: join(wrapper, "package.json") };
    const installDir = prepareCacheUpdate("0.7.6", log, ctx);
    assert.equal(installDir, wrapper);

    // package.json now pins 0.7.6
    const pkg = JSON.parse(readFileSync(join(wrapper, "package.json"), "utf8"));
    assert.equal(pkg.dependencies["@eleboucher/opencode-memini"], "0.7.6");

    // node_modules plugin dir is gone
    assert.equal(existsSync(pkgDir), false, "cached node_modules package should be removed");

    // both lockfiles removed
    assert.equal(existsSync(join(wrapper, "package-lock.json")), false);
    assert.equal(existsSync(join(wrapper, "bun.lock")), false);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("prepareCacheUpdate no-ops (returns the dir) when the pin already matches", () => {
  const root = mkdtempSync(join(tmpdir(), "memini-ctx-"));
  try {
    const { wrapper, pkgDir } = buildWrapperLayout(root, { withLockfiles: ["package-lock.json"] });
    const log = { warn: () => {} };
    const ctx = { installDir: wrapper, packageJsonPath: join(wrapper, "package.json") };
    // Pin already 0.7.5 — matching the layout fixture above.
    const installDir = prepareCacheUpdate("0.7.5", log, ctx);
    assert.equal(installDir, wrapper);
    // Short-circuited: node_modules plugin dir and lockfile are NOT touched.
    assert.equal(existsSync(pkgDir), true, "cached node_modules should be left in place on a matching pin");
    assert.equal(existsSync(join(wrapper, "package-lock.json")), true, "lockfile should be left in place on a matching pin");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("prepareCacheUpdate returns null when given no ctx and the live install context can't be resolved", () => {
  // From the test process, import.meta.url of memini.js lives in the dev
  // checkout, where resolveInstallContext() walks up to the repo root. The
  // repo root here has a package.json + node_modules, so it would actually
  // resolve — making this a weak test. Instead, assert the explicit-null-ctx
  // contract by passing a ctx whose packageJsonPath doesn't exist: the rewrite
  // step throws and the function returns null.
  const root = mkdtempSync(join(tmpdir(), "memini-ctx-"));
  try {
    const bogus = { installDir: root, packageJsonPath: join(root, "does-not-exist.json") };
    const warnings = [];
    const log = { warn: (m) => warnings.push(m) };
    const result = prepareCacheUpdate("0.7.6", log, bogus);
    assert.equal(result, null);
    assert.ok(warnings.some((w) => /failed to rewrite cache package\.json/.test(w)));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test("resolveInstallContext: smoke check it returns null or a real ctx (doesn't throw from the dev checkout)", () => {
  // In the dev checkout, resolveInstallContext() walks up from this test file's
  // sibling memini.js to the repo root, which has node_modules + package.json.
  // That is NOT a real opencode wrapper dir, but the walker's contract is only
  // "find an ancestor with node_modules + package.json", so it resolves there.
  // This test asserts it doesn't throw; we don't assert the exact path because
  // where it lands depends on the caller's cwd layout, not the contract.
  const ctx = resolveInstallContext();
  assert.ok(ctx === null || (typeof ctx.installDir === "string" && typeof ctx.packageJsonPath === "string"));
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

test("effectiveConfig capture caps: env > server > built-in default", () => {
  // An explicit env value beats the server's settings.
  const envExplicit = resolveConfig(
    { MEMINI_CAPTURE_USER_MAX_CHARS: "50000", MEMINI_CAPTURE_ASSISTANT_MAX_CHARS: "60000" },
    undefined,
    "/r",
  );
  const envVsServer = effectiveConfig(envExplicit, {
    namespace: "r",
    namespace_source: "cwd",
    settings: { capture_user_max_chars: 1000, capture_assistant_max_chars: 3000 },
  });
  assert.equal(envVsServer.capture_user_max_chars, 50000);
  assert.equal(envVsServer.capture_assistant_max_chars, 60000);

  // Without the env var, the server's value fills in over the built-in default.
  const bare = resolveConfig({}, undefined, "/r");
  const serverOnly = effectiveConfig(bare, {
    namespace: "r",
    namespace_source: "cwd",
    settings: { capture_user_max_chars: 1000, capture_assistant_max_chars: 4000 },
  });
  assert.equal(serverOnly.capture_user_max_chars, 1000);
  assert.equal(serverOnly.capture_assistant_max_chars, 4000);

  // With neither env nor server, the built-in defaults apply.
  const withNothing = effectiveConfig(bare, { namespace: "r", namespace_source: "cwd", settings: {} });
  assert.equal(withNothing.capture_user_max_chars, 1000);
  assert.equal(withNothing.capture_assistant_max_chars, 3000);
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

test("memoizeAsync dedupes concurrent in-flight callers", async () => {
  let calls = 0;
  let time = 0;
  let resolveFn;
  const memo = memoizeAsync(
    () => {
      calls++;
      return new Promise((resolve) => {
        resolveFn = resolve;
      });
    },
    100,
    () => time,
  );

  const pending = [memo(), memo(), memo(), memo(), memo()];
  assert.equal(calls, 1, "only one in-flight call");

  resolveFn(42);
  assert.deepEqual(await Promise.all(pending), [42, 42, 42, 42, 42]);

  time = 150;
  const nextCall = memo();
  assert.equal(calls, 2, "refreshed after TTL");
  resolveFn(99);
  assert.equal(await nextCall, 99);
});

test("memoizeAsync clears the cache on rejection", async () => {
  let calls = 0;
  let time = 0;
  let rejectFn;
  const memo = memoizeAsync(
    () => {
      calls++;
      return new Promise((_, reject) => {
        rejectFn = reject;
      });
    },
    100,
    () => time,
  );

  const first = memo();
  assert.equal(calls, 1);
  rejectFn(new Error("boom"));
  await assert.rejects(first, /boom/);

  const second = memo();
  assert.equal(calls, 2, "rejected promise did not poison the cache");
  rejectFn(new Error("still down"));
  await assert.rejects(second, /still down/);
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

// The per-session dedupe window is capped: oldest ids age out (and may
// re-inject); recent ids stay suppressed.
test("per-session injected-id window is bounded (oldest ids age out)", async () => {
  const realFetch = globalThis.fetch;
  let nextResults = [];
  globalThis.fetch = async () => ({
    ok: true,
    async json() { return { results: nextResults }; },
    async text() { return ""; },
  });
  const hit = (id) => ({ score: 0.9, memory: { id, tier: "semantic", summary: `note ${id}` } });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    // Push 206 distinct ids through the window (cap is 200): m0..m205.
    for (let i = 0; i < 206; i++) {
      nextResults = [hit(`m${i}`)];
      const output = { parts: [{ type: "text", text: `q${i}`, sessionID: "s1", messageID: `msg${i}` }] };
      await hooks["chat.message"]({ sessionID: "s1" }, output);
      assert.equal(output.parts.length, 2, `call ${i} should inject its fresh memory`);
    }
    // m0 was evicted from the 200-id window -> allowed to re-inject.
    nextResults = [hit("m0")];
    const old = { parts: [{ type: "text", text: "old", sessionID: "s1", messageID: "mo" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, old);
    assert.equal(old.parts.length, 2, "an id evicted from the window must be allowed to re-inject");
    // m205 is still inside the window -> suppressed.
    nextResults = [hit("m205")];
    const recent = { parts: [{ type: "text", text: "recent", sessionID: "s1", messageID: "mr" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, recent);
    assert.equal(recent.parts.length, 1, "a recent id must stay suppressed");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Already-shown ids ride to the server as exclude_ids; an older server that
// 400s on the unknown field gets one retry without it, then it stops.
test("recall sends exclude_ids and falls back when the server rejects them", async () => {
  const realFetch = globalThis.fetch;
  const requests = [];
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
        return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "prior note" } }] };
      },
      async text() { return ""; },
    };
  };
  const searches = () => requests.filter((r) => r.url.endsWith("/v1/search"));
  const msg = (id) => ({ parts: [{ type: "text", text: "query", sessionID: "s1", messageID: id }] });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    // First recall: nothing shown yet, so no exclude_ids on the wire.
    await hooks["chat.message"]({ sessionID: "s1" }, msg("m1"));
    assert.equal(searches()[0].body.exclude_ids, undefined);
    // Second recall: m1 was shown, so it must ride along as exclude_ids.
    await hooks["chat.message"]({ sessionID: "s1" }, msg("m2"));
    assert.deepEqual(searches()[1].body.exclude_ids, ["m1"]);

    // Old server: 400 on exclude_ids -> one retry without it, then never again.
    rejectExcludeIds = true;
    await hooks["chat.message"]({ sessionID: "s1" }, msg("m3"));
    const [, , withField, retry] = searches();
    assert.deepEqual(withField.body.exclude_ids, ["m1"], "first attempt still carries exclude_ids");
    assert.equal(retry.body.exclude_ids, undefined, "the retry must drop exclude_ids");
    await hooks["chat.message"]({ sessionID: "s1" }, msg("m4"));
    assert.equal(searches().length, 5, "after the fallback each recall is a single request");
    assert.equal(searches()[4].body.exclude_ids, undefined, "exclude_ids is never sent again");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Same bound for the captured assistant-id dedupe.
test("captured assistant-id window is bounded (an aged-out turn can re-capture)", async () => {
  const realFetch = globalThis.fetch;
  const posts = [];
  globalThis.fetch = async (url, init) => {
    if (String(url).endsWith("/v1/memories")) posts.push(JSON.parse(init.body));
    return { ok: true, async json() { return {}; }, async text() { return ""; } };
  };
  let turn = [];
  const client = { session: { messages: async () => ({ data: turn }) } };
  const mkTurn = (id) => [
    { info: { role: "user" }, parts: [{ type: "text", text: `u-${id}` }] },
    { info: { role: "assistant", id }, parts: [{ type: "text", text: `a-${id}` }] },
  ];
  const idle = { event: { type: "session.idle", properties: { sessionID: "s1" } } };
  try {
    const hooks = await MeminiPlugin(
      { client, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    turn = mkTurn("a0");
    await hooks.event(idle);
    assert.equal(posts.length, 1, "first idle captures the turn");
    // A re-fired idle for the same turn is still deduped.
    await hooks.event(idle);
    assert.equal(posts.length, 1, "same turn must not capture twice");
    // Push 200 more distinct assistant ids through the window (cap is 200).
    for (let i = 1; i <= 200; i++) {
      turn = mkTurn(`a${i}`);
      await hooks.event(idle);
    }
    assert.equal(posts.length, 201);
    // a0 has aged out of the window: a re-fired idle for it captures again.
    turn = mkTurn("a0");
    await hooks.event(idle);
    assert.equal(posts.length, 202, "an id evicted from the window is re-capturable");
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

// The floor rides the wire as min_rank_score (a server-enforced floor on the
// FINAL composite score), never the fused-scale min_score. A server that
// accepts it is authoritative: its result set is NOT re-filtered client-side.
test("chat.message floors on min_rank_score (never min_score) and trusts an enforcing server", async () => {
  const realFetch = globalThis.fetch;
  const searches = [];
  globalThis.fetch = async (url, init) => {
    if (String(url).endsWith("/v1/handshake")) {
      return { ok: true, async json() { return {}; }, async text() { return ""; } };
    }
    if (String(url).endsWith("/v1/search")) searches.push(init && init.body ? JSON.parse(init.body) : {});
    return {
      ok: true,
      async json() {
        // A hit below the floor: a server that enforced the floor would have
        // dropped it, so its presence proves the client does not re-filter.
        return {
          results: [
            { score: 0.9, memory: { tier: "semantic", summary: "high relevance" } },
            { score: 0.1, memory: { tier: "episodic", summary: "low relevance kept" } },
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
    const output = { parts: [{ type: "text", text: "user prompt", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    assert.equal(searches[0].min_rank_score, 0.4, "the knob rides as min_rank_score");
    assert.equal(searches[0].min_score, undefined, "the fused-scale min_score is never sent");
    const injected = output.parts[0];
    assert.ok(injected.text.includes("high relevance"));
    assert.ok(injected.text.includes("low relevance kept"), "an enforcing server's result set is authoritative");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Older server: it 400s min_rank_score, so one retry strips it and the client
// applies the composite floor as a fallback.
test("chat.message applies the floor client-side only as a fallback when the server rejects min_rank_score", async () => {
  const realFetch = globalThis.fetch;
  const searches = [];
  globalThis.fetch = async (url, init) => {
    if (String(url).endsWith("/v1/handshake")) {
      return { ok: true, async json() { return {}; }, async text() { return ""; } };
    }
    const body = init && init.body ? JSON.parse(init.body) : {};
    if (String(url).endsWith("/v1/search")) searches.push(body);
    if (String(url).endsWith("/v1/search") && body.min_rank_score !== undefined) {
      return { ok: false, status: 400, async json() { return {}; }, async text() { return 'unknown field "min_rank_score"'; } };
    }
    return {
      ok: true,
      async json() {
        return {
          results: [
            { score: 0.9, memory: { tier: "semantic", summary: "high relevance" } },
            { score: 0.1, memory: { tier: "episodic", summary: "low — should be filtered" } },
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
    const output = { parts: [{ type: "text", text: "user prompt", sessionID: "s1", messageID: "m1" }] };
    await hooks["chat.message"]({ sessionID: "s1" }, output);
    assert.equal(searches.length, 2, "one strip-and-retry, then it stops");
    assert.equal(searches[0].min_rank_score, 0.4, "the first attempt carries the floor");
    assert.equal(searches[1].min_rank_score, undefined, "the retry strips min_rank_score");
    assert.equal(searches[1].min_score, undefined, "the retry never resurrects min_score");
    const injected = output.parts[0];
    assert.ok(injected.text.includes("high relevance"));
    assert.ok(!injected.text.includes("low — should be filtered"), "the stripped floor is enforced client-side");
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
  globalThis.fetch = async (url, init) => {
    if (!String(url).endsWith("/v1/search")) return okJson({});
    const body = init && init.body ? JSON.parse(init.body) : {};
    // Old server rejects the composite floor, so the client applies it as a
    // fallback — which is exactly what keeps the low carryover hit out below.
    if (body.min_rank_score !== undefined) {
      return { ok: false, status: 400, async json() { return {}; }, async text() { return 'unknown field "min_rank_score"'; } };
    }
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

// --- turn-capture truncation conformance ------------------------------------
//
// This plugin ships standalone (no build step), so it carries its own copy of
// the client core's truncateForCapture. The copy is only useful if it IS the
// same function, and it once was not: it and the core disagreed on a NaN cap
// from birth, each passing its own tests. The shared fixture is what makes the
// two the same function rather than two functions with the same name.
const captureVectors = JSON.parse(
  readFileSync(
    join(
      fileURLToPath(new URL(".", import.meta.url)),
      "..",
      "..",
      "..",
      "packages",
      "memini-client",
      "test",
      "fixtures",
      "capture-vectors.json",
    ),
    "utf8",
  ),
);

for (const v of captureVectors.cases) {
  test(`capture vector: ${v.name}`, () => {
    assert.equal(truncateForCapture(v.text, v.max), v.expect);
  });
}

test("capture vectors: the fixture's marker is this copy's marker", () => {
  assert.equal(truncateForCapture("ab", 1), "a" + captureVectors.marker);
});

test("buildTurnCapture: 0 on a side captures it whole", () => {
  assert.equal(buildTurnCapture("uuu", "aaa", 0, 2), "uuu\n\naa" + captureVectors.marker);
});

// --- windowed injection cooldown (enforce-core parity) ----------------------
//
// This plugin ships standalone, so it carries copies of the enforcement core's
// injectedSuppressed / injectedIdentity (packages/memini-client/src/enforce/).
// The shared golden vectors are what keep the copies the same functions —
// replayed here for the two functions this plugin implements.
const enforcementVectors = JSON.parse(
  readFileSync(
    join(
      fileURLToPath(new URL(".", import.meta.url)),
      "..",
      "..",
      "..",
      "packages",
      "memini-client",
      "vectors",
      "enforcement.json",
    ),
    "utf8",
  ),
);

for (const v of enforcementVectors.filter((c) => c.fn === "injectedSuppressed")) {
  test(`enforcement vector: injectedSuppressed / ${v.name}`, () => {
    assert.equal(injectedSuppressed(v.input.entry, v.input.identity, v.input.opts), v.expected);
  });
}

for (const v of enforcementVectors.filter((c) => c.fn === "injectedIdentity")) {
  test(`enforcement vector: injectedIdentity / ${v.name}`, () => {
    assert.equal(injectedIdentity(v.input.m), v.expected);
  });
}

test("resolveConfig resolves the inject cooldown knobs: option > env > default, explicit 0 respected", () => {
  const defaults = resolveConfig({}, undefined, "/repo");
  assert.equal(defaults.inject_cooldown_ms, 1800000);
  assert.equal(defaults.inject_cooldown_prompts, 3);
  assert.equal(defaults.explicit.inject_cooldown_ms, false);
  assert.equal(defaults.explicit.inject_cooldown_prompts, false);

  const env = { MEMINI_INJECT_COOLDOWN_MS: "5000", MEMINI_INJECT_COOLDOWN_PROMPTS: "0" };
  const fromEnv = resolveConfig(env, undefined, "/repo");
  assert.equal(fromEnv.inject_cooldown_ms, 5000);
  assert.equal(fromEnv.inject_cooldown_prompts, 0, "an explicit 0 must be respected");
  assert.equal(fromEnv.explicit.inject_cooldown_ms, true);
  assert.equal(fromEnv.explicit.inject_cooldown_prompts, true);

  const fromOpts = resolveConfig(env, { inject_cooldown_ms: 0, inject_cooldown_prompts: 9 }, "/repo");
  assert.equal(fromOpts.inject_cooldown_ms, 0, "option 0 beats env");
  assert.equal(fromOpts.inject_cooldown_prompts, 9);
});

test("effectiveConfig fills the cooldown knobs from the handshake settings unless explicitly set", () => {
  const cfg = resolveConfig({}, undefined, "/repo");
  const hs = { settings: { inject_cooldown_ms: 60000, inject_cooldown_prompts: 5 } };
  const live = effectiveConfig(cfg, hs);
  assert.equal(live.inject_cooldown_ms, 60000);
  assert.equal(live.inject_cooldown_prompts, 5);

  const pinned = resolveConfig({ MEMINI_INJECT_COOLDOWN_MS: "1000" }, { inject_cooldown_prompts: 1 }, "/repo");
  const still = effectiveConfig(pinned, hs);
  assert.equal(still.inject_cooldown_ms, 1000, "env beats the server");
  assert.equal(still.inject_cooldown_prompts, 1, "option beats the server");

  assert.equal(effectiveConfig(cfg, null).inject_cooldown_ms, 1800000, "no handshake -> built-in default");
});

// chat.message flow: suppressed while inside EITHER window, re-served (and
// re-recorded) once BOTH lapse. Time is skewed via Date.now; the prompt
// counter advances once per chat.message.
test("windowed cooldown: a shown memory re-serves only after BOTH windows lapse", async () => {
  const realFetch = globalThis.fetch;
  const realNow = Date.now;
  let skew = 0;
  Date.now = () => realNow.call(Date) + skew;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) throw new Error("no handshake");
    return {
      ok: true,
      async json() {
        return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "prior note" } }] };
      },
      async text() { return ""; },
    };
  };
  const msg = (id) => ({ parts: [{ type: "text", text: "query", sessionID: "s1", messageID: id }] });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    const first = msg("m1");
    await hooks["chat.message"]({ sessionID: "s1" }, first);
    assert.equal(first.parts.length, 2, "first message injects");
    // In both windows: suppressed.
    const second = msg("m2");
    await hooks["chat.message"]({ sessionID: "s1" }, second);
    assert.equal(second.parts.length, 1, "in-window repeat suppresses");
    // Time lapses (31 min > the 30-min default) but the prompt window has not
    // (this is prompt 3 since injection at prompt 1: delta 2 < 3): still suppressed.
    skew = 31 * 60_000;
    const third = msg("m3");
    await hooks["chat.message"]({ sessionID: "s1" }, third);
    assert.equal(third.parts.length, 1, "time lapsed but prompt window holds: suppressed");
    // Prompt 4: delta 3 >= 3 — both windows lapsed, re-served and re-recorded.
    const fourth = msg("m4");
    await hooks["chat.message"]({ sessionID: "s1" }, fourth);
    assert.equal(fourth.parts.length, 2, "both windows lapsed: re-served");
    // The re-record restarts both windows: suppressed again.
    const fifth = msg("m5");
    await hooks["chat.message"]({ sessionID: "s1" }, fifth);
    assert.equal(fifth.parts.length, 1, "the re-shown memory suppresses again");
  } finally {
    globalThis.fetch = realFetch;
    Date.now = realNow;
  }
});

test("windowed cooldown: prompt dimension alone governs when the time window is 0", async () => {
  process.env.MEMINI_INJECT_COOLDOWN_MS = "0";
  process.env.MEMINI_INJECT_COOLDOWN_PROMPTS = "2";
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) throw new Error("no handshake");
    return {
      ok: true,
      async json() {
        return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "prior note" } }] };
      },
      async text() { return ""; },
    };
  };
  const msg = (id) => ({ parts: [{ type: "text", text: "query", sessionID: "s1", messageID: id }] });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    const first = msg("m1");
    await hooks["chat.message"]({ sessionID: "s1" }, first);
    assert.equal(first.parts.length, 2, "prompt 1 injects (n=1)");
    const second = msg("m2");
    await hooks["chat.message"]({ sessionID: "s1" }, second);
    assert.equal(second.parts.length, 1, "prompt 2: delta 1 < 2, suppressed");
    const third = msg("m3");
    await hooks["chat.message"]({ sessionID: "s1" }, third);
    assert.equal(third.parts.length, 2, "prompt 3: delta 2 >= 2, re-served");
  } finally {
    globalThis.fetch = realFetch;
    delete process.env.MEMINI_INJECT_COOLDOWN_MS;
    delete process.env.MEMINI_INJECT_COOLDOWN_PROMPTS;
  }
});

test("windowed cooldown: a content change bypasses the window and re-injects", async () => {
  const realFetch = globalThis.fetch;
  let searchCalls = 0;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) throw new Error("no handshake");
    if (String(url).endsWith("/v1/search")) searchCalls++;
    const content = searchCalls <= 1 ? "version one" : "version two — updated";
    return {
      ok: true,
      async json() {
        return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: content } }] };
      },
      async text() { return ""; },
    };
  };
  const msg = (id) => ({ parts: [{ type: "text", text: "query", sessionID: "s1", messageID: id }] });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    const first = msg("m1");
    await hooks["chat.message"]({ sessionID: "s1" }, first);
    assert.match(first.parts[0].text, /version one/);
    const second = msg("m2");
    await hooks["chat.message"]({ sessionID: "s1" }, second);
    assert.equal(second.parts.length, 2, "an updated memory must re-inject inside the window");
    assert.match(second.parts[0].text, /version two/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("windowed cooldown: both knobs at zero restores forever suppression (legacy #134)", async () => {
  process.env.MEMINI_INJECT_COOLDOWN_MS = "0";
  process.env.MEMINI_INJECT_COOLDOWN_PROMPTS = "0";
  const realFetch = globalThis.fetch;
  const realNow = Date.now;
  let skew = 0;
  Date.now = () => realNow.call(Date) + skew;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/v1/handshake")) throw new Error("no handshake");
    return {
      ok: true,
      async json() {
        return { results: [{ score: 0.9, memory: { id: "m1", tier: "semantic", summary: "prior note" } }] };
      },
      async text() { return ""; },
    };
  };
  const msg = (id) => ({ parts: [{ type: "text", text: "query", sessionID: "s1", messageID: id }] });
  try {
    const hooks = await MeminiPlugin(
      { client: {}, worktree: "/tmp/proj", directory: "/tmp/proj" },
      { base_url: "http://localhost:8080" },
    );
    const first = msg("m1");
    await hooks["chat.message"]({ sessionID: "s1" }, first);
    assert.equal(first.parts.length, 2);
    skew = 24 * 60 * 60_000; // even a day later
    const later = msg("m2");
    await hooks["chat.message"]({ sessionID: "s1" }, later);
    assert.equal(later.parts.length, 1, "both knobs at zero must suppress forever");
  } finally {
    globalThis.fetch = realFetch;
    Date.now = realNow;
    delete process.env.MEMINI_INJECT_COOLDOWN_MS;
    delete process.env.MEMINI_INJECT_COOLDOWN_PROMPTS;
  }
});
