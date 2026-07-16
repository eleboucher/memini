import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { isSensitive, redactValue } from "../src/redact.js";
import { normalizeNamespace, validateNamespace } from "../src/namespace-validate.js";
import { readOverride, overrideKey } from "../src/override.js";
import {
  looksLikePluginRoot,
  resolveHarnessCwd,
  writeSessionCwd,
  readSessionCwd,
  deleteSessionCwd,
  SESSION_CWD_TTL_MS,
} from "../src/session.js";
import { BEHAVIOR_KNOBS, effectiveSetting, type BehaviorKnob } from "../src/settings.js";
import { DEFAULT_TIMEOUT_MS, MIN_TIMEOUT_MS } from "../src/bootstrap.js";

function tmp(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "memini-client-"));
}

// ─── redaction ──────────────────────────────────────────────────────

test("isSensitive catches credentials but not the api-keys FILE path", () => {
  for (const n of ["MEMINI_API_KEY", "MEMINI_TOKEN", "MEMINI_MCP_BEARER", "MEMINI_POSTGRES_DSN"]) {
    assert.equal(isSensitive(n), true, n);
  }
  // A path is not a credential; redacting it would hide useful diagnostics.
  assert.equal(isSensitive("MEMINI_API_KEYS_FILE"), false);
  assert.equal(isSensitive("MEMINI_BASE_URL"), false);
});

test("redactValue elides short secrets entirely rather than half-revealing them", () => {
  assert.equal(redactValue("short"), "***");
  assert.equal(redactValue("123456789012"), "***"); // exactly 12 — still elided
  assert.equal(redactValue("sk-abcdefghijklmnop4f2a"), "sk-…4f2a");
});

// ─── namespace validation ───────────────────────────────────────────

test("normalizeNamespace matches the server's canonical form", () => {
  assert.equal(normalizeNamespace("  work//memini/ "), "work/memini");
  assert.equal(normalizeNamespace("/a///b//"), "a/b");
});

test("validateNamespace rejects header-injection payloads", () => {
  // The namespace rides on the X-Memini-Namespace header. CR/LF would split it.
  assert.match(validateNamespace("a\r\nX-Evil: 1")!, /newline/);
  assert.match(validateNamespace("a\nb")!, /newline/);
  assert.match(validateNamespace("a\x00b")!, /control character/);
  assert.match(validateNamespace("a\tb")!, /control character/);
  assert.match(validateNamespace("café")!, /non-ASCII/);
  assert.match(validateNamespace("")!, /empty/);
  assert.match(validateNamespace("x".repeat(257))!, /256 bytes/);
  assert.equal(validateNamespace("acme/phoenix/api"), null);
});

// ─── override (read path only — write path removed, see override.ts) ─

test("a malformed overrides file degrades to no override instead of throwing", () => {
  const p = path.join(tmp(), "overrides.json");
  fs.writeFileSync(p, "{ not json");
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));
  assert.equal(readOverride(cwd, { overridesPath: p }), undefined);
});

test("overrideKey is stable across subdirectories of one repo", () => {
  // In a non-git tmp dir the key is just the resolved path, so assert the
  // property that matters: the key is absolute and normalized.
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));
  assert.equal(overrideKey(cwd), path.resolve(cwd));
  assert.equal(path.isAbsolute(overrideKey(cwd)), true);
});

// ─── session / project-dir recovery ────────────────────────────────

test("looksLikePluginRoot rejects the plugin's own install dir", () => {
  const env = { CLAUDE_PLUGIN_ROOT: "/home/u/.claude/plugins/cache/memini/memini/0.6.7" };
  assert.equal(looksLikePluginRoot("/home/u/.claude/plugins/cache/memini/memini/0.6.7", env), true);
  // Even without the env var, anything under a plugin cache is never a project.
  assert.equal(looksLikePluginRoot("/home/u/.claude/plugins/cache/x/y/1.0.0", {}), true);
  assert.equal(looksLikePluginRoot("/home/u/repos/memini", env), false);
});

