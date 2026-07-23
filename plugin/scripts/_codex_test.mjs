#!/usr/bin/env node
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");
const fixtures = path.join(root, "test", "fixtures", "codex");

function fixture(name) {
  return fs.readFileSync(path.join(fixtures, name), "utf8");
}

function hook(script, payload, extra = {}) {
  const cache = fs.mkdtempSync(path.join(os.tmpdir(), "memini-codex-"));
  return spawnSync(process.execPath, [path.join(root, "scripts", script)], {
    input: payload,
    encoding: "utf8",
    cwd: root,
    env: {
      ...process.env,
      PLUGIN_ROOT: root,
      XDG_CACHE_HOME: cache,
      MEMINI_BASE_URL: "http://127.0.0.1:1",
      MEMINI_TIMEOUT_MS: "50",
      MEMINI_API_KEY: "super-secret-test-token",
      ...extra,
    },
  });
}

test("Codex startup and compact SessionStart use native context envelopes", () => {
  for (const name of ["session-start-startup.json", "session-start-compact.json"]) {
    const r = hook("session-start.mjs", fixture(name));
    assert.equal(r.status, 0);
    assert.ok(r.stdout, r.stderr);
    const out = JSON.parse(r.stdout);
    assert.equal(out.hookSpecificOutput.hookEventName, "SessionStart");
    assert.match(out.hookSpecificOutput.additionalContext, /memini-(?:memory-directive|compact-recovery)/);
    assert.doesNotMatch(r.stdout + r.stderr, /super-secret-test-token/);
  }
});

test("Codex apply_patch and Bash payloads buffer without leaking secrets", () => {
  const cache = fs.mkdtempSync(path.join(os.tmpdir(), "memini-codex-tools-"));
  const env = { XDG_CACHE_HOME: cache };
  for (const name of ["pre-tool-apply-patch.json", "post-tool-bash.json"]) {
    const script = name.startsWith("pre-") ? "pre-tool-use.mjs" : "post-tool-use.mjs";
    const r = hook(script, fixture(name), env);
    assert.equal(r.status, 0);
    assert.doesNotMatch(r.stdout + r.stderr, /super-secret-test-token/);
  }
});

test("Codex prompt recall and PreCompact fail soft with valid outputs", () => {
  const prompt = hook("user-prompt-submit.mjs", fixture("user-prompt.json"));
  assert.equal(prompt.status, 0);
  assert.equal(prompt.stdout, "");
  const compact = hook("pre-compact.mjs", fixture("pre-compact.json"));
  assert.equal(compact.status, 0);
  assert.deepEqual(JSON.parse(compact.stdout), {});
  assert.doesNotMatch(prompt.stderr + compact.stderr, /super-secret-test-token/);
});

test("Codex Stop ignores transcript paths and always returns valid JSON", () => {
  const r = hook("stop.mjs", fixture("stop.json"));
  assert.equal(r.status, 0);
  assert.ok(r.stdout, r.stderr);
  assert.deepEqual(JSON.parse(r.stdout), {});
  assert.doesNotMatch(r.stdout + r.stderr, /super-secret-test-token|must\/not\/be\/read/);
});

test("Codex wiring is documented-event-only and Claude retains SessionEnd", () => {
  const codex = JSON.parse(fs.readFileSync(path.join(root, "hooks", "hooks.json")));
  const claude = JSON.parse(fs.readFileSync(path.join(root, "hooks", "hooks.claude.json")));
  assert.deepEqual(Object.keys(codex.hooks).sort(), [
    "PostToolUse", "PreCompact", "PreToolUse", "SessionStart", "Stop", "UserPromptSubmit",
  ].sort());
  assert.ok(claude.hooks.SessionEnd);
  assert.match(JSON.stringify(codex), /PLUGIN_ROOT/);
});

test("all nine skills are installed with required safety invariants", () => {
  const dirs = fs.readdirSync(path.join(root, "skills")).sort();
  assert.deepEqual(dirs, ["backfill", "doctor", "forget", "namespace", "pin", "recall", "recap", "remember", "status"]);
  assert.match(fs.readFileSync(path.join(root, "skills", "forget", "SKILL.md"), "utf8"), /explicit user\s+confirmation/i);
  assert.match(fs.readFileSync(path.join(root, "skills", "pin", "SKILL.md"), "utf8"), /existing tags plus/);
  for (const name of ["doctor", "backfill"])
    assert.match(fs.readFileSync(path.join(root, "skills", name, "SKILL.md"), "utf8"), /local memini binary/i);
});
