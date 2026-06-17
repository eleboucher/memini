// Test harness for the memini plugin's hook scripts.
//
// Pure Node (no test framework). Run with: node plugin/scripts/_test.mjs
//
// Strategy:
//   1. Spin up a tiny in-process mock memini server.
//   2. Drive each hook script by piping a fake agent payload into its stdin.
//   3. Assert the hook hits the mock with the right namespace + payload, and
//      produces the right stdout (for hooks that inject context).
//
// No external network, no real embeddings. CI-friendly.

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve, basename } from "node:path";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import http from "node:http";

const __dirname = dirname(fileURLToPath(import.meta.url));
const SCRIPTS = __dirname;

// Each test that touches the session buffer gets an isolated cache dir so runs
// don't pollute the real ~/.cache or each other.
function freshCache() {
  return mkdtempSync(join(tmpdir(), "memini-test-"));
}

function runHook(script, payload, env = {}) {
  return new Promise((resolveProm, reject) => {
    const child = spawn("node", [resolve(SCRIPTS, script)], {
      env: { ...process.env, ...env, MEMINI_DEBUG: "1" },
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

test("resolveProject uses git remote origin repo name over toplevel basename", async () => {
  const { resolveProject } = await import("./_shared.mjs");
  const proj = resolveProject(__dirname);
  assert.equal(proj, "memini");
});

test("resolveProject: a /tmp clone of a real repo resolves to that repo's name", async () => {
  const { execSync } = await import("node:child_process");
  const { mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const dir = mkdtempSync(join(tmpdir(), "memini-test-"));
  const bareDir = mkdtempSync(join(tmpdir(), "memini-bare-"));
  try {
    execSync("git init -q --bare", { cwd: bareDir });
    execSync("git init -q", { cwd: dir });
    execSync(`git remote add origin file://${bareDir}/my-cool-repo.git`, { cwd: dir });
    const { resolveProject } = await import("./_shared.mjs?cb=" + Date.now());
    assert.equal(resolveProject(dir), "my-cool-repo");
  } finally {
    rmSync(dir, { recursive: true, force: true });
    rmSync(bareDir, { recursive: true, force: true });
  }
});

test("resolveProject: falls back to toplevel basename when no origin remote", async () => {
  const { execSync } = await import("node:child_process");
  const { mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const dir = mkdtempSync(join(tmpdir(), "memini-test-"));
  try {
    execSync("git init -q", { cwd: dir });
    const { resolveProject } = await import("./_shared.mjs?cb=" + Date.now());
    assert.equal(resolveProject(dir), basename(dir));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("resolveProject: falls back to cwd basename for non-git dirs", async () => {
  const { mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const dir = mkdtempSync(join(tmpdir(), "memini-test-"));
  try {
    const { resolveProject } = await import("./_shared.mjs?cb=" + Date.now());
    assert.equal(resolveProject(dir), basename(dir));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("resolveProject respects MEMINI_NAMESPACE override", async () => {
  const prev = process.env.MEMINI_NAMESPACE;
  process.env.MEMINI_NAMESPACE = "forced-ns";
  try {
    const { resolveProject } = await import("./_shared.mjs");
    assert.equal(resolveProject("/tmp/whatever"), "forced-ns");
  } finally {
    if (prev === undefined) delete process.env.MEMINI_NAMESPACE;
    else process.env.MEMINI_NAMESPACE = prev;
  }
});

test("repoNameFromRemote parses common git URL shapes", async () => {
  const { repoNameFromRemote } = await import("./_shared.mjs");
  assert.equal(repoNameFromRemote("git@github.com:user/repo.git"), "repo");
  assert.equal(repoNameFromRemote("https://github.com/user/repo.git"), "repo");
  assert.equal(repoNameFromRemote("https://github.com/user/repo"), "repo");
  assert.equal(repoNameFromRemote("ssh://git@host:2222/path/to/repo.git"), "repo");
  assert.equal(repoNameFromRemote("ssh://git@host:2222/path/to/repo"), "repo");
  assert.equal(repoNameFromRemote("repo.git"), "repo");
  assert.equal(repoNameFromRemote(""), null);
  assert.equal(repoNameFromRemote(null), null);
  assert.equal(repoNameFromRemote("https://github.com/user/multi-level/nested.git"), "nested");
});

test("repoSlugFromRemote builds an owner-repo slug", async () => {
  const { repoSlugFromRemote } = await import("./_shared.mjs");
  assert.equal(repoSlugFromRemote("git@github.com:alice/app.git"), "alice-app");
  assert.equal(repoSlugFromRemote("https://github.com/bob/app"), "bob-app");
  assert.equal(repoSlugFromRemote("ssh://git@host:2222/team/svc.git"), "team-svc");
  assert.equal(repoSlugFromRemote("app.git"), "app"); // single segment -> bare name
  assert.equal(repoSlugFromRemote(""), null);
});

test("resolveProject: MEMINI_NAMESPACE_SCOPE=owner-repo disambiguates by owner", async () => {
  const { execSync } = await import("node:child_process");
  const { mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const dir = mkdtempSync(join(tmpdir(), "memini-test-"));
  const prevScope = process.env["MEMINI_NAMESPACE_SCOPE"];
  const prevCache = process.env["XDG_CACHE_HOME"];
  process.env["XDG_CACHE_HOME"] = mkdtempSync(join(tmpdir(), "memini-cache-"));
  process.env["MEMINI_NAMESPACE_SCOPE"] = "owner-repo";
  try {
    execSync("git init -q", { cwd: dir });
    execSync("git remote add origin https://github.com/acme/widget.git", { cwd: dir });
    const { resolveProject } = await import("./_shared.mjs?cb=" + Date.now());
    assert.equal(resolveProject(dir), "acme-widget");
  } finally {
    if (prevScope === undefined) delete process.env["MEMINI_NAMESPACE_SCOPE"];
    else process.env["MEMINI_NAMESPACE_SCOPE"] = prevScope;
    if (prevCache === undefined) delete process.env["XDG_CACHE_HOME"];
    else process.env["XDG_CACHE_HOME"] = prevCache;
    rmSync(dir, { recursive: true, force: true });
  }
});

test("resolveProject: self-heals to the same namespace after the remote is removed", async () => {
  const { execSync } = await import("node:child_process");
  const { mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const dir = mkdtempSync(join(tmpdir(), "memini-test-"));
  const prevCache = process.env["XDG_CACHE_HOME"];
  process.env["XDG_CACHE_HOME"] = mkdtempSync(join(tmpdir(), "memini-cache-"));
  try {
    execSync("git init -q", { cwd: dir });
    execSync("git remote add origin https://github.com/acme/widget.git", { cwd: dir });
    const { resolveProject } = await import("./_shared.mjs?cb=" + Date.now());
    // First resolution derives + caches "widget" under both the remote and path keys.
    assert.equal(resolveProject(dir), "widget");
    // Drop the remote: without self-heal this would fall back to the toplevel
    // basename and silently orphan the project's memory.
    execSync("git remote remove origin", { cwd: dir });
    assert.equal(resolveProject(dir), "widget");
  } finally {
    if (prevCache === undefined) delete process.env["XDG_CACHE_HOME"];
    else process.env["XDG_CACHE_HOME"] = prevCache;
    rmSync(dir, { recursive: true, force: true });
  }
});

test("session-start.mjs: fetches the briefing with right namespace, writes context to stdout", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ method: req.method, url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.setHeader("Content-Type", "application/json");
    // session-start.mjs makes a single layered briefing call, not N searches.
    if (req.url.startsWith("/v1/namespaces/") && req.url.includes("/briefing")) {
      res.end(
        JSON.stringify({
          namespace: "memini",
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
  });

  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      { MEMINI_URL: url },
    );

    assert.match(stdout, /<memini-context[^>]*>/, "should emit context block");
    assert.match(stdout, /last session did X/, "should surface prior memory");
    assert.equal(hits.length, 1, `expected 1 briefing call, got ${hits.length}`);
    const [h] = hits;
    assert.equal(h.method, "GET");
    assert.equal(h.ns, "memini", `expected namespace=memini, got ${h.ns}`);
    assert.match(h.url, /^\/v1\/namespaces\/memini\/briefing\b/);
  } finally {
    await close();
  }
});

test("session-end.mjs: no events buffered → no POST, no noise", async () => {
  const cache = freshCache(); // empty buffer dir → no digest
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ method: req.method, url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });

  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "nobuf", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );

    assert.equal(hits.length, 0, "an empty session must not write a bare end marker");
  } finally {
    await close();
  }
});

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
    // Edit and Bash are buffered; Read is filtered out. None should POST.
    for (const [tool, input] of [
      ["Edit", { file_path: "foo.go" }],
      ["Read", { file_path: "bar.go" }],
      ["Bash", { command: "go test ./..." }],
    ]) {
      await runHook(
        "post-tool-use.mjs",
        JSON.stringify({ session_id: "buf1", cwd: __dirname, tool_name: tool, tool_input: input }),
        { MEMINI_URL: url, XDG_CACHE_HOME: cache },
      );
    }
    assert.equal(hits.length, 0, `PostToolUse must not POST, got ${hits.length} calls`);
  } finally {
    await close();
  }
});

test("session-end.mjs: distills buffered events into one digest", async () => {
  const cache = freshCache();
  // Buffer two edits (one file twice) and a command.
  const events = [
    ["Edit", { file_path: "auth.go" }],
    ["Edit", { file_path: "auth.go" }],
    ["Bash", { command: "go test ./..." }],
  ];
  for (const [tool, input] of events) {
    await runHook(
      "post-tool-use.mjs",
      JSON.stringify({ session_id: "dig1", cwd: __dirname, tool_name: tool, tool_input: input }),
      { XDG_CACHE_HOME: cache },
    );
  }

  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });

  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "dig1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );

    assert.equal(hits.length, 1, "session-end should write exactly one digest memory");
    const body = JSON.parse(hits[0].body);
    assert.equal(body.tier, "episodic");
    assert.match(body.content, /3 tool calls/, "digest should count events");
    assert.match(body.content, /auth\.go \(2\)/, "digest should count repeated file edits");
    assert.match(body.content, /go test/, "digest should mention commands");
    assert.deepEqual(body.metadata.files, ["auth.go"]);
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
    { XDG_CACHE_HOME: cache },
  );

  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });

  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "codexpatch1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );

    assert.equal(hits.length, 1, "session-end should write a digest for apply_patch edits");
    const body = JSON.parse(hits[0].body);
    assert.match(body.content, /Edited: src\/auth\.js, src\/session\.js\./);
    assert.deepEqual(body.metadata.files, ["src/auth.js", "src/session.js"]);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: searches by file path, surfaces context to stdout", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        results: [{ memory: { content: "auth decision" }, score: 0.95 }],
      }),
    );
  });

  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({
        session_id: "s1",
        cwd: __dirname,
        tool_name: "Read",
        tool_input: { file_path: "internal/auth.go" },
      }),
      { MEMINI_URL: url },
    );

    assert.match(stdout, /<memini-pretool[^>]*>/, "should emit pretool block");
    assert.match(stdout, /auth decision/, "should surface hit");
    assert.equal(hits.length, 1);
    const body = JSON.parse(hits[0].body);
    assert.match(body.query, /Read on internal\/auth\.go/);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: excludes this session's own captures from recall", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { content: "auth decision" }, score: 0.9 }] }));
  });

  try {
    await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({
        session_id: "s1",
        cwd: __dirname,
        tool_name: "Read",
        tool_input: { file_path: "internal/auth.go" },
      }),
      { MEMINI_URL: url },
    );
    assert.equal(hits.length, 1);
    const body = JSON.parse(hits[0].body);
    assert.deepEqual(body.exclude_metadata, { session_id: "s1" });
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
      { XDG_CACHE_HOME: cache },
    );
  }

  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });

  try {
    await runHook(
      "pre-compact.mjs",
      JSON.stringify({ session_id: "pc1", cwd: __dirname, trigger: "auto" }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
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
  const { url, close } = await startMockServer((req, res) => {
    hits.push({ url: req.url });
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });
  try {
    await runHook(
      "pre-compact.mjs",
      JSON.stringify({ session_id: "empty", cwd: __dirname }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(hits.length, 0, "no buffer should mean no checkpoint");
  } finally {
    await close();
  }
});

// Build a fake Claude Code transcript with `n` real user messages plus noise
// (tool-result arrays, sidechains, isMeta, command-caveat strings) that must
// not be counted.
function writeTranscript(path, userCount) {
  const lines = [];
  for (let i = 0; i < userCount; i++) {
    lines.push(JSON.stringify({ type: "user", message: { role: "user", content: `q${i}` } }));
    lines.push(JSON.stringify({ type: "assistant", message: { content: [{ type: "text", text: "a" }] } }));
  }
  // Noise that must be ignored by the counter:
  lines.push(JSON.stringify({ type: "user", isSidechain: true, message: { content: "side" } }));
  lines.push(JSON.stringify({ type: "user", isMeta: true, message: { content: "meta" } }));
  lines.push(JSON.stringify({ type: "user", message: { content: [{ type: "tool_result", content: "x" }] } }));
  lines.push(JSON.stringify({ type: "user", message: { content: "<local-command-caveat>noise" } }));
  writeFileSync(path, lines.join("\n") + "\n");
}

test("stop.mjs: blocks once after the auto-save interval, baselining first", async () => {
  const cache = freshCache();
  const tp = join(cache, "transcript.jsonl");
  const { url, close } = await startMockServer((_req, res) => {
    res.statusCode = 201;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ id: "m1" }));
  });
  const env = { MEMINI_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "5" };
  try {
    // First sight at 3 user messages → baseline, no block.
    writeTranscript(tp, 3);
    let { stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "as1", cwd: __dirname, transcript_path: tp }),
      env,
    );
    assert.equal(stdout.trim(), "", "first sight should baseline, not block");

    // Below interval (3 → 6, delta 3 < 5) → still no block.
    writeTranscript(tp, 6);
    ({ stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "as1", cwd: __dirname, transcript_path: tp }),
      env,
    ));
    assert.equal(stdout.trim(), "", "below interval should not block");

    // At/over interval (3 → 9, delta 6 >= 5) → block with decision JSON.
    writeTranscript(tp, 9);
    ({ stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "as1", cwd: __dirname, transcript_path: tp }),
      env,
    ));
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
  const { url, close } = await startMockServer((_req, res) => {
    res.statusCode = 201;
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ id: "m1" }));
  });
  try {
    // stop_hook_active → pass through
    let { stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "g1", cwd: __dirname, transcript_path: tp, stop_hook_active: true }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "1" },
    );
    assert.equal(stdout.trim(), "", "stop_hook_active must pass through");

    // opt-out
    ({ stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "g2", cwd: __dirname, transcript_path: tp }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE: "0", MEMINI_AUTO_SAVE_INTERVAL: "1" },
    ));
    assert.equal(stdout.trim(), "", "MEMINI_AUTO_SAVE=0 must pass through");

    // no transcript_path
    ({ stdout } = await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "g3", cwd: __dirname }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "1" },
    ));
    assert.equal(stdout.trim(), "", "missing transcript must pass through");
  } finally {
    await close();
  }
});

