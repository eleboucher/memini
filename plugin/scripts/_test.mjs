// Test harness for the memini plugin's hook scripts.
//
// Pure Node (no test framework). Run with: node --test plugin/scripts/_test.mjs
//
// Strategy:
//   1. Spin up a tiny in-process mock memini server.
//   2. Drive each hook script by piping a fake agent payload into its stdin.
//   3. Assert the hook resolves the right namespace (via the /v1/handshake
//      flow, a cached handshake, or local derivation), hits the mock with the
//      right payload, and produces the right stdout.
//
// The config-handshake redesign inverts namespace resolution to the server:
// SessionStart POSTs /v1/handshake and caches the result per session
// (pid-<ppid>.handshake.json); every other hook reads that cache. The hot-path
// hooks (Pre/PostToolUse) are network-free — cache-only.
//
// No external network, no real embeddings. CI-friendly.

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, execSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve, basename } from "node:path";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import http from "node:http";

const __dirname = dirname(fileURLToPath(import.meta.url));
const SCRIPTS = __dirname;

// A plaintext, loopback address with nothing listening: performHandshake gets an
// immediate ECONNREFUSED, so a hook that expects to degrade to local derivation
// does so fast and deterministically instead of waiting on a real server that a
// developer may have running on :8080.
const DEAD_URL = "http://127.0.0.1:1";

// Point XDG_CONFIG_HOME at an empty temp dir for the whole run (spawned hooks
// inherit it) so a developer's real ~/.config/memini can't leak into these tests.
process.env.XDG_CONFIG_HOME = mkdtempSync(join(tmpdir(), "memini-config-"));

// Strip every ambient MEMINI_* env var for the whole run. A developer's shell
// (or this very plugin, running against a live memini) commonly exports
// MEMINI_NAMESPACE, MEMINI_CAPTURE_TURNS, MEMINI_BASE_URL, MEMINI_API_KEY, etc.;
// those leak into both in-process calls and spawned hooks and clobber tests that
// assert the *computed* default. Each test sets whatever MEMINI_* it needs.
for (const k of Object.keys(process.env)) {
  if (k.startsWith("MEMINI_")) delete process.env[k];
}

// Each test that touches the session buffer / handshake cache gets an isolated
// cache dir so runs don't pollute the real ~/.cache or each other.
function freshCache() {
  return mkdtempSync(join(tmpdir(), "memini-test-"));
}

// A full HandshakeResponse (api/openapi.yaml), with sensible defaults. Override
// any field: mkHS({ namespace: "x", settings: { session_digest: false } }).
function mkHS(over = {}) {
  const ns = over.namespace ?? "memini";
  return {
    namespace: ns,
    namespace_source: over.namespace_source ?? "remote",
    ...(over.pin ? { pin: over.pin } : {}),
    identity: over.identity ?? { authenticated: true, key_name: "test-key" },
    settings: over.settings ?? {},
    settings_sources: over.settings_sources ?? {},
    read_set: over.read_set ?? [{ namespace: ns, origin: "primary" }],
    server: over.server ?? { version: "test-server", default_namespace: "default" },
  };
}

// Wrap a mock handler so POST /v1/handshake is served automatically (and NOT
// recorded by the inner handler's `hits`), letting an on-miss hook do its live
// handshake while the test's assertions stay focused on the capture/recall
// calls. `hs` may be an object or (body) => object.
function withHandshake(hs, handler) {
  return (req, res, body) => {
    if (req.method === "POST" && req.url === "/v1/handshake") {
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 200;
      res.end(JSON.stringify(typeof hs === "function" ? hs(body) : hs));
      return;
    }
    handler(req, res, body);
  };
}

// Seed the per-session handshake cache the way SessionStart would, so a
// subsequent hook resolves from the cache (no live handshake). Keyed by the
// spawned hook's ppid — which, for runHook, is THIS test process's pid.
async function primeCache(cache, cwd, hs, extraEnv = {}) {
  const mod = await import("./_client.gen.mjs");
  const env = { ...process.env, XDG_CACHE_HOME: cache, ...extraEnv };
  const facts = mod.gatherFacts(cwd, env);
  mod.writeCachedHandshake(process.pid, cwd, facts, hs, env);
}

// Run `fn` with process.env overrides applied (undefined deletes), restoring
// the previous values after.
async function withEnv(overrides, fn) {
  const prev = {};
  for (const [k, v] of Object.entries(overrides)) {
    prev[k] = process.env[k];
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  try {
    return await fn();
  } finally {
    for (const [k, v] of Object.entries(prev)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
}

function runHook(script, payload, env = {}) {
  // A developer shell may export transport vars pointing at a real memini; strip
  // them so each test's explicit env points the hook at the in-process mock.
  const base = { ...process.env };
  for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_NAMESPACE", "MEMINI_HOME"]) delete base[k];
  return new Promise((resolveProm, reject) => {
    const child = spawn("node", [resolve(SCRIPTS, script)], {
      env: { ...base, ...env, MEMINI_DEBUG: "1" },
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (c) => (stdout += c));
    child.stderr.on("data", (c) => (stderr += c));
    child.on("close", (code) => {
      if (code !== 0) {
        reject(new Error(`${script} exited ${code}\nstderr: ${stderr}`));
        return;
      }
      resolveProm({ stdout, stderr });
    });
    child.stdin.end(payload);
  });
}

// Run a command script (namespace.mjs / status.mjs) with argv args and a cwd —
// these read process.cwd() and process.argv, not stdin.
function runCommand(script, argv, env = {}, cwd = process.cwd()) {
  const base = { ...process.env };
  for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_NAMESPACE", "MEMINI_HOME"]) delete base[k];
  return new Promise((resolveProm, reject) => {
    const child = spawn("node", [resolve(SCRIPTS, script), ...argv], {
      cwd,
      env: { ...base, ...env },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (c) => (stdout += c));
    child.stderr.on("data", (c) => (stderr += c));
    child.on("close", (code) => resolveProm({ code, stdout, stderr }));
    child.on("error", reject);
  });
}

function startMockServer(handler) {
  return new Promise((resolveProm) => {
    const server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => handler(req, res, body));
    });
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      const url = `http://127.0.0.1:${port}`;
      const close = () => new Promise((r) => server.close(() => r(undefined)));
      resolveProm({ url, close });
    });
  });
}

// A throwaway non-git directory with a distinct basename, for local-derivation
// and per-session isolation tests.
function tmpDir(prefix) {
  return mkdtempSync(join(tmpdir(), `memini-${prefix}-`));
}

// ─── getSessionContext: the resolution + cache policy core ────────────────

test("getSessionContext(never): cache miss → degraded local namespace, zero network", async () => {
  const { getSessionContext } = await import("./_shared.mjs");
  const cache = freshCache();
  const env = { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL };
  const ctx = await getSessionContext({ cwd: __dirname, ppid: 987654, allowNetwork: "never", env });
  assert.equal(ctx.degraded, true, "no cache → degraded");
  assert.equal(ctx.namespace, "memini", "local derivation of the memini repo");
  assert.match(ctx.source, /^local-/);
  assert.equal(ctx.handshake, undefined);
});

test("getSessionContext(never): cache hit → server namespace + settings; env overrides server", async () => {
  const { getSessionContext } = await import("./_shared.mjs");
  const { gatherFacts, writeCachedHandshake } = await import("./_client.gen.mjs");
  const cache = freshCache();
  const env = { XDG_CACHE_HOME: cache };
  const facts = gatherFacts(__dirname, env);
  writeCachedHandshake(
    4242,
    __dirname,
    facts,
    mkHS({ namespace: "srv/ns", settings: { session_digest: false, inject_briefing_facts: 2 } }),
    env,
  );

  const ctx = await getSessionContext({ cwd: __dirname, ppid: 4242, allowNetwork: "never", env });
  assert.equal(ctx.degraded, false);
  assert.equal(ctx.namespace, "srv/ns");
  assert.equal(ctx.source, "server:remote");
  assert.equal(ctx.setting("session_digest").value, false);
  assert.equal(ctx.setting("session_digest").source, "server");
  assert.equal(ctx.setting("inject_briefing_facts").value, 2);

  // A local env var still wins over the server value.
  const ctx2 = await getSessionContext({
    cwd: __dirname,
    ppid: 4242,
    allowNetwork: "never",
    env: { ...env, MEMINI_SESSION_DIGEST: "1" },
  });
  assert.equal(ctx2.setting("session_digest").value, true);
  assert.equal(ctx2.setting("session_digest").source, "env-override");
});

test("getSessionContext(always): live handshake resolves + caches; on-miss reuses it", async () => {
  const { getSessionContext } = await import("./_shared.mjs");
  const cache = freshCache();
  let handshakes = 0;
  const { url, close } = await startMockServer((req, res) => {
    if (req.url === "/v1/handshake") {
      handshakes++;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(mkHS({ namespace: "live/ns" })));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const env = { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: url };
    const a = await getSessionContext({ cwd: __dirname, ppid: 7777, allowNetwork: "always", env, timeoutMs: 2000 });
    assert.equal(a.namespace, "live/ns");
    assert.equal(a.degraded, false);
    assert.equal(handshakes, 1);

    const b = await getSessionContext({ cwd: __dirname, ppid: 7777, allowNetwork: "on-miss", env });
    assert.equal(b.namespace, "live/ns");
    assert.equal(handshakes, 1, "on-miss must reuse the cache, not re-handshake");
  } finally {
    await close();
  }
});

test("getSessionContext(always): handshake failure → degraded, no cache written", async () => {
  const { getSessionContext } = await import("./_shared.mjs");
  const { readCachedHandshake, gatherFacts } = await import("./_client.gen.mjs");
  const cache = freshCache();
  const env = { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL };
  const ctx = await getSessionContext({ cwd: __dirname, ppid: 5150, allowNetwork: "always", env, timeoutMs: 1500 });
  assert.equal(ctx.degraded, true);
  assert.equal(ctx.namespace, "memini");
  // The absence of a cache entry is the degraded signal later hooks read.
  assert.equal(readCachedHandshake(5150, __dirname, gatherFacts(__dirname, env), env), undefined);
});

// ─── concurrency (the PR-#111 scenario) ───────────────────────────────────

test("concurrency: per-session handshake caches don't cross-contaminate", async () => {
  const { getSessionContext } = await import("./_shared.mjs");
  const { gatherFacts, writeCachedHandshake } = await import("./_client.gen.mjs");
  const cache = freshCache(); // deliberately SHARED between the two sessions
  const env = { XDG_CACHE_HOME: cache };
  const repoA = tmpDir("alpha");
  const repoB = tmpDir("beta");

  writeCachedHandshake(101, repoA, gatherFacts(repoA, env), mkHS({ namespace: "ns-alpha" }), env);
  writeCachedHandshake(202, repoB, gatherFacts(repoB, env), mkHS({ namespace: "ns-beta" }), env);

  const a = await getSessionContext({ cwd: repoA, ppid: 101, allowNetwork: "never", env });
  const b = await getSessionContext({ cwd: repoB, ppid: 202, allowNetwork: "never", env });
  assert.equal(a.namespace, "ns-alpha", "session A resolves its own namespace");
  assert.equal(b.namespace, "ns-beta", "session B resolves its own namespace");

  // Session A asking about session B's cwd must NOT read A's cached namespace —
  // the cache is keyed by (ppid, cwd, facts), so this is a miss → degraded local.
  const cross = await getSessionContext({ cwd: repoB, ppid: 101, allowNetwork: "never", env });
  assert.notEqual(cross.namespace, "ns-alpha", "no cross-contamination across sessions");
  assert.equal(cross.degraded, true);
});

// ─── session-start (the one hook that does the live handshake) ────────────

test("session-start.mjs: an empty briefing still gets the memory directive", async () => {
  // A brand-new project (no memories) still needs the save directive — returning
  // early on an empty briefing used to drop it in exactly the sessions it matters.
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "memini", pinned: [], facts: [], procedures: [], recent: [] }));
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "empty1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache() },
    );
    assert.doesNotMatch(stdout, /<memini-context/, "nothing to inject, so no context block");
    assert.match(stdout, /memini-memory-directive/, "but the save directive must still be emitted");
  } finally {
    await close();
  }
});

