import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { isSensitive, redactValue } from "../src/redact.js";
import { normalizeNamespace, validateNamespace } from "../src/namespace-validate.js";
import { clearOverride, readOverride, writeOverride, overrideKey } from "../src/override.js";
import {
  looksLikePluginRoot,
  resolveHarnessCwd,
  writeSessionCwd,
  readSessionCwd,
  deleteSessionCwd,
  SESSION_CWD_TTL_MS,
} from "../src/session.js";
import { describeSettings, type ResolvedNamespace } from "../src/settings.js";

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

// ─── override ───────────────────────────────────────────────────────

test("override round-trips and clears", () => {
  const dir = tmp();
  const p = path.join(dir, "overrides.json");
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));

  assert.equal(readOverride(cwd, { overridesPath: p }), undefined);

  const written = writeOverride(cwd, "atvik", { overridesPath: p });
  assert.equal(written.namespace, "atvik");
  assert.equal(readOverride(cwd, { overridesPath: p })?.namespace, "atvik");

  assert.equal(clearOverride(cwd, { overridesPath: p }), true);
  assert.equal(readOverride(cwd, { overridesPath: p }), undefined);
  // Clearing a second time is a no-op, not an error.
  assert.equal(clearOverride(cwd, { overridesPath: p }), false);
});

test("writeOverride normalizes and refuses an invalid namespace", () => {
  const p = path.join(tmp(), "overrides.json");
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));

  assert.equal(writeOverride(cwd, " work//x/ ", { overridesPath: p }).namespace, "work/x");
  assert.throws(() => writeOverride(cwd, "bad\r\nInject: 1", { overridesPath: p }), /newline/);
  assert.throws(() => writeOverride(cwd, "   ", { overridesPath: p }), /empty/);
});

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

// ─── settings ───────────────────────────────────────────────────────

/** A stand-in harness resolver: env pin wins, else a fixed "git-derived" name. */
function fakeResolver(env: Record<string, string | undefined>): ResolvedNamespace {
  const pin = (env["MEMINI_NAMESPACE"] || "").trim();
  if (pin) return { namespace: pin, source: "env" };
  return { namespace: "memini", source: "git-remote" };
}

test("counterfactual lines see PAST an override, not through it", () => {
  // Regression: describeSettings computes "what would this be without X" by
  // re-resolving against a stripped environment — but the override lives in a
  // FILE, so env-stripping alone leaves it in place and `withoutOverride` reports
  // the override right back at you. A resolver that honors ignoreOverride (as the
  // real ones do) is the only thing that makes these two lines meaningful.
  const p = path.join(tmp(), "overrides.json");
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));
  writeOverride(cwd, "atvik", { overridesPath: p });

  // A resolver that behaves like the real one: consults the override unless told not to.
  const realistic = (
    env: Record<string, string | undefined>,
    o?: { ignoreOverride?: boolean },
  ): ResolvedNamespace => {
    if (!o?.ignoreOverride) {
      const ovr = readOverride(cwd, { env, overridesPath: p });
      if (ovr) return { namespace: ovr.namespace, source: "override" };
    }
    return fakeResolver(env);
  };

  const s = describeSettings({
    cwd,
    env: { MEMINI_NAMESPACE: "default", MEMINI_HOME: "personal/kit" },
    resolve: realistic,
    overridesPath: p,
  });

  assert.equal(s.namespace.effective, "atvik");
  // These must NOT be "atvik" — that would be the override reporting itself.
  assert.equal(s.namespace.withoutOverride.namespace, "default");
  assert.equal(s.namespace.derived.namespace, "memini");
});

test("describeSettings exposes a global env pin as the diagnosis, not just a value", () => {
  const p = path.join(tmp(), "overrides.json");
  const s = describeSettings({
    cwd: process.cwd(),
    env: { MEMINI_NAMESPACE: "default", MEMINI_HOME: "personal/kit" },
    resolve: fakeResolver,
    overridesPath: p,
  });

  assert.equal(s.namespace.effective, "default");
  assert.equal(s.namespace.source, "env");
  // The line that makes the trap visible: what it WOULD have been.
  assert.equal(s.namespace.derived.namespace, "memini");

  const pin = s.warnings.find((w) => w.code === "global-namespace-pin");
  assert.ok(pin, "expected a global-namespace-pin warning");
  assert.equal(pin!.level, "warn");
  assert.match(pin!.message, /EVERY project/);
});

test("an override beats a globally-pinned MEMINI_NAMESPACE", () => {
  // This is the whole point. If the env won, the namespace command would be a
  // no-op on exactly the machines that need it.
  const p = path.join(tmp(), "overrides.json");
  const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "proj-"));
  writeOverride(cwd, "atvik", { overridesPath: p });

  const s = describeSettings({
    cwd,
    env: { MEMINI_NAMESPACE: "default", MEMINI_HOME: "personal/kit" },
    resolve: fakeResolver,
    overridesPath: p,
  });

  assert.equal(s.namespace.effective, "atvik");
  assert.equal(s.namespace.source, "override");
  assert.equal(s.namespace.withoutOverride.namespace, "default");
  assert.ok(s.warnings.some((w) => w.code === "override-active"));
  // The pin warning is suppressed while an override is masking it — the
  // override IS the fix, so nagging about the pin would be noise.
  assert.equal(s.warnings.some((w) => w.code === "global-namespace-pin"), false);
});

test("describeSettings warns when MEMINI_HOME is unset and redacts the token", () => {
  const s = describeSettings({
    cwd: process.cwd(),
    env: { MEMINI_API_KEY: "sk-abcdefghijklmnop4f2a", MEMINI_BASE_URL: "https://m.example.com" },
    resolve: fakeResolver,
    overridesPath: path.join(tmp(), "overrides.json"),
  });

  assert.ok(s.warnings.some((w) => w.code === "home-unset"));

  const key = s.settings.find((k) => k.name === "MEMINI_API_KEY")!;
  assert.equal(key.value, "sk-…4f2a");
  assert.equal(key.sensitive, true);
  assert.equal(key.source, "env");

  // An unset knob still appears, carrying its default — "unset" is a finding.
  const home = s.settings.find((k) => k.name === "MEMINI_HOME")!;
  assert.equal(home.source, "default");
  assert.equal(home.isDefault, true);
});

test("a bearer token over plaintext HTTP to a remote host is a warning", () => {
  const base = {
    cwd: process.cwd(),
    resolve: fakeResolver,
    overridesPath: path.join(tmp(), "overrides.json"),
  };

  const remote = describeSettings({
    ...base,
    env: { MEMINI_API_KEY: "sk-abcdefghijklmnop", MEMINI_BASE_URL: "http://memini.example.com" },
  });
  assert.ok(remote.warnings.some((w) => w.code === "plaintext-bearer"));

  // Loopback is fine — the token never crosses a network.
  const local = describeSettings({
    ...base,
    env: { MEMINI_API_KEY: "sk-abcdefghijklmnop", MEMINI_BASE_URL: "http://localhost:8080" },
  });
  assert.equal(local.warnings.some((w) => w.code === "plaintext-bearer"), false);
});