test("resolveHarnessCwd prefers CLAUDE_PROJECT_DIR, then falls back to the session file", () => {
  const cacheHome = tmp();
  const proj = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));
  const env = { XDG_CACHE_HOME: cacheHome };

  // A pid above Linux's default pid_max (4194304), so the parent-process branch
  // cannot accidentally match a live process and mask the fallback under test.
  const DEAD_PID = 2147483646;

  // Nothing recorded and no such process -> undefined, rather than a guess.
  assert.equal(resolveHarnessCwd(env, DEAD_PID), undefined);

  // The portable fallback: a hook recorded the project dir under the shared ppid.
  writeSessionCwd(DEAD_PID, proj, env);
  assert.equal(readSessionCwd(DEAD_PID, env), path.resolve(proj));
  const viaFile = resolveHarnessCwd(env, DEAD_PID);
  assert.equal(viaFile?.cwd, path.resolve(proj));
  assert.equal(viaFile?.source, "session-file");

  // An explicit CLAUDE_PROJECT_DIR outranks it.
  const viaEnv = resolveHarnessCwd({ ...env, CLAUDE_PROJECT_DIR: proj }, DEAD_PID);
  assert.equal(viaEnv?.source, "CLAUDE_PROJECT_DIR");
});

test("an expired session record is refused, because a recycled pid is worse than none", () => {
  // A pid is not a durable identity. On Windows there is no /proc and no lsof, so
  // this record is the ONLY mechanism — and Windows recycles pids quickly. If a
  // session crashes (a clean exit deletes its record) and the OS later hands the
  // same pid to an unrelated session in a different repo, an unchecked record
  // would confidently hand that session the OLD repo's directory. Its MCP calls
  // would target one namespace while its hooks wrote to another: exactly the
  // split this module exists to prevent.
  //
  // Note fs.existsSync is NOT a freshness check — the old repo still exists on
  // disk. It is just the wrong repo.
  const env = { XDG_CACHE_HOME: tmp() };
  const oldRepo = fs.mkdtempSync(path.join(os.tmpdir(), "repo-a-"));
  const DEAD_PID = 2147483645;

  const t0 = 1_000_000_000_000;
  writeSessionCwd(DEAD_PID, oldRepo, env, t0);

  // Fresh: honored.
  assert.equal(readSessionCwd(DEAD_PID, env, t0 + 1000), path.resolve(oldRepo));
  // Just inside the window: still honored, so a live session never expires.
  assert.equal(readSessionCwd(DEAD_PID, env, t0 + SESSION_CWD_TTL_MS - 1), path.resolve(oldRepo));
  // Past it: refused, even though the directory still exists.
  assert.equal(readSessionCwd(DEAD_PID, env, t0 + SESSION_CWD_TTL_MS + 1), undefined);
  // And resolveHarnessCwd therefore declines to guess rather than answering wrong.
  assert.equal(resolveHarnessCwd(env, DEAD_PID), undefined);
});

test("a clock skewed into the future is treated as untrustworthy, not as fresh", () => {
  const env = { XDG_CACHE_HOME: tmp() };
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), "repo-"));
  const DEAD_PID = 2147483644;
  const t0 = 1_000_000_000_000;

  writeSessionCwd(DEAD_PID, repo, env, t0);
  // Reading at a time BEFORE the record was written means the clock moved. A
  // negative age would otherwise sail through an `age > TTL` check.
  assert.equal(readSessionCwd(DEAD_PID, env, t0 - 1000), undefined);
});

test("deleteSessionCwd closes the pid-reuse window on a clean exit", () => {
  const env = { XDG_CACHE_HOME: tmp() };
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), "repo-"));
  const DEAD_PID = 2147483643;

  writeSessionCwd(DEAD_PID, repo, env);
  assert.equal(readSessionCwd(DEAD_PID, env), path.resolve(repo));

  deleteSessionCwd(DEAD_PID, env);
  assert.equal(readSessionCwd(DEAD_PID, env), undefined);
  // Deleting again is a no-op, not a throw: SessionEnd must never fail the agent.
  deleteSessionCwd(DEAD_PID, env);
});

test("a corrupt session record degrades to no record rather than throwing", () => {
  const env = { XDG_CACHE_HOME: tmp() };
  const DEAD_PID = 2147483642;
  const p = path.join(env.XDG_CACHE_HOME, "memini", "sessions", `pid-${DEAD_PID}.cwd`);
  fs.mkdirSync(path.dirname(p), { recursive: true });

  for (const junk of ["{ not json", "", '{"cwd":"/nope"}', '{"writtenAt":123}']) {
    fs.writeFileSync(p, junk);
    assert.equal(readSessionCwd(DEAD_PID, env), undefined, `junk: ${JSON.stringify(junk)}`);
  }
});

