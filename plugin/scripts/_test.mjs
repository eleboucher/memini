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
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import http from "node:http";

const __dirname = dirname(fileURLToPath(import.meta.url));
const SCRIPTS = __dirname;

// Point XDG_CONFIG_HOME at an empty temp dir for the whole run (spawned hooks
// inherit it) so a developer's real ~/.config/memini/config.json can't leak
// tenant prefixes into these tests. Tenant tests override it per-test.
process.env.XDG_CONFIG_HOME = mkdtempSync(join(tmpdir(), "memini-config-"));

// Strip every ambient MEMINI_* env var for the whole run. A developer's shell
// (or this very plugin, running against a live memini) commonly exports
// MEMINI_NAMESPACE, MEMINI_CAPTURE_TURNS, MEMINI_BASE_URL, MEMINI_API_KEY,
// etc. for day-to-day use; those leak into both in-process calls (resolveProject)
// and spawned hooks (runHook's env is seeded from process.env) and clobber
// tests that assert the *computed* default. Each test sets whatever MEMINI_*
// it needs explicitly, so a clean slate here is always safe.
for (const k of Object.keys(process.env)) {
  if (k.startsWith("MEMINI_")) delete process.env[k];
}

// Each test that touches the session buffer gets an isolated cache dir so runs
// don't pollute the real ~/.cache or each other.
function freshCache() {
  return mkdtempSync(join(tmpdir(), "memini-test-"));
}

// A temp XDG_CONFIG_HOME dir; when `config` is given it is written as
// memini/config.json inside it.
function freshConfig(config) {
  const dir = mkdtempSync(join(tmpdir(), "memini-config-"));
  if (config !== undefined) {
    mkdirSync(join(dir, "memini"), { recursive: true });
    writeFileSync(join(dir, "memini", "config.json"), JSON.stringify(config));
  }
  return dir;
}

// Run `fn` with process.env overrides applied (undefined deletes), restoring
// the previous values after — tenant tests exercise _shared.mjs in-process, so
// env must be isolated by hand.
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
  // A developer shell may export MEMINI_BASE_URL/_URL/_API_KEY/_TOKEN pointing at
  // a real memini; strip them so each test's explicit env points the hook at the
  // in-process mock (canonical MEMINI_BASE_URL would otherwise win over the
  // test's MEMINI_URL and the hook would hit the real server).
  const base = { ...process.env };
  for (const k of ["MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_TOKEN"]) delete base[k];
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

// --- tenant roots config ($XDG_CONFIG_HOME/memini/config.json) --------------

test("resolveProject: tenant prefix applies on fresh derivation AND on a cache-hit; cache stays un-prefixed", async () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "projx");
  mkdirSync(dir);
  const configHome = freshConfig({ tenantRoots: [{ path: parent, tenant: "work" }] });
  await withEnv(
    {
      XDG_CONFIG_HOME: configHome,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: undefined,
    },
    async () => {
      const { resolveProject } = await import("./_shared.mjs?cb=tenant-" + Date.now());
      assert.equal(resolveProject(dir), "work/projx", "fresh derivation gets the tenant prefix");
      // Second call resolves from the project map; the prefix must still apply.
      assert.equal(resolveProject(dir), "work/projx", "cache-hit gets the tenant prefix too");
      // Removing the config removes the prefix — the cache stores the bare base.
      process.env.XDG_CONFIG_HOME = freshConfig();
      assert.equal(resolveProject(dir), "projx", "no config file -> no prefix, even for cached projects");
    },
  );
});

