#!/usr/bin/env node
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");
const fixtures = path.join(root, "test", "fixtures", "cursor");

// Same isolation doctrine as _test.mjs: a developer's real config dir must
// never leak into a spawned hook (session-start syncs the credentials file).
process.env.XDG_CONFIG_HOME = fs.mkdtempSync(path.join(os.tmpdir(), "memini-config-"));

function fixture(name) {
  return fs.readFileSync(path.join(fixtures, name), "utf8");
}

// Strip ambient MEMINI_*/CURSOR_* vars: a shell inside the Cursor IDE can
// carry them and flip host detection underneath a test.
function baseEnv() {
  const env = { ...process.env };
  for (const k of Object.keys(env)) if (k.startsWith("MEMINI_") || k.startsWith("CURSOR_")) delete env[k];
  env.MEMINI_API_KEY = "super-secret-test-token";
  env.MEMINI_TIMEOUT_MS = "50";
  return env;
}

function freshCache() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "memini-cursor-"));
}

// Async spawn, never spawnSync: the mock server lives in THIS process, so a
// synchronous spawn would deadlock it.
function hook(script, payload, { cache, ...extra } = {}) {
  return new Promise((resolveProm, reject) => {
    const child = spawn(process.execPath, [path.join(root, "scripts", script)], {
      cwd: root,
      env: { ...baseEnv(), XDG_CACHE_HOME: cache ?? freshCache(), ...extra },
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (c) => (stdout += c));
    child.stderr.on("data", (c) => (stderr += c));
    child.on("close", (status) => resolveProm({ status, stdout, stderr }));
    child.on("error", reject);
    child.stdin.end(payload);
  });
}

const HANDSHAKE = {
  namespace: "cursor/ns",
  namespace_source: "remote",
  identity: { authenticated: true, key_name: "test-key" },
  settings: {},
  settings_sources: {},
  read_set: [{ namespace: "cursor/ns", origin: "primary" }],
  server: { version: "test-server", default_namespace: "default" },
};

const SEARCH_HIT = {
  results: [{ score: 0.9, memory: { id: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", content: "rotate tokens weekly", tier: "semantic" } }],
};

// Minimal memini mock: handshake + briefing served automatically, everything
// else recorded into `calls` ({method, url, ns, body}) for assertions.
function startMock() {
  const calls = [];
  return new Promise((resolveProm) => {
    const server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        if (req.method === "POST" && req.url === "/v1/activity/injected") {
          res.statusCode = 204;
          res.end();
          return;
        }
        if (req.method === "POST" && req.url === "/v1/handshake") {
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify(HANDSHAKE));
          return;
        }
        calls.push({ method: req.method, url: req.url, ns: req.headers["x-memini-namespace"], body });
        if (req.method === "GET" && req.url.startsWith("/v1/namespaces/briefing")) {
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify({ namespace: "cursor/ns", pinned: [], facts: [], procedures: [], recent: [] }));
          return;
        }
        if (req.method === "POST" && req.url === "/v1/search") {
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify(SEARCH_HIT));
          return;
        }
        if (req.method === "POST" && req.url === "/v1/memories") {
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify({ id: "bbbbbbbb-1111-4222-8333-444444444444" }));
          return;
        }
        res.statusCode = 404;
        res.end();
      });
    });
    server.listen(0, "127.0.0.1", () =>
      resolveProm({
        url: `http://127.0.0.1:${server.address().port}`,
        calls,
        close: () => new Promise((r) => server.close(r)),
      }),
    );
  });
}

const NO_LEAK = /super-secret-test-token/;