test("session-start.mjs: handshake DOWN → degraded local namespace, still emits the directive", async () => {
  // Server down should degrade to "no context", not to "and also stop saving".
  // The namespace falls back to local derivation and a one-line warning is
  // printed to stderr.
  const { stdout, stderr } = await runHook(
    "session-start.mjs",
    JSON.stringify({ session_id: "down1", cwd: __dirname }),
    { MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: freshCache() },
  );
  assert.match(stdout, /memini-memory-directive/);
  assert.match(stderr, /server unreachable — using local namespace "memini"/);
});

test("session-start.mjs: fetches the briefing under the HANDSHAKE-resolved namespace", async () => {
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "team/app" }), (req, res, body) => {
      hits.push({ method: req.method, url: req.url, ns: req.headers["x-memini-namespace"], body });
      res.setHeader("Content-Type", "application/json");
      // Strict path match: the header-scoped route only, so a regression back
      // to a path-param URL fails this test instead of being echoed JSON.
      if (new URL(req.url, "http://x").pathname === "/v1/namespaces/briefing") {
        res.end(
          JSON.stringify({
            namespace: "team/app",
            pinned: [],
            facts: [{ content: "convention: use tabs" }],
            procedures: [],
            recent: [{ content: "last session did X" }],
          }),
        );
      } else {
        res.statusCode = 404;
        res.end();
      }
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache() },
    );
    assert.match(stdout, /<memini-context[^>]*>/, "should emit context block");
    assert.match(stdout, /last session did X/, "should surface prior memory");
    assert.match(stdout, /memory_remember/, "should inject the memory directive");
    assert.ok(!stdout.includes("<memory>"), "injected context must not contain memory markup");
    // Exactly one briefing call (the handshake is served separately), under the
    // namespace the SERVER resolved — not a locally-derived one.
    assert.equal(hits.length, 1, `expected 1 briefing call, got ${hits.length}`);
    assert.equal(hits[0].method, "GET");
    assert.equal(hits[0].ns, "team/app", `expected namespace=team/app, got ${hits[0].ns}`);
    // Header-scoped route: NO namespace path segment — the namespace travels
    // in X-Memini-Namespace (asserted above), matching api/openapi.yaml.
    assert.match(hits[0].url, /^\/v1\/namespaces\/briefing\?/);
  } finally {
    await close();
  }
});

test("session-start.mjs: briefing survives an SPA catch-all serving HTML on every wrong path", async () => {
  // Regression for the route bug where getBriefing requested
  // /v1/namespaces/<ns>/briefing (path param). That route does not exist; a
  // real deployment's admin-UI SPA catch-all answered it 200-with-HTML, and
  // getBriefing silently nulled — SessionStart injected only the memory
  // directive, never actual briefing content. This mock reproduces the real
  // server's shape EXACTLY: JSON only on the exact header-scoped path
  // (/v1/namespaces/briefing, no path segment) WITH X-Memini-Namespace set;
  // every other GET — including the old path-param form — 200s with HTML like
  // the SPA does. A mock that echoed JSON for whatever path the code requested
  // would recreate the blind spot that let the bug through.
  const seen = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "team/app" }), (req, res) => {
      const pathname = new URL(req.url, "http://x").pathname;
      seen.push({ pathname, ns: req.headers["x-memini-namespace"] });
      if (pathname === "/v1/namespaces/briefing" && req.headers["x-memini-namespace"]) {
        res.setHeader("Content-Type", "application/json");
        res.end(
          JSON.stringify({
            namespace: "team/app",
            pinned: [],
            facts: [{ content: "briefing served by the real route" }],
            procedures: [],
            recent: [],
          }),
        );
        return;
      }
      // The SPA catch-all: 200 + HTML for anything else, old path included.
      res.statusCode = 200;
      res.setHeader("Content-Type", "text/html; charset=utf-8");
      res.end("<!doctype html><html><head><title>memini admin</title></head><body><div id=\"app\"></div></body></html>");
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "spa1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache() },
    );
    assert.match(stdout, /<memini-context[^>]*>/, "SessionStart must inject a real context block");
    assert.match(stdout, /briefing served by the real route/, "the briefing content must come from the header-scoped route");
    const briefingCalls = seen.filter((s) => s.pathname.includes("briefing"));
    assert.ok(
      briefingCalls.every((s) => s.pathname === "/v1/namespaces/briefing"),
      `every briefing request must use the header-scoped path, got ${JSON.stringify(briefingCalls)}`,
    );
  } finally {
    await close();
  }
});

test("session-start.mjs: emits the briefing Scope line the MCP tools tell the model to read", async () => {
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          namespace: "memini",
          scope_header: "Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4)",
          pinned: [],
          facts: [{ content: "convention: use tabs" }],
          procedures: [],
          recent: [],
        }),
      );
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "scope1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache() },
    );
    assert.match(stdout, /Scope: acme\/phoenix\/api ← acme\/phoenix\(3\) ← acme\(4\)/);
  } finally {
    await close();
  }
});

test("session-start.mjs: briefing caps honored from server settings; env overrides server", async () => {
  const seen = [];
  const handler = withHandshake(
    (body) => mkHS({ namespace: "memini", settings: { inject_briefing_facts: 1 } }),
    (req, res) => {
      const u = new URL(req.url, "http://x");
      seen.push(u.searchParams.get("per_section_facts"));
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "memini", pinned: [], facts: [{ content: "f1" }], procedures: [], recent: [] }));
    },
  );

  let srv = await startMockServer(handler);
  try {
    await runHook("session-start.mjs", JSON.stringify({ session_id: "cap1", cwd: __dirname }), {
      MEMINI_BASE_URL: srv.url,
      XDG_CACHE_HOME: freshCache(),
    });
    assert.equal(seen[0], "1", "server-provided inject_briefing_facts=1 must reach the briefing call");
  } finally {
    await srv.close();
  }

  seen.length = 0;
  srv = await startMockServer(handler);
  try {
    await runHook("session-start.mjs", JSON.stringify({ session_id: "cap2", cwd: __dirname }), {
      MEMINI_BASE_URL: srv.url,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_INJECT_BRIEFING_FACTS: "4",
    });
    assert.equal(seen[0], "4", "a local env var must override the server setting");
  } finally {
    await srv.close();
  }
});

test("session-start.mjs: MEMINI_INJECT_BRIEFING_* caps per-section results", async () => {
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      hits.push(req.url);
      const u = new URL(req.url, "http://x");
      const cap = (param, all) => {
        const raw = u.searchParams.get(param);
        if (raw === null) return all;
        const n = Number.parseInt(raw, 10);
        if (!Number.isFinite(n) || n < 0) return all;
        return all.slice(0, n);
      };
      const all = {
        pinned: [{ content: "p1" }, { content: "p2" }],
        facts: [{ content: "f1" }, { content: "f2" }, { content: "f3" }],
        procedures: [{ content: "pr1" }],
        recent: [{ content: "r1" }, { content: "r2" }],
      };
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          namespace: "memini",
          pinned: cap("per_section_pinned", all.pinned),
          facts: cap("per_section_facts", all.facts),
          procedures: cap("per_section_procedures", all.procedures),
          recent: cap("per_section_recent", all.recent),
        }),
      );
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      {
        MEMINI_BASE_URL: url,
        XDG_CACHE_HOME: freshCache(),
        MEMINI_INJECT_BRIEFING_PINNED: "1",
        MEMINI_INJECT_BRIEFING_FACTS: "0",
        MEMINI_INJECT_BRIEFING_PROCEDURES: "5",
        MEMINI_INJECT_BRIEFING_RECENT: "3",
      },
    );
    assert.equal(hits.length, 1);
    const u = new URL(hits[0], "http://x");
    assert.equal(u.searchParams.get("per_section_pinned"), "1");
    assert.equal(u.searchParams.get("per_section_facts"), "0");
    assert.equal(u.searchParams.get("per_section_procedures"), "5");
    assert.equal(u.searchParams.get("per_section_recent"), "3");
    assert.match(stdout, /- p1/);
    assert.doesNotMatch(stdout, /p2/);
    assert.doesNotMatch(stdout, /Decisions/);
  } finally {
    await close();
  }
});

test("session-start.mjs: MEMINI_INJECT_BRIEFING_MAX_TOK truncates the rendered block", async () => {
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          namespace: "memini",
          pinned: [],
          facts: [
            { content: "alpha alpha alpha alpha alpha alpha alpha alpha alpha alpha" },
            { content: "beta beta beta beta beta beta beta beta beta beta beta beta" },
            { content: "gamma gamma gamma gamma gamma gamma gamma gamma gamma gamma" },
          ],
          procedures: [],
          recent: [],
        }),
      );
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), MEMINI_INJECT_BRIEFING_MAX_TOK: "20" },
    );
    assert.match(stdout, /\[...\s+\d+ item\(s\) truncated/);
    assert.match(stdout, /alpha/);
  } finally {
    await close();
  }
});

test("session-start.mjs: MEMINI_INJECT_LABELS=tier renders tier annotations", async () => {
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          namespace: "memini",
          pinned: [],
          facts: [{ content: "use tabs in this project", tier: "semantic" }],
          procedures: [],
          recent: [],
        }),
      );
    }),
  );
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), MEMINI_INJECT_LABELS: "tier,reason" },
    );
    assert.match(stdout, /\[semantic · durable fact\]/);
    assert.match(stdout, /use tabs in this project/);
  } finally {
    await close();
  }
});

test("MEMORY_INSTRUCTION tells the agent about visibility, not just tier", async () => {
  const { MEMORY_INSTRUCTION } = await import("./_shared.mjs");
  assert.match(MEMORY_INSTRUCTION, /visibility/);
  assert.match(MEMORY_INSTRUCTION, /personal/);
});

// ─── REST client (postJSON / getJSON / postRemember) ──────────────────────

test("X-Memini-Home header: emitted on GET/POST when MEMINI_HOME is set, absent when unset", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res) => {
    hits.push({ method: req.method, home: req.headers["x-memini-home"] });
    res.statusCode = 200;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ ok: true }));
  });
  const prevUrl = process.env.MEMINI_BASE_URL;
  const prevHome = process.env.MEMINI_HOME;
  process.env.MEMINI_BASE_URL = url;
  try {
    process.env.MEMINI_HOME = "personal/acme";
    const withHome = await import("./_shared.mjs?cb=home-set-" + Date.now());
    await withHome.getJSON("/v1/whatever", "memini");
    await withHome.postJSON("/v1/whatever", { x: 1 }, "memini");

    delete process.env.MEMINI_HOME;
    const withoutHome = await import("./_shared.mjs?cb=home-unset-" + Date.now());
    await withoutHome.getJSON("/v1/whatever", "memini");
    await withoutHome.postJSON("/v1/whatever", { x: 1 }, "memini");

    assert.equal(hits.length, 4);
    assert.equal(hits[0].home, "personal/acme", "GET must carry X-Memini-Home when set");
    assert.equal(hits[1].home, "personal/acme", "POST must carry X-Memini-Home when set");
    assert.equal(hits[2].home, undefined, "GET must omit X-Memini-Home when unset");
    assert.equal(hits[3].home, undefined, "POST must omit X-Memini-Home when unset");
  } finally {
    if (prevUrl === undefined) delete process.env.MEMINI_BASE_URL;
    else process.env.MEMINI_BASE_URL = prevUrl;
    if (prevHome === undefined) delete process.env.MEMINI_HOME;
    else process.env.MEMINI_HOME = prevHome;
    await close();
  }
});

test("postRemember forwards visibility when the caller provides one; omits it otherwise", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ body });
    res.statusCode = 201;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ id: "m1" }));
  });
  const prevUrl = process.env.MEMINI_BASE_URL;
  process.env.MEMINI_BASE_URL = url;
  try {
    const { postRemember } = await import("./_shared.mjs?cb=post-remember-" + Date.now());
    await postRemember("some fact", "memini", { visibility: "personal" });
    await postRemember("other fact", "memini", {});
    assert.equal(hits.length, 2);
    assert.equal(JSON.parse(hits[0].body).visibility, "personal");
    assert.equal(JSON.parse(hits[1].body).visibility, undefined);
  } finally {
    if (prevUrl === undefined) delete process.env.MEMINI_BASE_URL;
    else process.env.MEMINI_BASE_URL = prevUrl;
    await close();
  }
});