test("mcp-headers.mjs: emits cwd-resolved namespace + bearer when token set", async () => {
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_TOKEN: "tok-123",
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "memini", "namespace from repo basename");
  assert.equal(h.Authorization, "Bearer tok-123");
});

test("mcp-headers.mjs: omits Authorization when no token", async () => {
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_TOKEN: "",
    MEMINI_API_KEY: "",
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "memini");
  assert.equal(h.Authorization, undefined);
});

test("plaintext bearer guard warns once for http to a non-loopback host", async () => {
  const { createPlaintextBearerAuthGuard } = await import("./_shared.mjs");
  const warnings = [];
  const guard = createPlaintextBearerAuthGuard((m) => warnings.push(m), {});
  guard("http://memini.example.com", "secret");
  guard("http://memini.example.com", "secret");
  assert.equal(warnings.length, 1);
});

test("plaintext bearer guard is silent for loopback, https, and no secret", async () => {
  const { createPlaintextBearerAuthGuard } = await import("./_shared.mjs");
  const warnings = [];
  const guard = createPlaintextBearerAuthGuard((m) => warnings.push(m), {});
  guard("http://localhost:8080", "secret");
  guard("https://memini.example.com", "secret");
  guard("http://memini.example.com", "");
  assert.equal(warnings.length, 0);
});