test("resolveProject: XDG fallback is ~/.config, not ~/config", async () => {
  const home = mkdtempSync(join(tmpdir(), "memini-home-"));
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "projy");
  mkdirSync(dir);
  mkdirSync(join(home, ".config", "memini"), { recursive: true });
  writeFileSync(
    join(home, ".config", "memini", "config.json"),
    JSON.stringify({ tenantRoots: [{ path: parent, tenant: "work" }] }),
  );
  await withEnv(
    {
      XDG_CONFIG_HOME: undefined,
      HOME: home,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: undefined,
    },
    async () => {
      const { resolveProject } = await import("./_shared.mjs?cb=xdg-" + Date.now());
      assert.equal(resolveProject(dir), "work/projy", "config under ~/.config must be honored");
    },
  );
});

test("resolveProject: empty-path and non-object tenant roots match nothing, later roots still work", async () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "projz");
  mkdirSync(dir);
  const outside = mkdtempSync(join(tmpdir(), "memini-outside-"));
  const configHome = freshConfig({
    tenantRoots: [{ path: "", tenant: "evil" }, "junk", { tenant: "nopath" }, { path: parent, tenant: "work" }],
  });
  await withEnv(
    {
      XDG_CONFIG_HOME: configHome,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: undefined,
    },
    async () => {
      const { resolveProject } = await import("./_shared.mjs?cb=empty-" + Date.now());
      // The empty-path entry must not startsWith-match every absolute cwd...
      assert.equal(resolveProject(outside), basename(outside), "empty path must not match everything");
      // ...and bad entries must not abort the scan before the valid root.
      assert.equal(resolveProject(dir), "work/projz", "valid root after bad entries still matches");
    },
  );
});

test("resolveProject: a configured trailing slash on the root still matches", async () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "projt");
  mkdirSync(dir);
  const configHome = freshConfig({ tenantRoots: [{ path: parent + "/", tenant: "work" }] });
  await withEnv(
    {
      XDG_CONFIG_HOME: configHome,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: undefined,
    },
    async () => {
      const { resolveProject } = await import("./_shared.mjs?cb=slash-" + Date.now());
      assert.equal(resolveProject(dir), "work/projt");
    },
  );
});

test("resolveProject: MEMINI_AGENT suffix applies unconditionally; only the tenant prefix is config-gated", async () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "proja");
  mkdirSync(dir);
  const withConfig = freshConfig({ tenantRoots: [{ path: parent, tenant: "work" }] });
  const noConfig = freshConfig();
  await withEnv(
    {
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: "reviewer",
    },
    async () => {
      process.env.XDG_CONFIG_HOME = withConfig;
      const { resolveProject } = await import("./_shared.mjs?cb=agent-" + Date.now());
      assert.equal(resolveProject(dir), "work/proja/reviewer", "config present -> tenant prefix + agent suffix");
      process.env.XDG_CONFIG_HOME = noConfig;
      // Zero-migration: MEMINI_AGENT predates the tenant feature, so a
      // config-less install keeps its pre-feature "<project>/<agent>" namespace.
      assert.equal(resolveProject(dir), "proja/reviewer", "no config -> no tenant prefix, but agent suffix still applies");
    },
  );
});

test("resolveProject: a tenant root matches on a path boundary, not a string prefix", async () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const workRoot = join(parent, "work");
  const workspace = join(parent, "workspace");
  mkdirSync(workRoot);
  mkdirSync(workspace);
  const configHome = freshConfig({ tenantRoots: [{ path: workRoot, tenant: "work" }] });
  await withEnv(
    {
      XDG_CONFIG_HOME: configHome,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: undefined,
    },
    async () => {
      const { resolveProject } = await import("./_shared.mjs?cb=bound-" + Date.now());
      assert.equal(resolveProject(workspace), "workspace", "~/dev/workspace must not match a ~/dev/work root");
      assert.equal(resolveProject(workRoot), "work/work", "the root itself does match");
    },
  );
});