test("postJSON/getJSON: HTTP errors are logged even without MEMINI_DEBUG", async () => {
  const { url, close } = await startMockServer((req, res) => {
    res.statusCode = 500;
    res.end("boom");
  });
  const realError = console.error;
  const logged = [];
  console.error = (...a) => logged.push(a.join(" "));
  const prevUrl = process.env.MEMINI_BASE_URL;
  const prevDebug = process.env.MEMINI_DEBUG;
  process.env.MEMINI_BASE_URL = url;
  delete process.env.MEMINI_DEBUG;
  try {
    const { postJSON, getJSON } = await import("./_shared.mjs?cb=errlog");
    assert.equal(await postJSON("/v1/memories", { content: "x" }, "ns"), null);
    assert.equal(await getJSON("/v1/memories", "ns"), null);
    assert.ok(logged.some((m) => m.includes("POST /v1/memories -> 500")), `expected a POST failure log, got: ${JSON.stringify(logged)}`);
    assert.ok(logged.some((m) => m.includes("GET /v1/memories -> 500")), `expected a GET failure log, got: ${JSON.stringify(logged)}`);
  } finally {
    console.error = realError;
    if (prevUrl === undefined) delete process.env.MEMINI_BASE_URL;
    else process.env.MEMINI_BASE_URL = prevUrl;
    if (prevDebug !== undefined) process.env.MEMINI_DEBUG = prevDebug;
    await close();
  }
});

test("REST client: MEMINI_REQUIRE_HTTPS refuses a bearer over plaintext HTTP (broadened truthiness)", async () => {
  // The plaintext-bearer guard is now the bundle's assertBearerTransportSafe:
  // it THROWS (caught by postJSON → null) under MEMINI_REQUIRE_HTTPS with the
  // broadened 1/true/yes/on parsing, and is otherwise silent (no warn-and-send).
  for (const flag of ["1", "true", "on"]) {
    const logged = [];
    const realError = console.error;
    console.error = (...a) => logged.push(a.join(" "));
    try {
      await withEnv(
        { MEMINI_BASE_URL: "http://memini.invalid", MEMINI_API_KEY: "secret-token", MEMINI_REQUIRE_HTTPS: flag },
        async () => {
          const { postJSON } = await import("./_shared.mjs?cb=https-" + flag + "-" + Date.now());
          assert.equal(await postJSON("/v1/memories", { content: "x" }, "ns"), null);
        },
      );
    } finally {
      console.error = realError;
    }
    assert.ok(logged.some((m) => /plaintext HTTP/.test(m)), `REQUIRE_HTTPS=${flag} must refuse with a plaintext-HTTP message`);
  }
});

test("REST client: plaintext bearer without MEMINI_REQUIRE_HTTPS is sent silently (no guard warning)", async () => {
  const logged = [];
  const realError = console.error;
  console.error = (...a) => logged.push(a.join(" "));
  try {
    await withEnv({ MEMINI_BASE_URL: "http://memini.invalid", MEMINI_API_KEY: "secret-token", MEMINI_REQUIRE_HTTPS: undefined }, async () => {
      const { postJSON } = await import("./_shared.mjs?cb=noguard-" + Date.now());
      // memini.invalid fails DNS → null, but the guard must NOT have fired.
      assert.equal(await postJSON("/v1/memories", { content: "x" }, "ns"), null);
    });
  } finally {
    console.error = realError;
  }
  assert.ok(!logged.some((m) => /plaintext HTTP/.test(m)), "the default posture must not emit the plaintext-HTTP refusal");
});

// ─── capture hooks: post-tool-use / session-end / pre-compact / stop ──────

test("post-tool-use.mjs: buffers state-changing tools, never POSTs", async () => {
  const cache = freshCache();
  const hits = [];
  const { url, close } = await startMockServer((req, res) => {
    hits.push({ url: req.url });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });
  try {
    for (const [tool, input] of [
      ["Edit", { file_path: "foo.go" }],
      ["Read", { file_path: "bar.go" }],
      ["Bash", { command: "go test ./..." }],
    ]) {
      await runHook(
        "post-tool-use.mjs",
        JSON.stringify({ session_id: "buf1", cwd: __dirname, tool_name: tool, tool_input: input }),
        { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
      );
    }
    assert.equal(hits.length, 0, `PostToolUse must not POST, got ${hits.length} calls`);
  } finally {
    await close();
  }
});

test("MEMINI_SESSION_DIGEST=0 stops every session-digest write", async () => {
  const cache = freshCache();
  const off = { XDG_CACHE_HOME: cache, MEMINI_SESSION_DIGEST: "0" };

  // PostToolUse must not even buffer (network-free: env override alone gates it).
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "d0", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "a.go" } }),
    off,
  );
  assert.equal(existsSync(join(cache, "memini", "sessions", "d0.jsonl")), false, "must not write the buffer when nothing will consume it");

  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      hits.push({ url: req.url });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    const env = { ...off, MEMINI_BASE_URL: url, MEMINI_CAPTURE_TURNS: "0", MEMINI_AUTO_SAVE: "0" };
    const payload = JSON.stringify({ session_id: "d0", cwd: __dirname, reason: "user_exit" });
    await runHook("session-end.mjs", payload, env);
    await runHook("stop.mjs", payload, env);
    await runHook("pre-compact.mjs", payload, env);
    assert.deepEqual(hits, [], "no digest, no checkpoint, no pre-compact rescue");
  } finally {
    await close();
  }
});

test("session digests are still on by default", async () => {
  const cache = freshCache();
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "d1", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "a.go" } }),
    { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
  );
  assert.equal(existsSync(join(cache, "memini", "sessions", "d1.jsonl")), true);
});

test("session-end.mjs: no events buffered → no POST, no noise", async () => {
  const cache = freshCache();
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      hits.push({ url: req.url });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "nobuf", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 0, "an empty session must not write a bare end marker");
  } finally {
    await close();
  }
});

test("session-end.mjs: distills buffered events into one digest under the handshake namespace", async () => {
  const cache = freshCache();
  for (const [tool, input] of [
    ["Edit", { file_path: "auth.go" }],
    ["Edit", { file_path: "auth.go" }],
    ["Bash", { command: "go test ./..." }],
  ]) {
    await runHook(
      "post-tool-use.mjs",
      JSON.stringify({ session_id: "dig1", cwd: __dirname, tool_name: tool, tool_input: input }),
      { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
    );
  }

  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res, body) => {
      hits.push({ url: req.url, ns: req.headers["x-memini-namespace"], body });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "dig1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 2, "session-end should write the digest AND supersede the stop: marker");
    assert.equal(hits[0].url, "/v1/memories", "first call is the digest");
    assert.equal(hits[0].ns, "memini", "digest targets the resolved namespace");
    assert.equal(hits[1].url, "/v1/memories/stop%3Adig1/supersede");
    assert.equal(JSON.parse(hits[1].body).by, "session-end:dig1");
    const body = JSON.parse(hits[0].body);
    assert.equal(body.tier, "episodic");
    assert.match(body.content, /3 tool calls/);
    assert.match(body.content, /auth\.go \(2\)/);
    assert.match(body.content, /go test/);
    assert.deepEqual(body.metadata.files, ["auth.go"]);
  } finally {
    await close();
  }
});

test("session-end.mjs: deletes the per-session handshake cache on clean exit", async () => {
  const cache = freshCache();
  // Seed a handshake cache under this test's pid (the hook's ppid).
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const cacheFile = join(cache, "memini", "sessions", `pid-${process.pid}.handshake.json`);
  assert.equal(existsSync(cacheFile), true, "precondition: cache primed");
  const { url, close } = await startMockServer((req, res) => {
    res.statusCode = 201;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ id: "m1" }));
  });
  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "hs-del", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(existsSync(cacheFile), false, "session-end must drop the handshake cache so a recycled pid can't read it");
  } finally {
    await close();
  }
});

test("session-end.mjs: counts files edited through Codex apply_patch", async () => {
  const cache = freshCache();
  const patch = `*** Begin Patch
*** Update File: src/auth.js
@@
-old
+new
*** Add File: src/session.js
+export const session = {};
*** End Patch
`;
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "codexpatch1", cwd: __dirname, tool_name: "apply_patch", tool_input: patch }),
    { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
  );

  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res, body) => {
      hits.push({ url: req.url, body });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "codexpatch1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 2, "session-end writes the digest AND supersedes the stop: marker");
    const body = JSON.parse(hits[0].body);
    assert.match(body.content, /Edited: src\/auth\.js, src\/session\.js\./);
    assert.deepEqual(body.metadata.files, ["src/auth.js", "src/session.js"]);
  } finally {
    await close();
  }
});

test("session-end.mjs: supersede tolerates a 404 (stop: marker missing)", async () => {
  const cache = freshCache();
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "nfdig1", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "x.go" } }),
    { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
  );

  let supersedeSeen = false;
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      if (req.url === "/v1/memories") {
        res.setHeader("Content-Type", "application/json");
        res.statusCode = 201;
        res.end(JSON.stringify({ id: "session-end:nfdig1" }));
        return;
      }
      if (req.url === "/v1/memories/stop%3Anfdig1/supersede") {
        supersedeSeen = true;
        res.statusCode = 404;
        res.end(JSON.stringify({ error: "memory not found" }));
        return;
      }
      res.statusCode = 500;
      res.end();
    }),
  );
  try {
    const { stderr } = await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "nfdig1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.ok(supersedeSeen, "session-end should still POST the supersede even when the target is missing");
    assert.doesNotMatch(stderr, /UnhandledPromise|Rejection|TypeError/, "404 must not crash the hook");
  } finally {
    await close();
  }
});

test("session-end.mjs: percent-encodes the stop: id in the supersede path", async () => {
  const cache = freshCache();
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "abc-123", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "y.go" } }),
    { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
  );

  const paths = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      paths.push(req.url);
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "ok" }));
    }),
  );
  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "abc-123", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.ok(paths.includes("/v1/memories/stop%3Aabc-123/supersede"), `expected percent-encoded stop id, got ${JSON.stringify(paths)}`);
  } finally {
    await close();
  }
});