test("a live parent process outranks a stale session file", () => {
  // Ordering that matters: the session file is written once per session, but the
  // parent's cwd is always current. Preferring the live parent is what keeps a
  // freshly-set namespace override from being masked by a stale cached value.
  if (process.platform !== "linux" && process.platform !== "darwin") return;

  const env = { XDG_CACHE_HOME: tmp() };
  const stale = fs.mkdtempSync(path.join(os.tmpdir(), "stale-"));
  writeSessionCwd(process.ppid, stale, env);

  const got = resolveHarnessCwd(env, process.ppid);
  assert.equal(got?.source, "parent-process");
  assert.notEqual(got?.cwd, path.resolve(stale));
});

test("resolveHarnessCwd reads the real parent process cwd", () => {
  // process.ppid is whatever spawned the test runner; its cwd must be readable
  // on Linux/macOS. This is the mechanism the headersHelper depends on.
  const got = resolveHarnessCwd({ XDG_CACHE_HOME: tmp() }, process.ppid);
  if (process.platform === "linux" || process.platform === "darwin") {
    assert.ok(got, "expected the parent process cwd to be recoverable");
    assert.equal(got!.source, "parent-process");
    assert.equal(path.isAbsolute(got!.cwd), true);
  }
});

// ─── behavior knobs / effectiveSetting ──────────────────────────────

test("BEHAVIOR_KNOBS covers exactly the 26 behavioral ClientSettings fields, excluding namespace_scope/namespace_prefix", () => {
  assert.equal(BEHAVIOR_KNOBS.length, 26);
  const wireKeys = BEHAVIOR_KNOBS.map((k) => k.wireKey);
  assert.equal(new Set(wireKeys).size, wireKeys.length, "wireKey must be unique per knob");
  assert.equal(wireKeys.includes("namespace_scope"), false);
  assert.equal(wireKeys.includes("namespace_prefix"), false);
  // The capture bounds are server-resolved settings like any other knob — they
  // were once literals inlined in each integration's capture hook.
  assert.ok(wireKeys.includes("capture_user_max_chars"));
  assert.ok(wireKeys.includes("capture_assistant_max_chars"));
  const envNames = BEHAVIOR_KNOBS.map((k) => k.envName);
  assert.equal(new Set(envNames).size, envNames.length, "envName must be unique per knob");
});

test("inject_dedupe knob: bool, default true, MEMINI_INJECT_DEDUPE=0 overrides a server true", () => {
  const k = BEHAVIOR_KNOBS.find((k) => k.wireKey === "inject_dedupe");
  assert.ok(k, "inject_dedupe must be a behavior knob");
  assert.equal(k!.envName, "MEMINI_INJECT_DEDUPE");
  assert.equal(k!.kind, "bool");
  assert.equal(k!.default, true);
  assert.deepEqual(effectiveSetting(k!, undefined, {}), { value: true, source: "default" });
  assert.deepEqual(effectiveSetting(k!, { inject_dedupe: true }, { MEMINI_INJECT_DEDUPE: "0" }), {
    value: false,
    source: "env-override",
  });
});

test("auto_save_min_events knob: int, default 3, MEMINI_AUTO_SAVE_MIN_EVENTS overrides a server value", () => {
  const k = BEHAVIOR_KNOBS.find((k) => k.wireKey === "auto_save_min_events");
  assert.ok(k, "auto_save_min_events must be a behavior knob");
  assert.equal(k!.envName, "MEMINI_AUTO_SAVE_MIN_EVENTS");
  assert.equal(k!.kind, "int");
  assert.equal(k!.default, 3);
  assert.deepEqual(effectiveSetting(k!, undefined, {}), { value: 3, source: "default" });
  assert.deepEqual(effectiveSetting(k!, { auto_save_min_events: 5 }, { MEMINI_AUTO_SAVE_MIN_EVENTS: "0" }), {
    value: 0,
    source: "env-override",
  });
});