test("resolveProject: a non-default config.template reshapes the namespace", async () => {
  const parent = mkdtempSync(join(tmpdir(), "memini-tenant-"));
  const dir = join(parent, "projtmpl");
  mkdirSync(dir);
  const configHome = freshConfig({
    tenantRoots: [{ path: parent, tenant: "work" }],
    template: "{tenant}-{project}",
  });
  await withEnv(
    {
      XDG_CONFIG_HOME: configHome,
      XDG_CACHE_HOME: freshCache(),
      MEMINI_NAMESPACE: undefined,
      MEMINI_AGENT: undefined,
    },
    async () => {
      const { resolveProject } = await import("./_shared.mjs?cb=tmpl-" + Date.now());
      assert.equal(resolveProject(dir), "work-projtmpl", "template joins segments with a dash");
    },
  );
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
    assert.match(stdout, /memory_remember/, "should inject the memory directive");
    assert.ok(!stdout.includes("<memory>"), "injected context must not contain memory markup");
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

    // Two calls: the long-lived digest and a supersede of the byte-identical
    // stop:<sessionId> marker the Stop hook emitted on the same final turn.
    assert.equal(hits.length, 2, "session-end should write the digest AND supersede the stop: marker");
    assert.equal(hits[0].url, "/v1/memories", "first call is the digest");
    assert.equal(hits[1].url, "/v1/memories/stop%3Adig1/supersede", "second call supersedes the stop: marker");
    const supersedeBody = JSON.parse(hits[1].body);
    assert.equal(supersedeBody.by, "session-end:dig1", "supersede target is the long-lived session-end row");
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

    assert.equal(hits.length, 2, "session-end writes the digest AND supersedes the stop: marker");
    const body = JSON.parse(hits[0].body);
    assert.match(body.content, /Edited: src\/auth\.js, src\/session\.js\./);
    assert.deepEqual(body.metadata.files, ["src/auth.js", "src/session.js"]);
  } finally {
    await close();
  }
});

test("session-end.mjs: supersede tolerates a 404 (stop: marker missing)", async () => {
  // The session-end hook must not fail when the Stop hook never wrote a
  // matching stop:<sessionId> row (e.g. a short session that compacted
  // before reaching the auto-save threshold). The server returns 404; the
  // hook logs in DEBUG and otherwise moves on. We verify the hook exited 0
  // and emitted no unhandled error in stderr.
  const cache = freshCache();
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "nfdig1", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "x.go" } }),
    { XDG_CACHE_HOME: cache },
  );

  let supersedeSeen = false;
  const { url, close } = await startMockServer((req, res, body) => {
    if (req.url === "/v1/memories") {
      res.setHeader("Content-Type", "application/json");
      res.statusCode = 201;
      res.end(JSON.stringify({ id: "session-end:nfdig1" }));
      return;
    }
    if (req.url === "/v1/memories/stop%3Anfdig1/supersede") {
      supersedeSeen = true;
      // The real server returns 404 when the stop: row doesn't exist; the
      // hook must swallow that and continue.
      res.statusCode = 404;
      res.end(JSON.stringify({ error: "memory not found" }));
      return;
    }
    res.statusCode = 500;
    res.end();
  });

  try {
    const { stderr } = await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "nfdig1", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.ok(supersedeSeen, "session-end should still POST the supersede even when the target is missing");
    assert.doesNotMatch(stderr, /UnhandledPromise|Rejection|TypeError/, "404 must not crash the hook");
  } finally {
    await close();
  }
});