test("pre-compact.mjs: distills buffer into an episodic precompact checkpoint", async () => {
  const cache = freshCache();
  for (const [tool, input] of [
    ["Edit", { file_path: "auth.go" }],
    ["Bash", { command: "go build ./..." }],
  ]) {
    await runHook(
      "post-tool-use.mjs",
      JSON.stringify({ session_id: "pc1", cwd: __dirname, tool_name: tool, tool_input: input }),
      { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
    );
  }

  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res, body) => {
      hits.push({ url: req.url, body });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    await runHook(
      "pre-compact.mjs",
      JSON.stringify({ session_id: "pc1", cwd: __dirname, trigger: "auto" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 1, "precompact should write exactly one checkpoint");
    const body = JSON.parse(hits[0].body);
    assert.equal(body.tier, "episodic");
    assert.equal(body.id, "precompact:pc1");
    assert.match(body.content, /Pre-compaction checkpoint/);
    assert.equal(body.metadata.trigger, "auto");
  } finally {
    await close();
  }
});

test("pre-compact.mjs: no buffer → no POST, no crash", async () => {
  const cache = freshCache();
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      hits.push({ url: req.url });
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    await runHook(
      "pre-compact.mjs",
      JSON.stringify({ session_id: "empty", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 0, "no buffer should mean no checkpoint");
  } finally {
    await close();
  }
});

test("stop/session-end/pre-compact: no session id → no server writes", async () => {
  const cache = freshCache();
  const tp = join(cache, "turn.jsonl");
  writeFileSync(
    tp,
    [
      JSON.stringify({ type: "user", message: { role: "user", content: "q" } }),
      JSON.stringify({
        type: "assistant",
        message: { id: "msg_1", content: [{ type: "text", text: 'a\n<memory>\n{"memories":[{"content":"fact"}]}\n</memory>' }] },
      }),
    ].join("\n") + "\n",
  );
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      hits.push({ url: req.url });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    await runHook(
      "post-tool-use.mjs",
      JSON.stringify({ cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "x.go" } }),
      { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
    );
    for (const script of ["stop.mjs", "pre-compact.mjs", "session-end.mjs"]) {
      await runHook(script, JSON.stringify({ cwd: __dirname, transcript_path: tp }), {
        MEMINI_BASE_URL: url,
        XDG_CACHE_HOME: cache,
      });
    }
    assert.equal(hits.length, 0, `identity-less payloads must not write, got ${JSON.stringify(hits)}`);
  } finally {
    await close();
  }
});

// Build a fake Claude Code transcript with `n` real user messages plus noise.
function writeTranscript(path, userCount) {
  const lines = [];
  for (let i = 0; i < userCount; i++) {
    lines.push(JSON.stringify({ type: "user", message: { role: "user", content: `q${i}` } }));
    lines.push(JSON.stringify({ type: "assistant", message: { content: [{ type: "text", text: "a" }] } }));
  }
  lines.push(JSON.stringify({ type: "user", isSidechain: true, message: { content: "side" } }));
  lines.push(JSON.stringify({ type: "user", isMeta: true, message: { content: "meta" } }));
  lines.push(JSON.stringify({ type: "user", message: { content: [{ type: "tool_result", content: "r" }] } }));
  lines.push(JSON.stringify({ type: "user", message: { content: "<command-name>/foo</command-name>" } }));
  writeFileSync(path, lines.join("\n") + "\n");
}

test("stop.mjs: blocks once after the auto-save interval, baselining first", async () => {
  const cache = freshCache();
  const tp = join(cache, "transcript.jsonl");
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (_req, res) => {
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  const env = { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "5" };
  try {
    writeTranscript(tp, 3);
    let { stdout } = await runHook("stop.mjs", JSON.stringify({ session_id: "as1", cwd: __dirname, transcript_path: tp }), env);
    assert.equal(stdout.trim(), "", "first sight should baseline, not block");

    writeTranscript(tp, 6);
    ({ stdout } = await runHook("stop.mjs", JSON.stringify({ session_id: "as1", cwd: __dirname, transcript_path: tp }), env));
    assert.equal(stdout.trim(), "", "below interval should not block");

    writeTranscript(tp, 9);
    ({ stdout } = await runHook("stop.mjs", JSON.stringify({ session_id: "as1", cwd: __dirname, transcript_path: tp }), env));
    const decision = JSON.parse(stdout);
    assert.equal(decision.decision, "block");
    assert.match(decision.reason, /memory_remember/);
  } finally {
    await close();
  }
});

test("stop.mjs: never blocks when stop_hook_active, opted out, or no transcript", async () => {
  const cache = freshCache();
  const tp = join(cache, "t.jsonl");
  writeTranscript(tp, 99);
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (_req, res) => {
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    let { stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "g1", cwd: __dirname, transcript_path: tp, stop_hook_active: true }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "1" },
    );
    assert.equal(stdout.trim(), "", "stop_hook_active must pass through");

    ({ stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "g2", cwd: __dirname, transcript_path: tp }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE: "0", MEMINI_AUTO_SAVE_INTERVAL: "1" },
    ));
    assert.equal(stdout.trim(), "", "MEMINI_AUTO_SAVE=0 must pass through");

    ({ stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "g3", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "1" },
    ));
    assert.equal(stdout.trim(), "", "missing transcript must pass through");
  } finally {
    await close();
  }
});

test("stop.mjs: honors a cached handshake — no live handshake, digest under the cached namespace", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "team/pinned" }));
  // Buffer an event (cache hit → session_digest default on → buffers).
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "ch1", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "a.go" } }),
    { XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL },
  );

  let handshakes = 0;
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.url === "/v1/handshake") {
      handshakes++;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(mkHS({ namespace: "should-not-be-used" })));
      return;
    }
    hits.push({ url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.statusCode = 201;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ id: "m1" }));
  });
  try {
    await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "ch1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(handshakes, 0, "a valid cached handshake must not trigger a live one");
    const digest = hits.find((h) => h.url === "/v1/memories");
    assert.ok(digest, "the stop checkpoint should be written");
    assert.equal(digest.ns, "team/pinned", "the write must target the cached-handshake namespace");
  } finally {
    await close();
  }
});

// ─── pre-tool-use (network-free hot path) ─────────────────────────────────

test("pre-tool-use.mjs: no cached handshake → ZERO network calls, local namespace", async () => {
  const cache = freshCache();
  let hits = 0;
  const { url, close } = await startMockServer((req, res) => {
    hits++;
    res.statusCode = 200;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [] }));
  });
  try {
    const { stdout, stderr } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "nc1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "internal/auth.go" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits, 0, "PreToolUse with no cache must make ZERO network calls");
    assert.equal(stdout, "", "degraded → no recall injected");
    assert.match(stderr, /PreToolUse .*project=memini source=local-/, "resolves a local namespace without the network");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: cache hit → recalls by file path under the handshake namespace", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "srv/app" }));
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { content: "auth decision" }, score: 0.95 }] }));
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "internal/auth.go" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.match(stdout, /<memini-pretool[^>]*>/);
    assert.match(stdout, /auth decision/);
    assert.equal(hits.length, 1);
    assert.equal(hits[0].ns, "srv/app", "recall must target the cached-handshake namespace");
    assert.match(JSON.parse(hits[0].body).query, /Read on internal\/auth\.go/);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: excludes this session's own captures from recall", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { content: "auth decision" }, score: 0.9 }] }));
  });
  try {
    await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "internal/auth.go" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 1);
    assert.deepEqual(JSON.parse(hits[0].body).exclude_metadata, { session_id: "s1" });
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_ITEMS caps items per file", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    const limit = JSON.parse(body || "{}").limit || 5;
    const n = Math.min(limit, 5);
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: Array.from({ length: n }, (_, i) => ({ memory: { content: `hit-${i}` }, score: 0.9 - i * 0.1 })) }));
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "/tmp/foo" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_INJECT_PRETOOL_ITEMS: "2" },
    );
    const ctx = JSON.parse(stdout).hookSpecificOutput.additionalContext;
    assert.match(ctx, /hit-0/);
    assert.match(ctx, /hit-1/);
    assert.doesNotMatch(ctx, /hit-3/);
    assert.equal(JSON.parse(hits[0].body).limit, 2);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_MIN_SCORE drops low-scored hits", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { content: "strong" }, score: 0.9 }, { memory: { content: "weak" }, score: 0.3 }] }));
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "/tmp/foo" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_INJECT_PRETOOL_MIN_SCORE: "0.5" },
    );
    const ctx = JSON.parse(stdout).hookSpecificOutput.additionalContext;
    assert.match(ctx, /strong/);
    assert.doesNotMatch(ctx, /weak/);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_TOOLS skips tools outside the allowlist", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const hits = [];
  const { url, close } = await startMockServer((req, res) => {
    hits.push(req.url);
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { content: "x" }, score: 0.9 }] }));
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname, tool_name: "Bash", tool_input: { command: "ls" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_INJECT_PRETOOL_TOOLS: "Read|Edit" },
    );
    assert.equal(stdout, "", "tool outside allowlist must produce no context");
    assert.equal(hits.length, 0, "tool outside allowlist must not hit memini");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_MAX_TOK truncates per-file block", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: Array.from({ length: 4 }, (_, i) => ({ memory: { content: `payload-${i} payload-${i} payload-${i} payload-${i}` }, score: 0.9 - i * 0.1 })) }));
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "/tmp/foo" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_INJECT_PRETOOL_ITEMS: "4", MEMINI_INJECT_PRETOOL_MAX_TOK: "10" },
    );
    const ctx = JSON.parse(stdout).hookSpecificOutput.additionalContext;
    assert.match(ctx, /\[...\s+\d+ item\(s\) truncated/);
  } finally {
    await close();
  }
});

// ─── PreToolUse: duplicate-injection suppression ──────────────────────────

test("writeLastRecallState/readLastRecallState: bounds to 32 most-recent entries, evicting oldest by `at`", async () => {
  const { readLastRecallState, writeLastRecallState } = await import("./_shared.mjs");
  const cache = freshCache();
  const prevXdg = process.env.XDG_CACHE_HOME;
  process.env.XDG_CACHE_HOME = cache;
  try {
    const state = {};
    for (let i = 0; i < 40; i++) {
      state[`file-${i}`] = { hash: `hash-${i}`, at: i }; // ascending `at` — file-0 is oldest
    }
    writeLastRecallState("bound-test", state);
    const after = readLastRecallState("bound-test");
    const keys = Object.keys(after);
    assert.equal(keys.length, 32, "state must be bounded to 32 entries");
    assert.ok(!("file-0" in after), "the oldest entry (lowest `at`) must be evicted");
    assert.ok("file-39" in after, "the most recent entry must be kept");
  } finally {
    if (prevXdg === undefined) delete process.env.XDG_CACHE_HOME;
    else process.env.XDG_CACHE_HOME = prevXdg;
  }
});

test("pre-tool-use.mjs: identical recall for the same file is suppressed on the second call", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  let calls = 0;
  const { url, close } = await startMockServer((req, res) => {
    calls++;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "auth decision" }, score: 0.95 }] }));
  });
  try {
    const payload = JSON.stringify({
      session_id: "dedupe1",
      cwd: __dirname,
      tool_name: "Read",
      tool_input: { file_path: "internal/auth.go" },
    });
    const first = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(first.stdout, /<memini-pretool[^>]*>/, "first call must inject");
    assert.match(first.stdout, /auth decision/);

    const second = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.equal(second.stdout, "", "second identical call must produce NO injection");
    assert.equal(calls, 2, "the recall call itself must still happen both times");

    const statePath = join(cache, "memini", "sessions", "dedupe1.lastrecall.json");
    assert.equal(existsSync(statePath), true);
    const state = JSON.parse(readFileSync(statePath, "utf8"));
    assert.ok(state["internal/auth.go"]?.hash, "the state file must record the file's fingerprint");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: changed recall results re-inject even for the same file", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  let calls = 0;
  const { url, close } = await startMockServer((req, res) => {
    calls++;
    res.setHeader("Content-Type", "application/json");
    const content = calls === 1 ? "first version of the fact" : "second, updated version of the fact";
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content }, score: 0.9 }] }));
  });
  try {
    const payload = JSON.stringify({
      session_id: "dedupe2",
      cwd: __dirname,
      tool_name: "Read",
      tool_input: { file_path: "internal/auth.go" },
    });
    const first = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(first.stdout, /first version of the fact/);

    const second = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.notEqual(second.stdout, "", "changed results must re-inject");
    assert.match(second.stdout, /second, updated version of the fact/);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: different files with identical result sets both inject (per-file map)", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "shared fact" }, score: 0.9 }] }));
  });
  try {
    const mk = (file) =>
      JSON.stringify({ session_id: "dedupe3", cwd: __dirname, tool_name: "Read", tool_input: { file_path: file } });

    const a = await runHook("pre-tool-use.mjs", mk("a.go"), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(a.stdout, /shared fact/, "file a must inject");

    const b = await runHook("pre-tool-use.mjs", mk("b.go"), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(b.stdout, /shared fact/, "a different file with the identical result set must ALSO inject");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: Read then Edit on the same file with identical results — the second is suppressed (tool-agnostic fingerprint)", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "pipeline behavior" }, score: 0.92 }] }));
  });
  try {
    const readCall = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "dedupe4", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "pipeline.py" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.match(readCall.stdout, /pipeline behavior/, "Read must inject");

    const editCall = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "dedupe4", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "pipeline.py" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(
      editCall.stdout,
      "",
      "Edit on the same file with the identical served memories must be suppressed, even though the tool differs",
    );
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: two different session_ids do not share last-recall state", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "cross-session fact" }, score: 0.9 }] }));
  });
  try {
    const mk = (sid) =>
      JSON.stringify({ session_id: sid, cwd: __dirname, tool_name: "Read", tool_input: { file_path: "shared.go" } });

    const a1 = await runHook("pre-tool-use.mjs", mk("sess-a"), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(a1.stdout, /cross-session fact/);
    const a2 = await runHook("pre-tool-use.mjs", mk("sess-a"), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.equal(a2.stdout, "", "second call in session A must be suppressed");

    const b1 = await runHook("pre-tool-use.mjs", mk("sess-b"), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(b1.stdout, /cross-session fact/, "a fresh session_id must NOT inherit session A's suppression state");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: server inject_dedupe=false disables suppression — duplicates inject again", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini", settings: { inject_dedupe: false } }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "always-inject fact" }, score: 0.9 }] }));
  });
  try {
    const payload = JSON.stringify({
      session_id: "dedupeoff1",
      cwd: __dirname,
      tool_name: "Read",
      tool_input: { file_path: "a.go" },
    });
    const first = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(first.stdout, /always-inject fact/);
    const second = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(second.stdout, /always-inject fact/, "with the server setting off, the identical block must inject again");
    assert.equal(
      existsSync(join(cache, "memini", "sessions", "dedupeoff1.lastrecall.json")),
      false,
      "dedupe off must not touch the state file",
    );
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_DEDUPE=0 overrides a server inject_dedupe=true", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini", settings: { inject_dedupe: true } }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "env-override fact" }, score: 0.9 }] }));
  });
  try {
    const payload = JSON.stringify({
      session_id: "dedupeenv1",
      cwd: __dirname,
      tool_name: "Read",
      tool_input: { file_path: "a.go" },
    });
    const env = { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_INJECT_DEDUPE: "0" };
    const first = await runHook("pre-tool-use.mjs", payload, env);
    assert.match(first.stdout, /env-override fact/);
    const second = await runHook("pre-tool-use.mjs", payload, env);
    assert.match(second.stdout, /env-override fact/, "the env override must beat the server's true and re-inject");
  } finally {
    await close();
  }
});

