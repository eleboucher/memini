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
import { dirname, resolve } from "node:path";
import { mkdtempSync } from "node:fs";
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

test("resolveProject picks git toplevel basename over cwd", async () => {
  const { resolveProject } = await import("./_shared.mjs");
  const proj = resolveProject(__dirname);
  // We're inside the memini repo, so this should be "memini"
  assert.equal(proj, "memini");
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

test("session-start.mjs: queries with right namespace, writes context to stdout", async () => {
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ method: req.method, url: req.url, ns: req.headers["x-memini-namespace"], body });
    res.setHeader("Content-Type", "application/json");
    if (req.url === "/v1/search") {
      res.end(
        JSON.stringify({
          results: [
            { memory: { content: "last session did X" }, score: 0.9 },
            { memory: { content: "convention: use tabs" }, score: 0.8 },
          ],
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
    assert.equal(hits.length, 2, `expected 2 search calls, got ${hits.length}`);
    for (const h of hits) {
      assert.equal(h.ns, "memini", `expected namespace=memini, got ${h.ns}`);
      assert.equal(h.url, "/v1/search");
    }
  } finally {
    await close();
  }
});

test("session-end.mjs: falls back to a bare marker when no events buffered", async () => {
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

    assert.equal(hits.length, 1);
    assert.equal(hits[0].url, "/v1/memories");
    assert.equal(hits[0].ns, "memini");
    const body = JSON.parse(hits[0].body);
    assert.equal(body.tier, "episodic");
    assert.match(body.content, /Session ended in memini/);
    assert.deepEqual(body.tags, ["session-marker", "memini"]);
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