test("hookCacheKey: Cursor keys by conversation_id, other hosts by ppid", async () => {
  // Cursor spawns each hook under a FRESH parent process (observed live:
  // on-miss hooks missed the ppid-keyed handshake cache and re-handshook), so
  // the per-session cache keys on conversation_id there.
  const { hookCacheKey } = await import("./_shared.mjs");
  const a = hookCacheKey({ conversation_id: "conv-1", cursor_version: "1.7.2" });
  const b = hookCacheKey({ conversation_id: "conv-1", cursor_version: "1.7.2" });
  const c = hookCacheKey({ conversation_id: "conv-2", cursor_version: "1.7.2" });
  assert.equal(a, b, "same conversation → same key across hook invocations");
  assert.notEqual(a, c, "different conversation → different key");
  assert.notEqual(a, process.ppid, "Cursor key must not ride the unstable ppid");
  assert.equal(hookCacheKey({ session_id: "s", cwd: "." }), process.ppid, "Claude keeps ppid");
  assert.equal(hookCacheKey({ conversation_id: "conv-1" }, { PLUGIN_ROOT: "/x" }), process.ppid, "Codex keeps ppid");
});

test("Cursor hot-path hooks do not share a cache across conversations", async () => {
  // A prompt hook under a different conversation_id than the session-start
  // must see a cold cache, not another conversation's namespace. Under ppid
  // keying this wrongly injects — both hooks share one parent in the test.
  const mock = await startMock();
  const cache = freshCache(); // deliberately SHARED between the two hooks
  try {
    const start = await hook("session-start.mjs", fixture("session-start.json"), {
      cache,
      MEMINI_BASE_URL: mock.url,
    });
    assert.equal(start.status, 0, start.stderr);

    const other = { ...JSON.parse(fixture("user-prompt.json")), conversation_id: "cursor-other" };
    const prompt = await hook("user-prompt-submit.mjs", JSON.stringify(other), {
      cache,
      MEMINI_BASE_URL: mock.url,
    });
    assert.equal(prompt.status, 0, prompt.stderr);
    assert.equal(prompt.stdout, "", "no cache for this conversation → degraded → silent");
    assert.equal(mock.calls.filter((c) => c.url === "/v1/search").length, 0);
  } finally {
    await mock.close();
  }
});