test("plaintext bearer guard throws when MEMINI_REQUIRE_HTTPS=1", async () => {
  const { createPlaintextBearerAuthGuard } = await import("./_shared.mjs");
  const guard = createPlaintextBearerAuthGuard(() => {}, { MEMINI_REQUIRE_HTTPS: "1" });
  assert.throws(() => guard("http://memini.example.com", "secret"), /plaintext HTTP/);
});

// --- Injection budget env knobs ------------------------------------------

test("intEnv/floatEnv/listEnv/labelsEnv parse env vars defensively", async () => {
  const { intEnv, floatEnv, listEnv, labelsEnv, approxTokens, fitByTokens } = await import("./_shared.mjs");
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
  // ~12 words => ~16 tokens at 4/3; pick something predictable.
  const long = Array.from({ length: 60 }, (_, i) => `w${i}`).join(" ");
  const expected = Math.ceil((60 * 4) / 3);
  assert.equal(approxTokens(long), expected);

  const items = ["one", "two three", "four five six seven eight"];
  const unlimited = fitByTokens(items, 0);
  assert.deepEqual(unlimited.items, items);
  assert.equal(unlimited.dropped, 0);

  // With a budget that fits item 0 only, the others drop.
  const tight = fitByTokens(items, approxTokens("one"));
  assert.deepEqual(tight.items, ["one"]);
  assert.equal(tight.dropped, 2);
});