test("session-end.mjs: percent-encodes the stop: id in the supersede path", async () => {
  // The stop: id contains a ':' which chi's router needs percent-encoded. If
  // we forgot to encode, the server would 404 with a "memory not found"
  // route mismatch instead of the real supersede handler.
  const cache = freshCache();
  await runHook(
    "post-tool-use.mjs",
    JSON.stringify({ session_id: "abc-123", cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "y.go" } }),
    { XDG_CACHE_HOME: cache },
  );

  const paths = [];
  const { url, close } = await startMockServer((req, res) => {
    paths.push(req.url);
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "ok" }));
  });

  try {
    await runHook(
      "session-end.mjs",
      JSON.stringify({ session_id: "abc-123", cwd: __dirname, reason: "user_exit" }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.ok(
      paths.includes("/v1/memories/stop%3Aabc-123/supersede"),
      `expected percent-encoded stop id in supersede path, got ${JSON.stringify(paths)}`,
    );
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

test("mcp-headers.mjs: MEMINI_REQUIRE_HTTPS=1 omits the bearer for plaintext non-loopback", async () => {
  // The headersHelper must go through the same plaintext-bearer guard as the
  // hooks' REST client; under REQUIRE_HTTPS it emits no Authorization rather
  // than leaking the key (a throw would break the MCP connection JSON).
  const { stdout, stderr } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_TOKEN: "tok-123",
    MEMINI_BASE_URL: "http://memini.example.com",
    MEMINI_REQUIRE_HTTPS: "1",
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "memini", "namespace must still be emitted");
  assert.equal(h.Authorization, undefined, "bearer must not travel over plaintext");
  assert.match(stderr, /plaintext HTTP/);
});

test("mcp-headers.mjs: warns (but still sends) for plaintext non-loopback by default", async () => {
  // Warn-and-send parity with the hooks' REST client default posture.
  const { stdout, stderr } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: __dirname,
    MEMINI_TOKEN: "tok-123",
    MEMINI_BASE_URL: "http://memini.example.com",
  });
  const h = JSON.parse(stdout);
  assert.equal(h.Authorization, "Bearer tok-123");
  assert.match(stderr, /plaintext HTTP/);
});

// The headersHelper runs with cwd = the plugin's version-named install dir and
// no CLAUDE_PROJECT_DIR. Resolving from cwd there scattered memories into
// version namespaces (e.g. "0.6.3"). These pin that it never happens: with no
// project cwd it uses the hooks' cached namespace, or omits the header.
test("mcp-headers.mjs: no CLAUDE_PROJECT_DIR + no cache → omits namespace (never cwd)", async () => {
  const cache = freshCache();
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: "",
    XDG_CACHE_HOME: cache,
    MEMINI_TOKEN: "",
    MEMINI_API_KEY: "",
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], undefined, "must not emit a cwd/version-derived namespace");
});