test("pre-compact.mjs: clears the last-recall state so an identical recall re-injects afterward", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "compaction-surviving fact" }, score: 0.9 }] }));
  });
  try {
    const payload = JSON.stringify({
      session_id: "pcdedupe1",
      cwd: __dirname,
      tool_name: "Read",
      tool_input: { file_path: "auth.go" },
    });
    const first = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(first.stdout, /compaction-surviving fact/, "first call must inject");

    const second = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.equal(second.stdout, "", "sanity check: repeat call is suppressed before compaction");

    await runHook(
      "pre-compact.mjs",
      JSON.stringify({ session_id: "pcdedupe1", cwd: __dirname, trigger: "auto" }),
      { MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: cache },
    );

    const third = await runHook("pre-tool-use.mjs", payload, { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    assert.match(third.stdout, /compaction-surviving fact/, "after pre-compact clears state, the identical recall must re-inject");
  } finally {
    await close();
  }
});

test("session-end.mjs: deletes the last-recall state alongside the other session files", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ results: [{ memory: { id: "m1", content: "session-end fact" }, score: 0.9 }], id: "m1" }));
  });
  try {
    await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({ session_id: "sedelete1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: "a.go" } }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    const statePath = join(cache, "memini", "sessions", "sedelete1.lastrecall.json");
    assert.equal(existsSync(statePath), true, "precondition: state recorded");

    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "sedelete1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(existsSync(statePath), false, "session-end must delete the last-recall state file");
  } finally {
    await close();
  }
});

test("session-start.mjs: deletes the last-recall state for its session at startup", async () => {
  const cache = freshCache();
  const statePath = join(cache, "memini", "sessions", "ssdelete1.lastrecall.json");
  mkdirSync(join(cache, "memini", "sessions"), { recursive: true });
  writeFileSync(statePath, JSON.stringify({ "a.go": { hash: "stale-hash", at: 1 } }));
  assert.equal(existsSync(statePath), true, "precondition: stale state present");

  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "memini", pinned: [], facts: [], procedures: [], recent: [] }));
    }),
  );
  try {
    await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "ssdelete1", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(existsSync(statePath), false, "SessionStart must clear stale last-recall state for a new/resumed session");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: last-recall state is bounded, evicting the oldest entry (33 distinct files)", async () => {
  const cache = freshCache();
  await primeCache(cache, __dirname, mkHS({ namespace: "memini" }));
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { id: "shared", content: "shared memory" }, score: 0.9 }] }));
  });
  try {
    const N = 33;
    for (let i = 0; i < N; i++) {
      await runHook(
        "pre-tool-use.mjs",
        JSON.stringify({ session_id: "evict1", cwd: __dirname, tool_name: "Read", tool_input: { file_path: `/tmp/file-${i}` } }),
        { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
      );
    }
    const statePath = join(cache, "memini", "sessions", "evict1.lastrecall.json");
    assert.equal(existsSync(statePath), true);
    const state = JSON.parse(readFileSync(statePath, "utf8"));
    const keys = Object.keys(state);
    assert.ok(keys.length <= 32, `expected state to stay bounded, got ${keys.length} entries`);
    assert.ok(!("/tmp/file-0" in state), "the oldest entry should have been evicted");
    assert.ok("/tmp/file-32" in state, "the most recent entry should be present");
  } finally {
    await close();
  }
});

// ─── mcp-headers (the MCP headersHelper) ──────────────────────────────────

// Spawn `script` from an intermediate node process whose cwd is `parentCwd`, so
// the script's process.ppid resolves to a parent sitting in that directory —
// reproducing the headersHelper's real situation.
function runHookWithParentCwd(script, parentCwd, env = {}) {
  const base = { ...process.env };
  for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_NAMESPACE", "MEMINI_HOME"]) delete base[k];
  const target = JSON.stringify(resolve(SCRIPTS, script));
  const runner = `
    const { spawn } = require("node:child_process");
    const c = spawn(process.execPath, [${target}], { stdio: ["ignore", "inherit", "inherit"] });
    c.on("close", (code) => process.exit(code ?? 0));
  `;
  return new Promise((resolveProm, reject) => {
    const child = spawn("node", ["-e", runner], {
      cwd: parentCwd,
      env: { ...base, ...env, MEMINI_DEBUG: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (c) => (stdout += c));
    child.stderr.on("data", (c) => (stderr += c));
    child.on("close", (code) => {
      if (code !== 0) {
        reject(new Error(`${script} exited ${code}\nstderr: ${stderr}`));
        return;
      }
      resolveProm({ stdout, stderr });
    });
    child.on("error", reject);
  });
}

test("mcp-headers.mjs: emits cwd-resolved namespace + bearer when token set", async () => {
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_API_KEY: "tok-123",
    MEMINI_BASE_URL: DEAD_URL, // handshake fails fast → local derivation
    XDG_CACHE_HOME: freshCache(),
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "memini", "namespace from local derivation of the repo");
  assert.equal(h.Authorization, "Bearer tok-123");
});

test("mcp-headers.mjs: omits Authorization when no token", async () => {
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_BASE_URL: DEAD_URL,
    XDG_CACHE_HOME: freshCache(),
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "memini");
  assert.equal(h.Authorization, undefined);
});

test("mcp-headers.mjs: emits X-Memini-Home when MEMINI_HOME is set, omits when unset", async () => {
  const set = JSON.parse(
    (await runHook("mcp-headers.mjs", "", { CLAUDE_PROJECT_DIR: __dirname, MEMINI_HOME: "personal/acme", MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: freshCache() })).stdout,
  );
  assert.equal(set["X-Memini-Home"], "personal/acme");
  const unset = JSON.parse(
    (await runHook("mcp-headers.mjs", "", { CLAUDE_PROJECT_DIR: __dirname, MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: freshCache() })).stdout,
  );
  assert.equal(unset["X-Memini-Home"], undefined);
});

test("mcp-headers.mjs: MEMINI_REQUIRE_HTTPS omits the bearer for plaintext non-loopback (broadened truthiness)", async () => {
  for (const flag of ["1", "true"]) {
    const { stdout, stderr } = await runHook("mcp-headers.mjs", "", {
      CLAUDE_PROJECT_DIR: __dirname,
      MEMINI_API_KEY: "tok-123",
      MEMINI_BASE_URL: "http://memini.invalid",
      MEMINI_REQUIRE_HTTPS: flag,
      XDG_CACHE_HOME: freshCache(),
    });
    const h = JSON.parse(stdout);
    assert.equal(h["X-Memini-Namespace"], "memini", `namespace must still be emitted (flag=${flag})`);
    assert.equal(h.Authorization, undefined, `bearer must not travel over plaintext (flag=${flag})`);
    assert.match(stderr, /plaintext HTTP/);
  }
});

test("mcp-headers.mjs: sends the bearer for plaintext non-loopback by default (no warning)", async () => {
  const { stdout, stderr } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_API_KEY: "tok-123",
    MEMINI_BASE_URL: "http://memini.invalid",
    XDG_CACHE_HOME: freshCache(),
  });
  const h = JSON.parse(stdout);
  assert.equal(h.Authorization, "Bearer tok-123");
  assert.doesNotMatch(stderr, /plaintext HTTP/, "the default posture must not warn");
});

test("mcp-headers.mjs: cache hit → the handshake namespace, no live handshake", async () => {
  const cache = freshCache();
  const proj = tmpDir("mcp-cache");
  await primeCache(cache, proj, mkHS({ namespace: "pinned/from-cache" }));
  let handshakes = 0;
  const { url, close } = await startMockServer((req, res) => {
    if (req.url === "/v1/handshake") handshakes++;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify(mkHS({ namespace: "should-not-be-used" })));
  });
  try {
    const { stdout } = await runHook("mcp-headers.mjs", "", {
      CLAUDE_PROJECT_DIR: proj,
      MEMINI_BASE_URL: url,
      XDG_CACHE_HOME: cache,
    });
    const h = JSON.parse(stdout);
    assert.equal(h["X-Memini-Namespace"], "pinned/from-cache");
    assert.equal(handshakes, 0, "a valid cached handshake must not trigger a live one");
  } finally {
    await close();
  }
});

test("mcp-headers.mjs: total failure → env namespace when MEMINI_NAMESPACE is set", async () => {
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_NAMESPACE: "env/pinned",
    MEMINI_BASE_URL: DEAD_URL,
    XDG_CACHE_HOME: freshCache(),
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "env/pinned", "env override wins in local fallback");
});

test("mcp-headers.mjs: resolves the namespace from the parent process's cwd", async () => {
  const proj = tmpDir("proj");
  const { stdout } = await runHookWithParentCwd("mcp-headers.mjs", proj, {
    CLAUDE_PROJECT_DIR: "",
    XDG_CACHE_HOME: freshCache(),
    MEMINI_BASE_URL: DEAD_URL,
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], basename(proj), "namespace must come from the parent's cwd");
});

test("mcp-headers.mjs: two parents in two repos get two namespaces (the race fix)", async () => {
  const a = tmpDir("repo-a");
  const b = tmpDir("repo-b");
  const cache = freshCache(); // deliberately SHARED between the two, as in real life
  const env = { CLAUDE_PROJECT_DIR: "", XDG_CACHE_HOME: cache, MEMINI_BASE_URL: DEAD_URL };
  const [ra, rb] = await Promise.all([
    runHookWithParentCwd("mcp-headers.mjs", a, env),
    runHookWithParentCwd("mcp-headers.mjs", b, env),
  ]);
  assert.equal(JSON.parse(ra.stdout)["X-Memini-Namespace"], basename(a));
  assert.equal(JSON.parse(rb.stdout)["X-Memini-Namespace"], basename(b));
});

test("mcp-headers.mjs: no project signal → auth-only headers (no namespace), server applies key default", async () => {
  const pluginRoot = join(freshCache(), "plugins", "cache", "memini", "memini", "0.6.9");
  mkdirSync(pluginRoot, { recursive: true });
  const { stdout } = await runHookWithParentCwd("mcp-headers.mjs", pluginRoot, {
    CLAUDE_PROJECT_DIR: "",
    CLAUDE_PLUGIN_ROOT: pluginRoot,
    XDG_CACHE_HOME: freshCache(),
    MEMINI_API_KEY: "tok-xyz",
    MEMINI_BASE_URL: DEAD_URL,
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], undefined, "must not emit a version-derived namespace");
  assert.equal(h.Authorization, "Bearer tok-xyz", "auth still flows so the server can apply the key default");
});

