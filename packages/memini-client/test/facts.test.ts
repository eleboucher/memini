import assert from "node:assert/strict";
import { test } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { gatherFacts, factsFingerprint, type ProjectFacts } from "../src/facts.js";

function tmpRepo(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memini-facts-"));
  execFileSync("git", ["init", "-q"], { cwd: dir });
  execFileSync("git", ["config", "user.email", "t@example.com"], { cwd: dir });
  execFileSync("git", ["config", "user.name", "t"], { cwd: dir });
  return dir;
}

// ─── gatherFacts ────────────────────────────────────────────────────

test("gatherFacts: a bare (non-git) directory carries only cwd_basename", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memini-facts-bare-"));
  const facts = gatherFacts(dir, {});
  assert.equal(facts.cwd_basename, path.basename(dir));
  assert.equal(facts.remote_url, undefined);
  assert.equal(facts.toplevel_path, undefined);
  assert.equal(facts.toplevel_basename, undefined);
  assert.equal(facts.agent, undefined);
  assert.equal(facts.env_namespace, undefined);
  assert.equal(facts.declared_namespace, undefined);
});

test("gatherFacts: a git repo with no remote still reports the toplevel", () => {
  const dir = tmpRepo();
  const facts = gatherFacts(dir, {});
  assert.equal(facts.remote_url, undefined);
  assert.equal(facts.toplevel_basename, path.basename(fs.realpathSync(dir)));
  assert.equal(path.resolve(facts.toplevel_path!), fs.realpathSync(dir));
});

test("gatherFacts: a configured origin remote is reported verbatim", () => {
  const dir = tmpRepo();
  execFileSync("git", ["remote", "add", "origin", "https://github.com/acme/phoenix.git"], { cwd: dir });
  const facts = gatherFacts(dir, {});
  assert.equal(facts.remote_url, "https://github.com/acme/phoenix.git");
});

test("gatherFacts: never throws even in a directory that does not exist", () => {
  assert.doesNotThrow(() => gatherFacts("/no/such/directory/at/all", {}));
});

test("gatherFacts: populates agent/env_namespace from env, never declared_namespace", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memini-facts-env-"));
  const facts = gatherFacts(dir, { MEMINI_AGENT: "reviewer", MEMINI_NAMESPACE: "  acme/phoenix  " });
  assert.equal(facts.agent, "reviewer");
  assert.equal(facts.env_namespace, "acme/phoenix");
  assert.equal(facts.declared_namespace, undefined);
});

test("gatherFacts: blank MEMINI_AGENT/MEMINI_NAMESPACE leave the fields absent", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memini-facts-blank-"));
  const facts = gatherFacts(dir, { MEMINI_AGENT: "", MEMINI_NAMESPACE: "   " });
  assert.equal(facts.agent, undefined);
  assert.equal(facts.env_namespace, undefined);
});

// ─── factsFingerprint ───────────────────────────────────────────────

test("factsFingerprint: stable regardless of key insertion order", () => {
  const a: ProjectFacts = { cwd_basename: "x", remote_url: "https://github.com/acme/phoenix.git", agent: "reviewer" };
  const b: ProjectFacts = { agent: "reviewer", remote_url: "https://github.com/acme/phoenix.git", cwd_basename: "x" };
  assert.equal(factsFingerprint(a), factsFingerprint(b));
});

test("factsFingerprint: changes when any single fact changes", () => {
  const base: ProjectFacts = { cwd_basename: "x", remote_url: "https://github.com/acme/phoenix.git" };
  const changedRemote: ProjectFacts = { cwd_basename: "x", remote_url: "https://github.com/acme/other.git" };
  const changedCwd: ProjectFacts = { cwd_basename: "y", remote_url: "https://github.com/acme/phoenix.git" };
  const withAgent: ProjectFacts = { ...base, agent: "reviewer" };

  const baseHash = factsFingerprint(base);
  assert.notEqual(factsFingerprint(changedRemote), baseHash);
  assert.notEqual(factsFingerprint(changedCwd), baseHash);
  assert.notEqual(factsFingerprint(withAgent), baseHash);
});

test("factsFingerprint: identical facts always hash identically", () => {
  const f: ProjectFacts = { cwd_basename: "adhoc-dir" };
  assert.equal(factsFingerprint(f), factsFingerprint({ cwd_basename: "adhoc-dir" }));
});