test("session-start.mjs: MEMINI_INJECT_BRIEFING_* caps per-section results", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res) => {
    hits.push(req.url);
    const u = new URL(req.url, url);
    // Server-side cap: honor per_section_X. The plugin sends these so the
    // mock must apply them to verify the cap actually fires server-side.
    // 0 explicitly disables a section; missing param keeps the full set
    // (the server defaults to 5, but this mock has fewer items per section
    // so we just return what's available).
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
        namespace: "ns",
        pinned: cap("per_section_pinned", all.pinned),
        facts: cap("per_section_facts", all.facts),
        procedures: cap("per_section_procedures", all.procedures),
        recent: cap("per_section_recent", all.recent),
      }),
    );
  });
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      {
        MEMINI_URL: url,
        MEMINI_INJECT_BRIEFING_PINNED: "1",
        MEMINI_INJECT_BRIEFING_FACTS: "0",
        MEMINI_INJECT_BRIEFING_PROCEDURES: "5",
        MEMINI_INJECT_BRIEFING_RECENT: "3",
      },
    );
    // Exactly one briefing call, with per-section caps applied.
    assert.equal(hits.length, 1);
    const u = new URL(hits[0], url);
    assert.equal(u.searchParams.get("per_section_pinned"), "1");
    assert.equal(u.searchParams.get("per_section_facts"), "0");
    assert.equal(u.searchParams.get("per_section_procedures"), "5");
    assert.equal(u.searchParams.get("per_section_recent"), "3");
    // Server-side caps mean only one pinned renders, and facts renders none.
    assert.match(stdout, /- p1/);
    assert.doesNotMatch(stdout, /p2/);
    assert.doesNotMatch(stdout, /Decisions/);
  } finally {
    await close();
  }
});