test("mcp-headers.mjs: the legacy global namespace file is IGNORED even when present", async () => {
  // Regression for the deleted last-resort. Write the old file at its old path;
  // with no project signal the helper must still emit NO namespace, not read it.
  const cache = freshCache();
  mkdirSync(join(cache, "memini"), { recursive: true });
  writeFileSync(join(cache, "memini", "namespace"), "legacy/should-be-ignored");
  const pluginRoot = join(freshCache(), "plugins", "cache", "memini", "memini", "0.6.9");
  mkdirSync(pluginRoot, { recursive: true });
  const { stdout } = await runHookWithParentCwd("mcp-headers.mjs", pluginRoot, {
    CLAUDE_PROJECT_DIR: "",
    CLAUDE_PLUGIN_ROOT: pluginRoot,
    XDG_CACHE_HOME: cache,
    MEMINI_BASE_URL: DEAD_URL,
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], undefined, "the deleted legacy file must not resurrect a namespace");
});

test("mcp-headers.mjs: the run.sh exec chain preserves the parent (load-bearing)", async () => {
  const proj = tmpDir("exec");
  const runSh = resolve(SCRIPTS, "run.sh");
  const target = resolve(SCRIPTS, "mcp-headers.mjs");
  const base = { ...process.env };
  for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_NAMESPACE", "MEMINI_HOME"]) delete base[k];

  const claudeStandIn = `
    const { spawn } = require("node:child_process");
    const c = spawn("sh", ["-c", 'exec "${runSh}" "${target}"'], {
      cwd: ${JSON.stringify(SCRIPTS)},
      stdio: ["ignore", "inherit", "inherit"],
    });
    c.on("close", (code) => process.exit(code ?? 0));
  `;
  const stdout = await new Promise((res, rej) => {
    const child = spawn("node", ["-e", claudeStandIn], {
      cwd: proj,
      env: { ...base, CLAUDE_PROJECT_DIR: "", XDG_CACHE_HOME: freshCache(), MEMINI_BASE_URL: DEAD_URL },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let out = "";
    child.stdout.on("data", (c) => (out += c));
    child.on("close", (code) => (code === 0 ? res(out) : rej(new Error(`exited ${code}`))));
    child.on("error", rej);
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], basename(proj), "the exec chain must keep `claude` as the parent");
});

test("mcp-headers.mjs: CLAUDE_PROJECT_DIR outranks the process tree", async () => {
  const proj = tmpDir("explicit");
  const other = tmpDir("parent");
  const { stdout } = await runHookWithParentCwd("mcp-headers.mjs", other, {
    CLAUDE_PROJECT_DIR: proj,
    XDG_CACHE_HOME: freshCache(),
    MEMINI_BASE_URL: DEAD_URL,
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], basename(proj));
});

// ─── namespace.mjs (pins) ─────────────────────────────────────────────────

function gitRepo(prefix, remote) {
  const dir = mkdtempSync(join(tmpdir(), `memini-${prefix}-`));
  execSync("git init -q", { cwd: dir });
  if (remote) execSync(`git remote add origin ${remote}`, { cwd: dir });
  return dir;
}

test("namespace.mjs: set PUTs /v1/pins with facts and invalidates caches", async () => {
  const repo = gitRepo("pin-set", "https://github.com/acme/widget.git");
  const cache = freshCache();
  // A stale handshake cache that must be invalidated.
  mkdirSync(join(cache, "memini", "sessions"), { recursive: true });
  const stale = join(cache, "memini", "sessions", "pid-999.handshake.json");
  writeFileSync(stale, JSON.stringify({ result: {}, cwd: repo, factsHash: "x", writtenAt: Date.now() }));

  const puts = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "PUT" && req.url === "/v1/pins") {
      puts.push(JSON.parse(body));
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ key: "remote:github.com/acme/widget", namespace: JSON.parse(body).namespace, created_at: "t", updated_at: "t" }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { code, stdout } = await runCommand("namespace.mjs", ["team/widget"], { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache }, repo);
    assert.equal(code, 0);
    assert.equal(puts.length, 1, "must PUT /v1/pins");
    assert.equal(puts[0].namespace, "team/widget");
    assert.ok(puts[0].remote_url, "must send the git remote as a pin key");
    assert.ok(puts[0].toplevel_path, "must send the toplevel path as a pin key");
    assert.match(stdout, /namespace pinned: team\/widget/);
    assert.equal(existsSync(stale), false, "setting a pin must invalidate cached handshakes");
  } finally {
    await close();
  }
});

test("namespace.mjs: clear DELETEs /v1/pins with facts; 404 reports nothing-to-clear", async () => {
  const repo = gitRepo("pin-clear", "https://github.com/acme/widget.git");
  const cache = freshCache();
  const deletes = [];
  let status = 204;
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "DELETE" && req.url === "/v1/pins") {
      deletes.push(JSON.parse(body));
      res.statusCode = status;
      if (status === 204) res.end();
      else res.end(JSON.stringify({ error: "no pin matches" }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    // Present pin → 204 → cleared.
    let r = await runCommand("namespace.mjs", ["--clear"], { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache }, repo);
    assert.equal(deletes.length, 1);
    assert.ok(deletes[0].remote_url && deletes[0].toplevel_path, "DELETE must carry the pin facts");
    assert.match(r.stdout, /pin cleared/);

    // No pin → 404 → nothing to clear.
    status = 404;
    r = await runCommand("namespace.mjs", ["--clear"], { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache }, repo);
    assert.match(r.stdout, /nothing to clear/i);
  } finally {
    await close();
  }
});

test("namespace.mjs: server unreachable → error pointing at MEMINI_NAMESPACE", async () => {
  const repo = gitRepo("pin-off", "https://github.com/acme/widget.git");
  const { code, stderr } = await runCommand("namespace.mjs", ["team/x"], { MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: freshCache() }, repo);
  assert.equal(code, 1);
  assert.match(stderr, /MEMINI_NAMESPACE/, "offline help must point at the env override");
});

// ─── migration: overrides.json → pins (session-start auto-migrate) ────────
//
// The config-handshake redesign retires ~/.config/memini/overrides.json in
// favor of server-side pins. session-start.mjs auto-migrates a matching
// entry the moment it can prove no pin exists yet (a successful handshake
// reporting a non-"pin" namespace_source) — the pin check itself is what
// makes this idempotent, so there is no separate "already migrated" marker.

// A fresh, isolated XDG_CONFIG_HOME so a test's overrides.json can't leak
// into another test (the global XDG_CONFIG_HOME set at the top of this file
// is shared across the whole run and must stay empty).
function freshConfigHome() {
  return mkdtempSync(join(tmpdir(), "memini-xdgconfig-"));
}

// Seed a legacy overrides.json entry the way the retired writeOverride once
// did (the write path is deleted from the client core; only reads remain, for
// exactly this migration). The key must match what readOverride computes, so
// it comes from the surviving overrideKey export; the file shape is the frozen
// legacy contract: { version: 1, overrides: { <key>: { namespace, setAt } } }.
async function writeOverrideEntry(repo, namespace, configHome) {
  const mod = await import("./_client.gen.mjs");
  const p = overridesJsonPath(configHome);
  let file = { version: 1, overrides: {} };
  try {
    file = JSON.parse(readFileSync(p, "utf8"));
  } catch {
    mkdirSync(dirname(p), { recursive: true });
  }
  file.overrides[mod.overrideKey(repo)] = {
    namespace,
    setAt: new Date().toISOString(),
  };
  writeFileSync(p, JSON.stringify(file, null, 2) + "\n");
}

function overridesJsonPath(configHome) {
  return join(configHome, "memini", "overrides.json");
}

test("session-start.mjs auto-migrate: override present, no pin → PUTs /v1/pins and prints one stderr line", async () => {
  const repo = gitRepo("automigrate-happy", "https://github.com/acme/legacy.git");
  const configHome = freshConfigHome();
  await writeOverrideEntry(repo, "team/legacy-ns", configHome);
  const before = readFileSync(overridesJsonPath(configHome), "utf8");

  const puts = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "POST" && req.url === "/v1/handshake") {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(mkHS({ namespace: "server/derived", namespace_source: "remote" })));
      return;
    }
    if (req.method === "PUT" && req.url === "/v1/pins") {
      puts.push(JSON.parse(body));
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ key: "path:" + repo, namespace: JSON.parse(body).namespace, created_at: "t", updated_at: "t" }));
      return;
    }
    if (req.url.startsWith("/v1/namespaces/") && req.url.includes("/briefing")) {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "server/derived", pinned: [], facts: [], procedures: [], recent: [] }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { stderr } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "am1", cwd: repo }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), XDG_CONFIG_HOME: configHome },
    );
    assert.equal(puts.length, 1, "must PUT /v1/pins exactly once");
    assert.equal(puts[0].namespace, "team/legacy-ns", "must migrate the override's namespace value");
    assert.ok(puts[0].toplevel_path, "must carry the project's toplevel_path fact");
    assert.equal(puts[0].remote_url, "https://github.com/acme/legacy.git");
    assert.equal(puts[0].note, "migrated from overrides.json");
    assert.match(
      stderr,
      /\[memini\] migrated your local namespace override for this project to a server pin/,
    );
    const after = readFileSync(overridesJsonPath(configHome), "utf8");
    assert.equal(after, before, "overrides.json must never be written by the auto-migrate path");
  } finally {
    await close();
  }
});

test("session-start.mjs auto-migrate: a pin already present → no PUT (idempotent)", async () => {
  const repo = gitRepo("automigrate-idempotent", "https://github.com/acme/legacy.git");
  const configHome = freshConfigHome();
  await writeOverrideEntry(repo, "team/legacy-ns", configHome);

  const puts = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "POST" && req.url === "/v1/handshake") {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify(
          mkHS({
            namespace: "team/legacy-ns",
            namespace_source: "pin",
            pin: { key: "path:" + repo, updated_at: "2026-07-01T00:00:00Z" },
          }),
        ),
      );
      return;
    }
    if (req.method === "PUT" && req.url === "/v1/pins") {
      puts.push(JSON.parse(body));
      res.statusCode = 200;
      res.end(JSON.stringify({ key: "path:" + repo, namespace: "team/legacy-ns", created_at: "t", updated_at: "t" }));
      return;
    }
    if (req.url.startsWith("/v1/namespaces/") && req.url.includes("/briefing")) {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "team/legacy-ns", pinned: [], facts: [], procedures: [], recent: [] }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { stderr } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "am2", cwd: repo }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), XDG_CONFIG_HOME: configHome },
    );
    assert.equal(puts.length, 0, "a pin already resolved by the handshake must not be re-migrated");
    assert.doesNotMatch(stderr, /migrated your local namespace override/);
  } finally {
    await close();
  }
});

test("session-start.mjs auto-migrate: PUT failure is fail-soft — session continues, overrides.json untouched", async () => {
  const repo = gitRepo("automigrate-failsoft", "https://github.com/acme/legacy.git");
  const configHome = freshConfigHome();
  await writeOverrideEntry(repo, "team/legacy-ns", configHome);
  const before = readFileSync(overridesJsonPath(configHome), "utf8");

  const { url, close } = await startMockServer((req, res) => {
    if (req.method === "POST" && req.url === "/v1/handshake") {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(mkHS({ namespace: "server/derived", namespace_source: "remote" })));
      return;
    }
    if (req.method === "PUT" && req.url === "/v1/pins") {
      res.statusCode = 500;
      res.end(JSON.stringify({ error: "boom" }));
      return;
    }
    if (req.url.startsWith("/v1/namespaces/") && req.url.includes("/briefing")) {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "server/derived", pinned: [], facts: [{ content: "still works" }], procedures: [], recent: [] }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { stdout, stderr } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "am3", cwd: repo }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), XDG_CONFIG_HOME: configHome },
    );
    assert.match(stdout, /still works/, "the session must continue normally despite the failed migration");
    assert.doesNotMatch(stderr, /UnhandledPromise|Rejection/, "a failed PUT must not crash the hook");
    const after = readFileSync(overridesJsonPath(configHome), "utf8");
    assert.equal(after, before, "overrides.json must never be written, even on migration failure");
  } finally {
    await close();
  }
});