test("mcp-headers.mjs: no CLAUDE_PROJECT_DIR → uses the hooks' cached namespace", async () => {
  const cache = freshCache();
  const { writeNamespace } = await import("./_shared.mjs");
  await withEnv({ XDG_CACHE_HOME: cache }, () => writeNamespace("personal/memini"));
  const { stdout } = await runHook("mcp-headers.mjs", "", {
    CLAUDE_PROJECT_DIR: "",
    XDG_CACHE_HOME: cache,
    MEMINI_TOKEN: "",
    MEMINI_API_KEY: "",
  });
  const h = JSON.parse(stdout);
  assert.equal(h["X-Memini-Namespace"], "personal/memini", "namespace from the hooks' cache");
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

// --- inline <memory> extraction -------------------------------------------

test("parseMemoryBlocks: extracts contents, tolerates malformed and empty blocks", async () => {
  const { parseMemoryBlocks } = await import("./_shared.mjs");
  const text =
    'prose\n<memory>\n{"memories":[{"content":"a"},{"content":"b"}]}\n</memory>\n' +
    "more\n<memory>{not json}</memory>\n" +
    '<memory>{"memories":[]}</memory>\n' +
    '<memory>{"memories":[{"content":"  "},{"content":"c"}]}</memory>';
  assert.deepEqual(
    parseMemoryBlocks(text).map((m) => m.content),
    ["a", "b", "c"],
  );
  assert.deepEqual(parseMemoryBlocks(""), []);
  assert.deepEqual(parseMemoryBlocks("no blocks here"), []);
});

test("MEMORY_INSTRUCTION: directs to memory_remember, contains no memory markup", async () => {
  const { MEMORY_INSTRUCTION } = await import("./_shared.mjs");
  assert.match(MEMORY_INSTRUCTION, /memory_remember/, "must name the MCP tool");
  assert.match(MEMORY_INSTRUCTION, /memory_update/, "must keep the stale-fact correction path");
  assert.match(MEMORY_INSTRUCTION, /semantic/, "must keep tier guidance");
  assert.ok(!MEMORY_INSTRUCTION.includes("<memory>"), "must not contain a literal <memory> tag the model could echo");
  assert.ok(!MEMORY_INSTRUCTION.includes("</memory>"), "must not contain a closing memory tag");
});

test("extractAssistantText: pulls text blocks from a transcript, skips tool-only turns", async () => {
  const { extractAssistantText } = await import("./_shared.mjs");
  const transcript = [
    JSON.stringify({ type: "user", message: { content: "hi" } }),
    JSON.stringify({ type: "assistant", message: { content: "plain string reply" } }),
    JSON.stringify({
      type: "assistant",
      message: { content: [{ type: "text", text: "block reply" }, { type: "tool_use", name: "Bash" }] },
    }),
    JSON.stringify({ type: "assistant", message: { content: [{ type: "tool_use", name: "Read" }] } }),
    "not json",
  ].join("\n");
  assert.deepEqual(extractAssistantText(transcript), ["plain string reply", "block reply"]);
});

test("envEnabled: default-on unless explicitly opted out", async () => {
  const { envEnabled } = await import("./_shared.mjs");
  const save = process.env.MEMINI_TESTFLAG;
  try {
    delete process.env.MEMINI_TESTFLAG;
    assert.equal(envEnabled("MEMINI_TESTFLAG", true), true, "unset → default");
    assert.equal(envEnabled("MEMINI_TESTFLAG", false), false, "unset → default");
    for (const v of ["0", "false", "no", "off", "OFF", " Off "]) {
      process.env.MEMINI_TESTFLAG = v;
      assert.equal(envEnabled("MEMINI_TESTFLAG", true), false, `${v} → false`);
    }
    for (const v of ["1", "true", "yes", "on", "anything"]) {
      process.env.MEMINI_TESTFLAG = v;
      assert.equal(envEnabled("MEMINI_TESTFLAG", false), true, `${v} → true`);
    }
  } finally {
    if (save === undefined) delete process.env.MEMINI_TESTFLAG;
    else process.env.MEMINI_TESTFLAG = save;
  }
});

test("isRealUserMessage: strings pass, tool_result arrays and command noise skip", async () => {
  const { isRealUserMessage } = await import("./_shared.mjs");
  assert.equal(isRealUserMessage("hello"), true);
  assert.equal(isRealUserMessage([{ type: "tool_result", content: "x" }]), false);
  assert.equal(isRealUserMessage("<local-command-stdout>x"), false);
  assert.equal(isRealUserMessage("<command-name>/foo"), false);
  assert.equal(isRealUserMessage(undefined), false);
  // memini's own injected recall blocks and hook system reminders must never
  // be captured as a user turn — that would echo recalled memories back into
  // memory.
  assert.equal(isRealUserMessage('<memini-pretool tool="Read" read-only>x'), false);
  assert.equal(isRealUserMessage('<memini-context project="p" read-only>x'), false);
  assert.equal(isRealUserMessage("<system-reminder>injected</system-reminder>"), false);
  // Harness-injected background-task events are not user turns either.
  assert.equal(isRealUserMessage("<task-notification>\n<task-id>abc</task-id>"), false);
  assert.equal(isRealUserMessage("[SYSTEM NOTIFICATION - NOT USER INPUT]\nThis is an automated event"), false);
});

test("extractLastTurn: returns the final user→assistant turn, skips noise", async () => {
  const { extractLastTurn } = await import("./_shared.mjs");
  const transcript = [
    JSON.stringify({ type: "user", message: { role: "user", content: "first question" } }),
    JSON.stringify({ type: "assistant", message: { id: "msg_1", content: [{ type: "text", text: "first answer" }] } }),
    JSON.stringify({ type: "user", message: { role: "user", content: "second question" } }),
    // tool-use-only assistant turn must not blank out the captured reply
    JSON.stringify({ type: "assistant", message: { id: "msg_2", content: [{ type: "tool_use", name: "Bash" }] } }),
    JSON.stringify({ type: "assistant", message: { id: "msg_3", content: [{ type: "text", text: "second answer" }] } }),
    // noise that must be ignored
    JSON.stringify({ type: "user", isSidechain: true, message: { content: "side" } }),
    JSON.stringify({ type: "user", message: { content: [{ type: "tool_result", content: "x" }] } }),
    JSON.stringify({ type: "user", message: { content: "<local-command-stdout>noise" } }),
  ].join("\n");
  assert.deepEqual(extractLastTurn(transcript), {
    userText: "second question",
    assistantText: "second answer",
    assistantId: "msg_3",
  });
  assert.deepEqual(extractLastTurn(""), { userText: "", assistantText: "", assistantId: "" });

  // assistantId falls back to the top-level uuid when message.id is absent.
  const uuidTail = [
    JSON.stringify({ type: "user", message: { role: "user", content: "q" } }),
    JSON.stringify({ type: "assistant", uuid: "uuid-9", message: { content: [{ type: "text", text: "a" }] } }),
  ].join("\n");
  assert.equal(extractLastTurn(uuidTail).assistantId, "uuid-9");
});

test("stop.mjs: captures the last turn as episodic by default, dedupes, opts out", async () => {
  const cache = freshCache();
  const tp = join(cache, "turn.jsonl");
  writeFileSync(
    tp,
    [
      JSON.stringify({ type: "user", message: { role: "user", content: "how do I do X" } }),
      JSON.stringify({ type: "assistant", message: { id: "msg_abc", content: [{ type: "text", text: "do it like this" }] } }),
    ].join("\n") + "\n",
  );
  const hits = [];
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ url: req.url, body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });
  const turnPosts = () =>
    hits.filter((h) => {
      try {
        return JSON.parse(h.body)?.metadata?.source === "turn_capture";
      } catch {
        return false;
      }
    });
  try {
    // Default on → one episodic turn-capture write.
    await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "tc1", cwd: __dirname, transcript_path: tp }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );
    let posts = turnPosts();
    assert.equal(posts.length, 1, "should capture the turn by default");
    const body = JSON.parse(posts[0].body);
    assert.equal(body.tier, "episodic");
    assert.match(body.content, /how do I do X/);
    assert.match(body.content, /do it like this/);
    assert.deepEqual(body.tags?.includes("turn-capture"), true);
    assert.equal(body.metadata.format, "turn");

    // Same session + same final turn → deduped on the assistant id, no new write.
    await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "tc1", cwd: __dirname, transcript_path: tp }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(turnPosts().length, 1, "identical turn must not be re-captured");

    // Opt out → no capture even for a fresh session.
    await runHook(
      "stop.mjs",
      JSON.stringify({ session_id: "tc2", cwd: __dirname, transcript_path: tp }),
      { MEMINI_URL: url, XDG_CACHE_HOME: cache, MEMINI_CAPTURE_TURNS: "0" },
    );
    assert.equal(turnPosts().length, 1, "MEMINI_CAPTURE_TURNS=0 must not capture");
  } finally {
    await close();
  }
});