// request_timeout_ms is the one knob that is ALSO read at the transport layer
// (bootstrap.ts), because a request timeout has to exist before the handshake
// that fetches settings. Both spellings must agree on the same default and the
// same floor, or "raise the timeout" would mean two different things depending
// on which layer answered.
test("request_timeout_ms knob: int, agrees with the transport layer, env overrides a server value", () => {
  const k = BEHAVIOR_KNOBS.find((k) => k.wireKey === "request_timeout_ms");
  assert.ok(k, "request_timeout_ms must be a behavior knob");
  assert.equal(k!.envName, "MEMINI_TIMEOUT_MS");
  assert.equal(k!.kind, "int");
  assert.equal(k!.default, DEFAULT_TIMEOUT_MS, "the knob and bootstrap must share one default");
  assert.equal(k!.min, MIN_TIMEOUT_MS, "the knob and bootstrap must share one floor");

  assert.deepEqual(effectiveSetting(k!, undefined, {}), {
    value: DEFAULT_TIMEOUT_MS,
    source: "default",
  });
  // The whole point of the server layer: an admin running a slow cross-encoder
  // raises the ceiling once, and every client that handshakes picks it up.
  assert.deepEqual(effectiveSetting(k!, { request_timeout_ms: 60000 }, {}), {
    value: 60000,
    source: "server",
  });
  // ...and a user can still override it locally.
  assert.deepEqual(effectiveSetting(k!, { request_timeout_ms: 60000 }, { MEMINI_TIMEOUT_MS: "45000" }), {
    value: 45000,
    source: "env-override",
  });
  // A below-floor override is raised to the floor, not silently swapped for the
  // default — matching envTimeoutMs and the schema's `minimum`.
  assert.deepEqual(effectiveSetting(k!, undefined, { MEMINI_TIMEOUT_MS: "50" }), {
    value: MIN_TIMEOUT_MS,
    source: "env-override",
  });
});

function knob(wireKey: string): BehaviorKnob {
  const k = BEHAVIOR_KNOBS.find((k) => k.wireKey === wireKey);
  assert.ok(k, `no BEHAVIOR_KNOBS entry for ${wireKey}`);
  return k!;
}

test("effectiveSetting: bool — env overrides server overrides default, with envEnabled-style falsy forms", () => {
  const k = knob("capture_turns");
  assert.deepEqual(effectiveSetting(k, undefined, {}), { value: true, source: "default" });
  assert.deepEqual(effectiveSetting(k, { capture_turns: false }, {}), { value: false, source: "server" });
  assert.deepEqual(effectiveSetting(k, { capture_turns: false }, { MEMINI_CAPTURE_TURNS: "1" }), {
    value: true,
    source: "env-override",
  });
  for (const v of ["0", "false", "no", "off", "OFF"]) {
    assert.deepEqual(effectiveSetting(k, undefined, { MEMINI_CAPTURE_TURNS: v }), {
      value: false,
      source: "env-override",
    });
  }
});

test("effectiveSetting: int — env parses, server value used absent an env override, else default", () => {
  const k = knob("auto_save_interval");
  assert.deepEqual(effectiveSetting(k, undefined, {}), { value: 10, source: "default" });
  assert.deepEqual(effectiveSetting(k, { auto_save_interval: 25 }, {}), { value: 25, source: "server" });
  assert.deepEqual(effectiveSetting(k, { auto_save_interval: 25 }, { MEMINI_AUTO_SAVE_INTERVAL: "5" }), {
    value: 5,
    source: "env-override",
  });
});

test("effectiveSetting: float — parses like floatEnv (>=0, else fallback)", () => {
  const k = knob("inject_pretool_min_score");
  assert.deepEqual(effectiveSetting(k, undefined, { MEMINI_INJECT_PRETOOL_MIN_SCORE: "0.65" }), {
    value: 0.65,
    source: "env-override",
  });
  assert.deepEqual(effectiveSetting(k, { inject_pretool_min_score: 0.2 }, {}), {
    value: 0.2,
    source: "server",
  });
});

test("effectiveSetting: list — pipe/comma separated, trimmed and lowercased, like listEnv", () => {
  const k = knob("inject_pretool_tools");
  assert.deepEqual(effectiveSetting(k, undefined, { MEMINI_INJECT_PRETOOL_TOOLS: "Read|Write, Edit" }), {
    value: ["read", "write", "edit"],
    source: "env-override",
  });
  assert.deepEqual(effectiveSetting(k, undefined, {}), {
    value: ["Read", "Write", "Edit", "Glob", "Grep"],
    source: "default",
  });
});

test("effectiveSetting: an empty-string env var does not count as an override", () => {
  const k = knob("recall_limit");
  assert.deepEqual(effectiveSetting(k, { recall_limit: 8 }, { MEMINI_RECALL_LIMIT: "" }), {
    value: 8,
    source: "server",
  });
});