test("session-start.mjs auto-migrate: the migrating session runs on the pin's namespace, not the derived one", async () => {
  // The bug this guards: the live handshake + per-session cache are written
  // BEFORE migrateOverrideToPin creates the pin, so a naive flow briefs and
  // captures under the DERIVED namespace while the freshly-migrated pin names
  // another — "writes land where recall doesn't look," for exactly one session,
  // at the moment users upgrade. After a successful migration the hook must
  // re-handshake so the WHOLE session (the briefing here, and any capture that
  // reads the per-session cache) runs on the pin's namespace.
  const repo = gitRepo("automigrate-same-session", "https://github.com/acme/legacy.git");
  const configHome = freshConfigHome();
  const cache = freshCache();
  await writeOverrideEntry(repo, "team/legacy-ns", configHome);

  let pinned = null; // set once the /v1/pins PUT lands; the handshake then resolves to it
  const briefingNs = [];
  const memoryNs = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "POST" && req.url === "/v1/handshake") {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify(
          pinned
            ? mkHS({ namespace: pinned, namespace_source: "pin", pin: { key: "path:" + repo } })
            : mkHS({ namespace: "server/derived", namespace_source: "remote" }),
        ),
      );
      return;
    }
    if (req.method === "PUT" && req.url === "/v1/pins") {
      pinned = JSON.parse(body).namespace;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ key: "path:" + repo, namespace: pinned, created_at: "t", updated_at: "t" }));
      return;
    }
    if (req.url.startsWith("/v1/namespaces/") && req.url.includes("/briefing")) {
      briefingNs.push(req.headers["x-memini-namespace"]);
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          namespace: req.headers["x-memini-namespace"],
          pinned: [],
          facts: [{ content: "hello" }],
          procedures: [],
          recent: [],
        }),
      );
      return;
    }
    if (req.method === "POST" && req.url === "/v1/memories") {
      memoryNs.push(req.headers["x-memini-namespace"]);
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ id: "m1" }));
      return;
    }
    if (req.url.includes("/supersede")) {
      res.statusCode = 200;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ id: "ok" }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    // Buffer a tool event so session-end has a digest to write this session.
    await runHook(
      "post-tool-use.mjs",
      JSON.stringify({ session_id: "am-same", cwd: repo, tool_name: "Edit", tool_input: { file_path: "auth.go" } }),
      { MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: cache },
    );

    // SessionStart migrates the override, re-handshakes, and briefs on the pin.
    await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "am-same", cwd: repo }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, XDG_CONFIG_HOME: configHome },
    );
    assert.deepEqual(
      briefingNs,
      ["team/legacy-ns"],
      `briefing must target the pin's namespace, got ${JSON.stringify(briefingNs)}`,
    );

    // A capture in the SAME session reads the per-session cache SessionStart
    // rewrote after migrating; it too must land on the pin's namespace.
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "am-same", cwd: repo, reason: "user_exit" }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, XDG_CONFIG_HOME: configHome },
    );
    assert.ok(memoryNs.length >= 1, "session-end must write a digest");
    assert.equal(
      memoryNs[0],
      "team/legacy-ns",
      `the digest must target the pin's namespace, got ${memoryNs[0]}`,
    );
  } finally {
    await close();
  }
});

// ─── removed-var warnings (session-start only) ────────────────────────────

test("session-start.mjs: removed env vars are warned once, combined, and otherwise ignored", async () => {
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "memini", pinned: [], facts: [], procedures: [], recent: [] }));
    }),
  );
  try {
    const { stderr } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "rv1", cwd: __dirname }),
      {
        MEMINI_BASE_URL: url,
        XDG_CACHE_HOME: freshCache(),
        MEMINI_URL: "http://old.example",
        MEMINI_NAMESPACE_SCOPE: "owner_repo",
      },
    );
    assert.match(
      stderr,
      /\[memini\] ignored removed env vars: MEMINI_URL, MEMINI_NAMESPACE_SCOPE \(see docs\/reference\/env-vars\.md\)/,
    );
    const lines = stderr.split("\n").filter((l) => l.includes("ignored removed env vars"));
    assert.equal(lines.length, 1, "must print exactly one combined line, not one per var");
  } finally {
    await close();
  }
});

test("session-start.mjs: no removed vars set → no warning at all", async () => {
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res) => {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ namespace: "memini", pinned: [], facts: [], procedures: [], recent: [] }));
    }),
  );
  try {
    const { stderr } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "rv2", cwd: __dirname }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache() },
    );
    assert.doesNotMatch(stderr, /ignored removed env vars/);
  } finally {
    await close();
  }
});

// ─── namespace.mjs --migrate (bulk) ────────────────────────────────────────

test("namespace.mjs --migrate: migrates every entry, prints a table, renames the file on full success", async () => {
  const repoA = gitRepo("bulk-a");
  const repoB = gitRepo("bulk-b");
  const configHome = freshConfigHome();
  await writeOverrideEntry(repoA, "team/a-ns", configHome);
  await writeOverrideEntry(repoB, "team/b-ns", configHome);

  const puts = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "GET" && req.url === "/v1/pins") {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ entries: [] }));
      return;
    }
    if (req.method === "PUT" && req.url === "/v1/pins") {
      const parsed = JSON.parse(body);
      puts.push(parsed);
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ key: "path:" + parsed.toplevel_path, namespace: parsed.namespace, created_at: "t", updated_at: "t" }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { code, stdout } = await runCommand("namespace.mjs", ["--migrate"], { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), XDG_CONFIG_HOME: configHome }, repoA);
    assert.equal(code, 0);
    assert.equal(puts.length, 2, "must PUT one pin per overrides.json entry");
    assert.ok(puts.every((p) => p.toplevel_path && !p.remote_url), "bulk migration keys purely off the stored path, not a re-derived git remote");
    assert.match(stdout, new RegExp(`${repoA.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}.*team/a-ns.*migrated`));
    assert.match(stdout, new RegExp(`${repoB.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}.*team/b-ns.*migrated`));
    assert.match(stdout, /renamed overrides\.json/i);
    assert.equal(existsSync(overridesJsonPath(configHome)), false, "the original file must be renamed away");
    assert.equal(existsSync(overridesJsonPath(configHome) + ".migrated"), true);
  } finally {
    await close();
  }
});

test("namespace.mjs --migrate: an already-pinned entry is skipped; a failed entry keeps the file in place", async () => {
  const repoA = gitRepo("bulk-pinned");
  const repoB = gitRepo("bulk-failed");
  const configHome = freshConfigHome();
  await writeOverrideEntry(repoA, "team/a-ns", configHome);
  await writeOverrideEntry(repoB, "team/b-ns", configHome);

  const puts = [];
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.method === "GET" && req.url === "/v1/pins") {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ entries: [{ key: "path:" + repoA, namespace: "team/a-ns", created_at: "t", updated_at: "t" }] }));
      return;
    }
    if (req.method === "PUT" && req.url === "/v1/pins") {
      puts.push(JSON.parse(body));
      res.statusCode = 500;
      res.end(JSON.stringify({ error: "db unavailable" }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { stdout } = await runCommand("namespace.mjs", ["--migrate"], { MEMINI_BASE_URL: url, XDG_CACHE_HOME: freshCache(), XDG_CONFIG_HOME: configHome }, repoA);
    assert.equal(puts.length, 1, "an already-pinned entry must not be re-PUT");
    assert.match(stdout, /already-pinned/);
    assert.match(stdout, /failed/);
    assert.equal(existsSync(overridesJsonPath(configHome)), true, "a partial failure must leave overrides.json in place for a retry");
    assert.equal(existsSync(overridesJsonPath(configHome) + ".migrated"), false);
  } finally {
    await close();
  }
});

test("namespace.mjs --migrate: surfaces config.json tenantRoots/template with manual-recreation instructions", async () => {
  const configHome = freshConfigHome();
  mkdirSync(join(configHome, "memini"), { recursive: true });
  const configPath = join(configHome, "memini", "config.json");
  const configBody = {
    tenantRoots: [{ path: "~/dev/work", tenant: "work" }],
    template: "{tenant}/{project}/{agent}",
  };
  writeFileSync(configPath, JSON.stringify(configBody));

  const { code, stdout } = await runCommand("namespace.mjs", ["--migrate"], { XDG_CACHE_HOME: freshCache(), XDG_CONFIG_HOME: configHome }, __dirname);
  assert.equal(code, 0, "config.json inspection needs no server round trip when overrides.json is empty");
  assert.match(stdout, /tenantRoots/);
  assert.match(stdout, /template/);
  assert.match(stdout, /namespace_prefix/i);
  assert.match(stdout, /cannot/i);
  // Read-only: config.json itself is never rewritten.
  assert.equal(JSON.parse(readFileSync(configPath, "utf8")).template, configBody.template);
});

// ─── status.mjs ───────────────────────────────────────────────────────────

test("status.mjs --json: setting sources (env-override / server key|global / default) and pin", async () => {
  const cache = freshCache();
  const { url, close } = await startMockServer((req, res) => {
    if (req.url === "/v1/handshake") {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify(
          mkHS({
            namespace: "proj",
            namespace_source: "pin",
            pin: { key: "remote:github.com/acme/proj", created_by: "alice", updated_at: "2026-07-13T00:00:00Z" },
            settings: { session_digest: false, inject_briefing_facts: 2 },
            settings_sources: { session_digest: "key", inject_briefing_facts: "global" },
          }),
        ),
      );
      return;
    }
    if (req.url.startsWith("/v1/namespaces/readset")) {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ entries: [{ namespace: "proj", origin: "primary" }] }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });
  try {
    const { stdout } = await runCommand(
      "status.mjs",
      ["--json"],
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, CLAUDE_PROJECT_DIR: __dirname, MEMINI_CAPTURE_TURNS: "0" },
      __dirname,
    );
    const r = JSON.parse(stdout);
    assert.equal(r.degraded, false);
    assert.equal(r.ns.effective, "proj");
    assert.ok(r.ns.pin, "pin provenance present");
    assert.equal(r.ns.pin.created_by, "alice");
    const byKey = Object.fromEntries(r.settings.map((s) => [s.wireKey, s]));
    assert.equal(byKey.session_digest.source, "server");
    assert.equal(byKey.session_digest.serverSource, "key");
    assert.equal(byKey.inject_briefing_facts.value, 2);
    assert.equal(byKey.inject_briefing_facts.serverSource, "global");
    assert.equal(byKey.capture_turns.source, "env-override");
    assert.equal(byKey.capture_turns.value, false);
    assert.equal(byKey.auto_save.source, "default");
  } finally {
    await close();
  }
});

test("status.mjs --json: degraded flag set when the server is unreachable", async () => {
  const { stdout } = await runCommand(
    "status.mjs",
    ["--json"],
    { MEMINI_BASE_URL: DEAD_URL, XDG_CACHE_HOME: freshCache(), CLAUDE_PROJECT_DIR: __dirname },
    __dirname,
  );
  const r = JSON.parse(stdout);
  assert.equal(r.degraded, true);
  assert.equal(r.server.reachable, false);
  assert.match(r.ns.source, /^local-/);
  // Every setting is a built-in default (or an env override); none from the server.
  assert.ok(r.settings.every((s) => s.source !== "server"), "no server-sourced settings when degraded");
  assert.ok(r.warnings.some((w) => w.code === "degraded-mode"), "degraded-mode warning present");
});

// ─── pure helpers (buffers / digest / transcript / env parsing) ───────────

test("intEnv/floatEnv/listEnv/labelsEnv parse env vars defensively", async () => {
  const { intEnv, floatEnv, listEnv, labelsEnv } = await import("./_shared.mjs");
  const prev = { ...process.env };
  try {
    process.env["T"] = "5";
    assert.equal(intEnv("T", 0), 5);
    process.env["T"] = "0";
    assert.equal(intEnv("T", 7), 0, "0 is allowed (cap = 0 disables a section)");
    process.env["T"] = "-1";
    assert.equal(intEnv("T", 7), 7, "negative falls back to default");
    process.env["T"] = "abc";
    assert.equal(intEnv("T", 7), 7);
    delete process.env["T"];
    assert.equal(intEnv("T", 7), 7);

    process.env["F"] = "0.65";
    assert.equal(floatEnv("F", 0), 0.65);
    process.env["F"] = "-1";
    assert.equal(floatEnv("F", 0.5), 0.5);
    delete process.env["F"];
    assert.equal(floatEnv("F", 0.5), 0.5);

    process.env["L"] = "Read|Edit, Write ";
    assert.deepEqual(listEnv("L"), ["read", "edit", "write"]);
    delete process.env["L"];
    assert.deepEqual(listEnv("L"), []);

    process.env["MEMINI_INJECT_LABELS"] = "tier,reason";
    const labels = labelsEnv();
    assert.equal(labels.has("tier"), true);
    assert.equal(labels.has("reason"), true);
    assert.equal(labels.has("age"), false);
    delete process.env["MEMINI_INJECT_LABELS"];
  } finally {
    process.env = prev;
  }
});