test("stop.mjs: turn-capture dedup survives an auto-save nudge (save-state co-tenancy)", async () => {
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
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });
  const captures = (txt) =>
    hits.filter((h) => {
      try {
        const b = JSON.parse(h.body);
        return b?.metadata?.source === "turn_capture" && b.content.includes(txt);
      } catch {
        return false;
      }
    });
  const env = { MEMINI_URL: url, XDG_CACHE_HOME: cache, MEMINI_AUTO_SAVE_INTERVAL: "2" };
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
    // lastCapturedTurn, so msgB dedupes here. (Regression guard for stop.mjs:83.)
    await runHook("stop.mjs", payload(), env);
    assert.equal(captures("answer B").length, 1, "msgB must not be re-captured after the nudge");
  } finally {
    await close();
  }
});

test("stop.mjs: turn-capture skips on stop_hook_active and missing transcript", async () => {
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
  const { url, close } = await startMockServer((req, res, body) => {
    hits.push({ body });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });
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
      { MEMINI_URL: url, XDG_CACHE_HOME: cache },
    );
    assert.equal(turnPosts().length, 0, "stop_hook_active must not capture");
    // No transcript path → nothing to capture, no crash.
    await runHook("stop.mjs", JSON.stringify({ session_id: "sk2", cwd: __dirname }), {
      MEMINI_URL: url,
      XDG_CACHE_HOME: cache,
    });
    assert.equal(turnPosts().length, 0, "missing transcript must not capture");
  } finally {
    await close();
  }
});