test("session-start.mjs: MEMINI_INJECT_BRIEFING_MAX_TOK truncates the rendered block", async () => {
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        namespace: "ns",
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
  });
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      { MEMINI_URL: url, MEMINI_INJECT_BRIEFING_MAX_TOK: "20" },
    );
    // Truncation marker appears when the cap drops items.
    assert.match(stdout, /\[...\s+\d+ item\(s\) truncated/);
    // First item still renders (head-most sections keep priority).
    assert.match(stdout, /alpha/);
  } finally {
    await close();
  }
});

test("session-start.mjs: MEMINI_INJECT_LABELS=tier renders tier annotations", async () => {
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        namespace: "ns",
        pinned: [],
        facts: [{ content: "use tabs in this project", tier: "semantic" }],
        procedures: [],
        recent: [],
      }),
    );
  });
  try {
    const { stdout } = await runHook(
      "session-start.mjs",
      JSON.stringify({ session_id: "s1", cwd: __dirname }),
      { MEMINI_URL: url, MEMINI_INJECT_LABELS: "tier,reason" },
    );
    // Labelled format includes the tier tag plus the section reason.
    assert.match(stdout, /\[semantic · durable fact\]/);
    assert.match(stdout, /use tabs in this project/);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_ITEMS caps items per file", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    const limit = JSON.parse(body || "{}").limit || 5;
    const n = Math.min(limit, 5);
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        results: Array.from({ length: n }, (_, i) => ({
          memory: { content: `hit-${i}` },
          score: 0.9 - i * 0.1,
        })),
      }),
    );
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({
        session_id: "s1",
        tool_name: "Read",
        tool_input: { file_path: "/tmp/foo" },
      }),
      { MEMINI_URL: url, MEMINI_INJECT_PRETOOL_ITEMS: "2" },
    );
    const parsed = JSON.parse(stdout);
    const ctx = parsed.hookSpecificOutput.additionalContext;
    // Two hits surface, three are dropped (server cap respected).
    assert.match(ctx, /hit-0/);
    assert.match(ctx, /hit-1/);
    assert.doesNotMatch(ctx, /hit-3/);
    const [h] = hits;
    assert.equal(JSON.parse(h.body).limit, 2);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_MIN_SCORE drops low-scored hits", async () => {
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        results: [
          { memory: { content: "strong" }, score: 0.9 },
          { memory: { content: "weak" }, score: 0.3 },
        ],
      }),
    );
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({
        session_id: "s1",
        tool_name: "Read",
        tool_input: { file_path: "/tmp/foo" },
      }),
      { MEMINI_URL: url, MEMINI_INJECT_PRETOOL_MIN_SCORE: "0.5" },
    );
    const ctx = JSON.parse(stdout).hookSpecificOutput.additionalContext;
    assert.match(ctx, /strong/);
    assert.doesNotMatch(ctx, /weak/);
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_TOOLS skips tools outside the allowlist", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res) => {
    hits.push(req.url);
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ results: [{ memory: { content: "x" }, score: 0.9 }] }));
  });
  try {
    // Bash isn't in the allowlist — the hook must skip without POSTing.
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({
        session_id: "s1",
        tool_name: "Bash",
        tool_input: { command: "ls" },
      }),
      { MEMINI_URL: url, MEMINI_INJECT_PRETOOL_TOOLS: "Read|Edit" },
    );
    assert.equal(stdout, "", "tool outside allowlist must produce no context");
    assert.equal(hits.length, 0, "tool outside allowlist must not hit memini");
  } finally {
    await close();
  }
});

test("pre-tool-use.mjs: MEMINI_INJECT_PRETOOL_MAX_TOK truncates per-file block", async () => {
  const { url, close } = await startMockServer((req, res) => {
    res.setHeader("Content-Type", "application/json");
    res.end(
      JSON.stringify({
        results: Array.from({ length: 4 }, (_, i) => ({
          memory: { content: `payload-${i} payload-${i} payload-${i} payload-${i}` },
          score: 0.9 - i * 0.1,
        })),
      }),
    );
  });
  try {
    const { stdout } = await runHook(
      "pre-tool-use.mjs",
      JSON.stringify({
        session_id: "s1",
        tool_name: "Read",
        tool_input: { file_path: "/tmp/foo" },
      }),
      { MEMINI_URL: url, MEMINI_INJECT_PRETOOL_ITEMS: "4", MEMINI_INJECT_PRETOOL_MAX_TOK: "10" },
    );
    const ctx = JSON.parse(stdout).hookSpecificOutput.additionalContext;
    assert.match(ctx, /\[...\s+\d+ item\(s\) truncated/);
  } finally {
    await close();
  }
});