test("approxTokens / fitByTokens trim from the tail under a token budget", async () => {
  const { approxTokens, fitByTokens } = await import("./_shared.mjs");
  const long = Array.from({ length: 60 }, (_, i) => `w${i}`).join(" ");
  assert.equal(approxTokens(long), Math.ceil((60 * 4) / 3));

  const items = ["one", "two three", "four five six seven eight"];
  const unlimited = fitByTokens(items, 0);
  assert.deepEqual(unlimited.items, items);
  assert.equal(unlimited.dropped, 0);

  const tight = fitByTokens(items, approxTokens("one"));
  assert.deepEqual(tight.items, ["one"]);
  assert.equal(tight.dropped, 2);
});

test("envEnabled: default-on unless explicitly opted out", async () => {
  const { envEnabled } = await import("./_shared.mjs");
  assert.equal(envEnabled("X", true, {}), true);
  assert.equal(envEnabled("X", true, { X: "" }), true);
  assert.equal(envEnabled("X", true, { X: "0" }), false);
  assert.equal(envEnabled("X", true, { X: "false" }), false);
  assert.equal(envEnabled("X", true, { X: "OFF" }), false);
  assert.equal(envEnabled("X", true, { X: "1" }), true);
  assert.equal(envEnabled("X", false, {}), false);
  assert.equal(envEnabled("X", false, { X: "yes" }), true);
});

test("parseMemoryBlocks: extracts contents, tolerates malformed and empty blocks", async () => {
  const { parseMemoryBlocks } = await import("./_shared.mjs");
  const text = `
    <memory>{"memories":[{"content":"a"},{"content":"b"}]}</memory>
    <memory>not json</memory>
    <memory>{"memories":[]}</memory>
    <memory>{"memories":[{"content":"  "},{"content":"c"}]}</memory>
  `;
  assert.deepEqual(parseMemoryBlocks(text).map((m) => m.content), ["a", "b", "c"]);
  assert.deepEqual(parseMemoryBlocks(""), []);
  assert.deepEqual(parseMemoryBlocks(null), []);
});

test("MEMORY_INSTRUCTION: directs to memory_remember, contains no memory markup", async () => {
  const { MEMORY_INSTRUCTION } = await import("./_shared.mjs");
  assert.match(MEMORY_INSTRUCTION, /memory_remember/);
  assert.ok(!MEMORY_INSTRUCTION.includes("<memory>"), "must not teach the legacy inline markup");
});

test("extractAssistantText: pulls text blocks from a transcript, skips tool-only turns", async () => {
  const { extractAssistantText } = await import("./_shared.mjs");
  const transcript = [
    JSON.stringify({ type: "assistant", message: { content: [{ type: "text", text: "hello" }] } }),
    JSON.stringify({ type: "assistant", message: { content: [{ type: "tool_use", name: "x" }] } }),
    JSON.stringify({ type: "user", message: { content: "ignored" } }),
    JSON.stringify({ type: "assistant", message: { content: "plain string" } }),
  ].join("\n");
  assert.deepEqual(extractAssistantText(transcript), ["hello", "plain string"]);
});

test("isRealUserMessage: strings pass, tool_result arrays and command noise skip", async () => {
  const { isRealUserMessage } = await import("./_shared.mjs");
  assert.equal(isRealUserMessage("a real question"), true);
  assert.equal(isRealUserMessage([{ type: "tool_result" }]), false);
  assert.equal(isRealUserMessage("<command-name>/foo</command-name>"), false);
  assert.equal(isRealUserMessage("<local-command-stdout>"), false);
  assert.equal(isRealUserMessage("<memini-context>...</memini-context>"), false);
  assert.equal(isRealUserMessage("[SYSTEM NOTIFICATION] bg task"), false);
});

test("extractLastTurn: returns the final user→assistant turn, skips noise", async () => {
  const { extractLastTurn } = await import("./_shared.mjs");
  const transcript = [
    JSON.stringify({ type: "user", message: { content: "first" } }),
    JSON.stringify({ type: "assistant", message: { id: "a1", content: [{ type: "text", text: "reply one" }] } }),
    JSON.stringify({ type: "user", message: { content: "<command-name>/x</command-name>" } }),
    JSON.stringify({ type: "user", message: { content: "second" } }),
    JSON.stringify({ type: "assistant", message: { id: "a2", content: [{ type: "text", text: "reply two" }] } }),
  ].join("\n");
  const t = extractLastTurn(transcript);
  assert.equal(t.userText, "second");
  assert.equal(t.assistantText, "reply two");
  assert.equal(t.assistantId, "a2");
});

test("stop.mjs: captures the last turn as episodic by default, dedupes, opts out", async () => {
  const cache = freshCache();
  const tp = join(cache, "turn.jsonl");
  writeFileSync(
    tp,
    [
      JSON.stringify({ type: "user", message: { role: "user", content: "how do I run tests?" } }),
      JSON.stringify({ type: "assistant", message: { id: "msg_9", content: [{ type: "text", text: "run mise test" }] } }),
    ].join("\n") + "\n",
  );
  const bodies = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res, body) => {
      if (req.url === "/v1/memories") bodies.push(JSON.parse(body));
      res.statusCode = 201;
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  try {
    // Default: capture the turn once.
    await runHook("stop.mjs", JSON.stringify({ session_id: "turn1", cwd: __dirname, transcript_path: tp }), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    // Second Stop on the same turn: dedup, no new capture.
    await runHook("stop.mjs", JSON.stringify({ session_id: "turn1", cwd: __dirname, transcript_path: tp }), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache });
    const turns = bodies.filter((b) => b?.metadata?.format === "turn");
    assert.equal(turns.length, 1, "the same turn must be captured at most once");
    assert.equal(turns[0].tier, "episodic");
    assert.match(turns[0].content, /how do I run tests/);

    // Opt out: no capture.
    bodies.length = 0;
    const tp2 = join(cache, "turn2.jsonl");
    writeFileSync(tp2, [
      JSON.stringify({ type: "user", message: { role: "user", content: "another q" } }),
      JSON.stringify({ type: "assistant", message: { id: "msg_10", content: [{ type: "text", text: "an answer" }] } }),
    ].join("\n") + "\n");
    await runHook("stop.mjs", JSON.stringify({ session_id: "turn2", cwd: __dirname, transcript_path: tp2 }), { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_CAPTURE_TURNS: "0" });
    assert.equal(bodies.filter((b) => b?.metadata?.format === "turn").length, 0, "MEMINI_CAPTURE_TURNS=0 must skip capture");
  } finally {
    await close();
  }
});

test("stop.mjs: turn-capture dedup survives an auto-save nudge (save-state co-tenancy)", async () => {
  // captureTurn and autoSaveReasonFor share one save-state file per session.
  // autoSaveReasonFor's writeSaveState({ ...state, lastSavedCount }) spread is
  // what keeps captureTurn's lastCapturedTurn (written EARLIER in the same Stop
  // run) alive across the nudge — drop the spread and every nudge would
  // silently re-capture the same turn. This is that regression's guard.
  const cache = freshCache();
  const tp = join(cache, "cot.jsonl");
  // Build a transcript with `userCount` turns whose final assistant message has
  // a specific id + text (the tail extractLastTurn captures).
  const writeTail = (userCount, tailId, tailText) => {
    const lines = [];
    for (let i = 0; i < userCount; i++) {
      const last = i === userCount - 1;
      lines.push(JSON.stringify({ type: "user", message: { role: "user", content: `q${i}` } }));
      lines.push(
        JSON.stringify({
          type: "assistant",
          message: { id: last ? tailId : `m${i}`, content: [{ type: "text", text: last ? tailText : `a${i}` }] },
        }),
      );
    }
    writeFileSync(tp, lines.join("\n") + "\n");
  };
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res, body) => {
      hits.push({ body });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  const captures = (txt) =>
    hits.filter((h) => {
      try {
        const b = JSON.parse(h.body);
        return b?.metadata?.source === "turn_capture" && b.content.includes(txt);
      } catch {
        return false;
      }
    });
  const env = { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "2" };
  const payload = () => JSON.stringify({ session_id: "cot", cwd: __dirname, transcript_path: tp });
  try {
    // Run 0: 2 turns, tail msgA. First sight baselines auto-save (no block) and captures msgA.
    writeTail(2, "msgA", "answer A");
    let { stdout } = await runHook("stop.mjs", payload(), env);
    assert.equal(stdout.trim(), "", "first sight baselines, no block");
    assert.equal(captures("answer A").length, 1, "captured the first turn");

    // Run 1: 4 turns, tail msgB. Crosses the interval → captures msgB, then nudges.
    writeTail(4, "msgB", "answer B");
    ({ stdout } = await runHook("stop.mjs", payload(), env));
    assert.equal(JSON.parse(stdout).decision, "block", "should nudge once past the interval");
    assert.equal(captures("answer B").length, 1, "captured the second turn before the nudge");

    // Run 2: same tail msgB. The nudge's save-state write must NOT have clobbered
    // lastCapturedTurn, so msgB dedupes here (autoSaveReasonFor's spread).
    await runHook("stop.mjs", payload(), env);
    assert.equal(captures("answer B").length, 1, "msgB must not be re-captured after the nudge");
  } finally {
    await close();
  }
});

test("stop.mjs: turn-capture skips on stop_hook_active and missing transcript", async () => {
  // The other stop_hook_active test only covers the auto-save block path; this
  // one pins captureTurn's OWN guards (a save cycle is not a real user turn;
  // no transcript means nothing to capture, no crash).
  const cache = freshCache();
  const tp = join(cache, "sk.jsonl");
  writeFileSync(
    tp,
    [
      JSON.stringify({ type: "user", message: { role: "user", content: "hi" } }),
      JSON.stringify({ type: "assistant", message: { id: "msgZ", content: [{ type: "text", text: "yo" }] } }),
    ].join("\n") + "\n",
  );
  const hits = [];
  const { url, close } = await startMockServer(
    withHandshake(mkHS({ namespace: "memini" }), (req, res, body) => {
      hits.push({ body });
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "m1" }));
    }),
  );
  const turnPosts = () =>
    hits.filter((h) => {
      try {
        return JSON.parse(h.body)?.metadata?.source === "turn_capture";
      } catch {
        return false;
      }
    });
  try {
    // stop_hook_active = save cycle, not a real user turn → no capture.
    await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "sk1", cwd: __dirname, transcript_path: tp, stop_hook_active: true }),
      { MEMINI_BASE_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(turnPosts().length, 0, "stop_hook_active must not capture");
    // No transcript path → nothing to capture, no crash.
    await runHook("stop.mjs", JSON.stringify({ session_id: "sk2", cwd: __dirname }), {
      MEMINI_BASE_URL: url,
      XDG_CACHE_HOME: cache,
    });
    assert.equal(turnPosts().length, 0, "missing transcript must not capture");
  } finally {
    await close();
  }
});

test("buildSessionDigest: marks a failed command, leaves the recovery command unmarked", async () => {
  const { buildSessionDigest } = await import("./_shared.mjs");
  const d = buildSessionDigest(
    [
      { tool: "Bash", cmd: "protoc --go_out=.", failed: true },
      { tool: "Bash", cmd: "./bin/protoc --go_out=." },
    ],
    "proj",
  );
  assert.match(d.content, /protoc --go_out=\. \(failed\)/);
  assert.ok(!d.content.includes("./bin/protoc --go_out=. (failed)"), "the recovery command must not be marked failed");
});

test("buildSessionDigest: a command that fails then passes on retry is not marked failed", async () => {
  const { buildSessionDigest } = await import("./_shared.mjs");
  const d = buildSessionDigest(
    [
      { tool: "Bash", cmd: "go test ./...", failed: true },
      { tool: "Bash", cmd: "go test ./..." },
    ],
    "proj",
  );
  assert.ok(!d.content.includes("(failed)"), "a retried-and-passed command should not read as failed");
});