test("stop/session-end/pre-compact: no session id → no server writes", async () => {
  // A write tagged session_id:"unknown" shares one exclusion bucket with every
  // other unknown-id session (exact-match exclusion), so identity-less payloads
  // must not produce server writes — from any of the capture paths.
  const cache = freshCache();
  const tp = join(cache, "turn.jsonl");
  writeFileSync(
    tp,
    [
      JSON.stringify({ type: "user", message: { role: "user", content: "q" } }),
      JSON.stringify({
        type: "assistant",
        message: { id: "msg_1", content: [{ type: "text", text: "a\n<memory>\n{\"memories\":[{\"content\":\"fact\"}]}\n</memory>" }] },
      }),
    ].join("\n") + "\n",
  );
  const hits = [];
  const { url, close } = await startMockServer((req, res) => {
    hits.push({ url: req.url });
    res.setHeader("Content-Type", "application/json");
    res.statusCode = 201;
    res.end(JSON.stringify({ id: "m1" }));
  });
  try {
    // Buffer an event under the "unknown" fallback so each hook has a digest
    // it *would* write.
    await runHook(
      "post-tool-use.mjs",
      JSON.stringify({ cwd: __dirname, tool_name: "Edit", tool_input: { file_path: "x.go" } }),
      { XDG_CACHE_HOME: cache },
    );
    for (const script of ["stop.mjs", "pre-compact.mjs", "session-end.mjs"]) {
      await runHook(script, JSON.stringify({ cwd: __dirname, transcript_path: tp }), {
        MEMINI_URL: url,
        XDG_CACHE_HOME: cache,
      });
    }
    assert.equal(hits.length, 0, `identity-less payloads must not write, got ${JSON.stringify(hits)}`);
  } finally {
    await close();
  }
});

test("postJSON/getJSON: HTTP errors are logged even without MEMINI_DEBUG", async () => {
  // A swallowed 401/500 on a capture or recall looks like "memory isn't
  // working" with nothing to debug; the degrade path must say why by default.
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
    assert.ok(
      logged.some((m) => m.includes("POST /v1/memories -> 500")),
      `expected a POST failure log, got: ${JSON.stringify(logged)}`,
    );
    assert.ok(
      logged.some((m) => m.includes("GET /v1/memories -> 500")),
      `expected a GET failure log, got: ${JSON.stringify(logged)}`,
    );
  } finally {
    console.error = realError;
    if (prevUrl === undefined) delete process.env.MEMINI_BASE_URL;
    else process.env.MEMINI_BASE_URL = prevUrl;
    if (prevDebug !== undefined) process.env.MEMINI_DEBUG = prevDebug;
    await close();
  }
});

// --- session digest: failed-command surfacing -----------------------------

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
  assert.ok(
    !d.content.includes("./bin/protoc --go_out=. (failed)"),
    "the recovery command must not be marked failed",
  );
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