test("Cursor sessionStart emits the native additional_context envelope", async () => {
  // Dead server → degraded → the memory directive still goes out, wrapped for
  // Cursor (never Claude's plain stdout, never Codex's hookSpecificOutput).
  const r = await hook("session-start.mjs", fixture("session-start.json"), {
    MEMINI_BASE_URL: "http://127.0.0.1:1",
  });
  assert.equal(r.status, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.match(out.additional_context, /memini-memory-directive/);
  assert.equal(out.hookSpecificOutput, undefined);
  assert.doesNotMatch(r.stdout + r.stderr, NO_LEAK);
});

test("Cursor host is detected from CURSOR_VERSION alone (no payload markers)", async () => {
  const r = await hook(
    "session-start.mjs",
    JSON.stringify({ session_id: "cursor-env", cwd: "." }),
    { MEMINI_BASE_URL: "http://127.0.0.1:1", CURSOR_VERSION: "1.7.2", CURSOR_PROJECT_DIR: "." },
  );
  assert.equal(r.status, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.match(out.additional_context, /memini-memory-directive/);
  assert.doesNotMatch(r.stdout + r.stderr, NO_LEAK);
});

test("Cursor prompt and preTool recalls inject via additional_context / agent_message", async () => {
  const mock = await startMock();
  const cache = freshCache();
  const env = { MEMINI_BASE_URL: mock.url, cache };
  try {
    // One conversation = one conversation_id = one handshake cache entry
    // (hookCacheKey), seeded by session-start. Prompt and pretool use SEPARATE
    // conversations: sharing one would trip cross-surface dedupe (the second
    // surface suppresses the same hit as already-in-context — by design).
    const conv = (name, id) => {
      const p = JSON.parse(fixture(name));
      p.session_id = id;
      p.conversation_id = id;
      return JSON.stringify(p);
    };
    const start = await hook("session-start.mjs", conv("session-start.json", "cursor-conv-1"), env);
    assert.equal(start.status, 0, start.stderr);

    const prompt = await hook("user-prompt-submit.mjs", conv("user-prompt.json", "cursor-conv-1"), env);
    assert.equal(prompt.status, 0, prompt.stderr);
    const promptOut = JSON.parse(prompt.stdout);
    assert.match(promptOut.additional_context, /<memini-recall read-only>/);
    assert.match(promptOut.additional_context, /rotate tokens weekly/);
    assert.equal(promptOut.hookSpecificOutput, undefined);

    const start2 = await hook("session-start.mjs", conv("session-start.json", "cursor-conv-2"), env);
    assert.equal(start2.status, 0, start2.stderr);
    const pre = await hook("pre-tool-use.mjs", conv("pre-tool-write.json", "cursor-conv-2"), env);
    assert.equal(pre.status, 0, pre.stderr);
    const preOut = JSON.parse(pre.stdout);
    assert.equal(preOut.permission, "allow");
    assert.match(preOut.agent_message, /<memini-pretool tool="Write" read-only>/);
    assert.match(preOut.agent_message, /rotate tokens weekly/);
    assert.equal(preOut.hookSpecificOutput, undefined);

    // The resolved namespace rode the per-session cache onto the recall calls.
    for (const c of mock.calls.filter((c) => c.url === "/v1/search")) {
      assert.equal(c.ns, "cursor/ns");
    }
    assert.doesNotMatch(prompt.stdout + prompt.stderr + pre.stdout + pre.stderr, NO_LEAK);
  } finally {
    await mock.close();
  }
});

test("Cursor postToolUse buffers the Shell event keyed by conversation_id", async () => {
  const cache = freshCache();
  const r = await hook("post-tool-use.mjs", fixture("post-tool-shell.json"), {
    cache,
    MEMINI_BASE_URL: "http://127.0.0.1:1",
  });
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout, "");
  const buffer = fs.readFileSync(path.join(cache, "memini", "sessions", "cursor-tools.jsonl"), "utf8");
  const event = JSON.parse(buffer.trim());
  assert.equal(event.tool, "Shell");
  assert.equal(event.cmd, "go test ./...");
  assert.doesNotMatch(r.stdout + r.stderr, NO_LEAK);
});

test("Cursor Stop never parses the transcript and never emits a followup", async () => {
  // A capture-worthy Claude-shaped transcript must stay unread under the
  // Cursor host gating, and no followup_message may be emitted (it would
  // auto-submit as a user message).
  const mock = await startMock();
  const cache = freshCache();
  const transcript = path.join(cache, "transcript.jsonl");
  fs.writeFileSync(
    transcript,
    [
      JSON.stringify({ type: "user", message: { content: "ship the auth change" } }),
      JSON.stringify({ type: "assistant", message: { id: "a1", content: [{ type: "text", text: "done, shipped" }] } }),
    ].join("\n"),
  );
  const payload = { ...JSON.parse(fixture("stop.json")), transcript_path: transcript };
  try {
    const r = await hook("stop.mjs", JSON.stringify(payload), { cache, MEMINI_BASE_URL: mock.url });
    assert.equal(r.status, 0, r.stderr);
    const out = JSON.parse(r.stdout);
    assert.deepEqual(out, {});
    assert.equal(out.followup_message, undefined);
    assert.equal(
      mock.calls.filter((c) => c.url === "/v1/memories").length,
      0,
      "no turn capture, no checkpoint write under Cursor",
    );
    assert.doesNotMatch(r.stdout + r.stderr, NO_LEAK);
  } finally {
    await mock.close();
  }
});

test("Cursor preCompact and sessionEnd stay side-effect-only with valid output", async () => {
  const mock = await startMock();
  const cache = freshCache();
  try {
    const compact = await hook("pre-compact.mjs", fixture("pre-compact.json"), { cache, MEMINI_BASE_URL: mock.url });
    assert.equal(compact.status, 0, compact.stderr);
    assert.deepEqual(JSON.parse(compact.stdout), {});

    const end = await hook("session-end.mjs", fixture("session-end.json"), { cache, MEMINI_BASE_URL: mock.url });
    assert.equal(end.status, 0, end.stderr);
    assert.equal(end.stdout, "");

    assert.equal(
      mock.calls.filter((c) => c.url === "/v1/memories").length,
      0,
      "no buffered events → no checkpoint or digest writes",
    );
    assert.doesNotMatch(compact.stdout + compact.stderr + end.stdout + end.stderr, NO_LEAK);
  } finally {
    await mock.close();
  }
});

test("Cursor hooks file wires the seven documented events via CURSOR_PLUGIN_ROOT", () => {
  const cursor = JSON.parse(fs.readFileSync(path.join(root, "hooks", "hooks.cursor.json")));
  assert.equal(cursor.version, 1);
  assert.deepEqual(
    Object.keys(cursor.hooks).sort(),
    ["beforeSubmitPrompt", "postToolUse", "preCompact", "preToolUse", "sessionEnd", "sessionStart", "stop"].sort(),
  );
  for (const handlers of Object.values(cursor.hooks)) {
    for (const handler of handlers) {
      assert.match(handler.command, /\$\{CURSOR_PLUGIN_ROOT\}\/scripts\/run\.sh/);
      assert.match(handler.command, /\$\{CURSOR_PLUGIN_ROOT\}\/scripts\/[a-z-]+\.mjs/);
    }
  }
});

test("each host loads only its own hooks file", () => {
  // Claude Code always loads hooks/hooks.json on top of the manifest path, so
  // a file there would run hooks twice under Claude.
  assert.equal(fs.existsSync(path.join(root, "hooks", "hooks.json")), false);
  const claude = JSON.parse(fs.readFileSync(path.join(root, ".claude-plugin", "plugin.json")));
  const codex = JSON.parse(fs.readFileSync(path.join(root, ".codex-plugin", "plugin.json")));
  const cursor = JSON.parse(fs.readFileSync(path.join(root, ".cursor-plugin", "plugin.json")));
  assert.equal(claude.hooks, "./hooks/hooks.claude.json");
  assert.equal(codex.hooks, "./hooks/hooks.codex.json");
  assert.equal(cursor.hooks, "./hooks/hooks.cursor.json");
});

test("Cursor manifest and marketplace are complete and version-aligned", () => {
  const cursor = JSON.parse(fs.readFileSync(path.join(root, ".cursor-plugin", "plugin.json")));
  const claude = JSON.parse(fs.readFileSync(path.join(root, ".claude-plugin", "plugin.json")));
  assert.equal(cursor.name, "memini");
  assert.equal(cursor.version, claude.version, "manifest versions drift (bump.yml syncs them)");
  assert.equal(cursor.skills, "./skills/");
  // commands/ are Claude-only: they invoke scripts via ${CLAUDE_PLUGIN_ROOT},
  // which Cursor does not expand. The skills cover the same surface.
  assert.equal(cursor.commands, undefined);
  assert.equal(cursor.mcpServers, "./.mcp.cursor.json");

  const marketplace = JSON.parse(
    fs.readFileSync(path.join(root, "..", ".cursor-plugin", "marketplace.json")),
  );
  const entry = marketplace.plugins.find((p) => p.name === "memini");
  assert.equal(entry.source, "./plugin");
});

test("Cursor MCP config is a bare static-localhost server", () => {
  // Plugin-bundled mcp.json does NOT interpolate ${env:...} — verified live:
  // the literal placeholder was sent as the bearer and 401'd. So the bundled
  // entry is bare localhost; auth/remote setups override via a user-scope
  // ~/.cursor/mcp.json entry (where ${env:...} works — see plugin README).
  const mcp = JSON.parse(fs.readFileSync(path.join(root, ".mcp.cursor.json")));
  const server = mcp.mcpServers.memini;
  assert.equal(server.url, "http://localhost:8080/mcp");
  assert.equal(server.headers, undefined);
});
