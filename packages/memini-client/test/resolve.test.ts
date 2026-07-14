import assert from "node:assert/strict";
import { test } from "node:test";

import { deriveLocalNamespace, resolveNamespace } from "../src/resolve.js";
import { readBootstrap } from "../src/bootstrap.js";
import type { ProjectFacts } from "../src/facts.js";
import type { HandshakeResult } from "../src/handshake.js";

// deriveLocalNamespace's exact per-case behavior is vector-tested against
// test/fixtures/derivation-vectors.json in derivation-vectors.test.ts. These
// cover the one case the fixture does not (there being truly nothing to
// derive from) plus resolveNamespace, which sits a layer above it.

test("deriveLocalNamespace: falls back to the literal namespace 'default' when there is nothing at all", () => {
  const facts: ProjectFacts = { cwd_basename: "" };
  const got = deriveLocalNamespace(facts);
  assert.deepEqual(got, { namespace: "default", source: "default" });
});

test("deriveLocalNamespace: the 'default' fallback carries no agent suffix — there is no base to nest under", () => {
  // Sanity check on ordering: the "default" branch returns before withAgent
  // ever runs, since there is no base to nest anything under.
  const facts: ProjectFacts = { cwd_basename: "", agent: "reviewer" };
  const got = deriveLocalNamespace(facts);
  assert.deepEqual(got, { namespace: "default", source: "default" });
});

// ─── resolveNamespace ───────────────────────────────────────────────

function fakeHandshake(overrides: Partial<HandshakeResult> = {}): HandshakeResult {
  return {
    namespace: "acme/phoenix",
    namespace_source: "pin",
    identity: { authenticated: true },
    settings: {},
    settings_sources: {},
    read_set: [],
    server: { version: "0.0.0-test", default_namespace: "default" },
    ...overrides,
  };
}

test("resolveNamespace: a successful handshake wins outright, not degraded", () => {
  const boot = readBootstrap({ MEMINI_NAMESPACE: "should-be-ignored" });
  const facts: ProjectFacts = { cwd_basename: "adhoc-dir" };
  const hs = fakeHandshake();

  const got = resolveNamespace(boot, facts, hs);
  assert.equal(got.namespace, "acme/phoenix");
  assert.equal(got.source, "server:pin");
  assert.equal(got.degraded, false);
});

test("resolveNamespace: server: prefix carries whichever namespace_source the server reported", () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { cwd_basename: "adhoc-dir" };
  const hs = fakeHandshake({ namespace_source: "server_default", namespace: "default" });

  const got = resolveNamespace(boot, facts, hs);
  assert.equal(got.source, "server:server_default");
  assert.equal(got.degraded, false);
});

test("resolveNamespace: no handshake — MEMINI_NAMESPACE wins, degraded", () => {
  const boot = readBootstrap({ MEMINI_NAMESPACE: "pinned-env" });
  const facts: ProjectFacts = { cwd_basename: "adhoc-dir" };

  const got = resolveNamespace(boot, facts, undefined);
  assert.equal(got.namespace, "pinned-env");
  assert.equal(got.source, "env");
  assert.equal(got.degraded, true);
});

test("resolveNamespace: no handshake, no env pin — local derivation, prefixed and degraded", () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { remote_url: "https://github.com/acme/phoenix.git", cwd_basename: "ignored" };

  const got = resolveNamespace(boot, facts, undefined);
  assert.equal(got.namespace, "phoenix");
  assert.equal(got.source, "local-remote");
  assert.equal(got.degraded, true);
});

test("resolveNamespace: local derivation all the way down to 'default' is still reported as local-default", () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { cwd_basename: "" };

  const got = resolveNamespace(boot, facts, undefined);
  assert.equal(got.namespace, "default");
  assert.equal(got.source, "local-default");
  assert.equal(got.degraded, true);
});

test("resolveNamespace: MEMINI_NAMESPACE_PREFIX prepends to a derived name offline", () => {
  const boot = readBootstrap({ MEMINI_NAMESPACE_PREFIX: "work" });
  const facts: ProjectFacts = { remote_url: "https://github.com/acme/phoenix.git", cwd_basename: "ignored" };

  const got = resolveNamespace(boot, facts, undefined);
  assert.equal(got.namespace, "work/phoenix");
  assert.equal(got.source, "local-remote");
  assert.equal(got.degraded, true);
});

test("resolveNamespace: MEMINI_NAMESPACE_PREFIX never prefixes the 'default' fallback", () => {
  const boot = readBootstrap({ MEMINI_NAMESPACE_PREFIX: "work" });
  const facts: ProjectFacts = { cwd_basename: "" };

  const got = resolveNamespace(boot, facts, undefined);
  assert.equal(got.namespace, "default");
  assert.equal(got.source, "local-default");
});

test("resolveNamespace: MEMINI_NAMESPACE (verbatim) beats the prefix — the prefix only shapes derivation", () => {
  const boot = readBootstrap({ MEMINI_NAMESPACE: "pinned-env", MEMINI_NAMESPACE_PREFIX: "work" });
  const facts: ProjectFacts = { remote_url: "https://github.com/acme/phoenix.git", cwd_basename: "ignored" };

  const got = resolveNamespace(boot, facts, undefined);
  assert.equal(got.namespace, "pinned-env");
  assert.equal(got.source, "env");
});
