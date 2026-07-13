import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { deriveLocalNamespace, repoNameFromRemote, repoSlugFromRemote, type LocalSource } from "../src/resolve.js";
import type { ProjectFacts } from "../src/facts.js";

// This fixture is the shared contract the actual derivation logic is built
// against across clients (Go internal/nsresolve, this package, Python
// integration tests) — see packages/memini-client/test/fixtures/derivation-vectors.json.
// The consumer tests below now exercise deriveLocalNamespace against it; this
// block additionally guards the fixture's own shape: every derivation case
// must carry facts+expect, every expected source must be a real
// namespace_source value, and every canonical_remote expectation must already
// be in its normalized form.

// __dirname isn't available under ESM; recover it from import.meta.url. The
// fixture lives under the SOURCE test/ tree (test/fixtures/...), not dist/ —
// tsc only compiles .ts files, so from the compiled dist/test/ location this
// walks back up to the package root and into test/fixtures.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const fixturePath = path.join(__dirname, "..", "..", "test", "fixtures", "derivation-vectors.json");

// Mirrors HandshakeResponse.namespace_source in api/openapi.yaml.
const NAMESPACE_SOURCES = new Set([
  "pin",
  "env",
  "declared",
  "remote",
  "toplevel",
  "cwd",
  "key_default",
  "server_default",
]);

function loadFixture(): {
  description: string;
  derivation: Array<{ name: string; facts: unknown; scope?: string; expect: { namespace: string; source: string } }>;
  canonical_remote: Array<{ input: string; expect: string }>;
} {
  const raw = fs.readFileSync(fixturePath, "utf8");
  return JSON.parse(raw);
}

test("derivation-vectors fixture has the documented top-level shape", () => {
  const data = loadFixture();
  assert.equal(typeof data.description, "string");
  assert.ok(data.description.length > 0);
  assert.ok(Array.isArray(data.derivation) && data.derivation.length > 0, "derivation must be a non-empty array");
  assert.ok(
    Array.isArray(data.canonical_remote) && data.canonical_remote.length > 0,
    "canonical_remote must be a non-empty array",
  );
});

test("every derivation case has facts+expect, and expect.source is a real namespace_source value", () => {
  const data = loadFixture();
  for (const c of data.derivation) {
    assert.equal(typeof c.name, "string", "case is missing a name");
    assert.ok(c.name.length > 0, "case name must not be empty");
    assert.ok(
      c.facts !== null && typeof c.facts === "object" && !Array.isArray(c.facts),
      `${c.name}: facts must be an object`,
    );
    assert.ok(
      c.expect !== null && typeof c.expect === "object" && !Array.isArray(c.expect),
      `${c.name}: expect must be an object`,
    );
    assert.equal(typeof c.expect.namespace, "string", `${c.name}: expect.namespace must be a string`);
    assert.ok(c.expect.namespace.length > 0, `${c.name}: expect.namespace must not be empty`);
    assert.equal(typeof c.expect.source, "string", `${c.name}: expect.source must be a string`);
    assert.ok(
      NAMESPACE_SOURCES.has(c.expect.source),
      `${c.name}: expect.source ${JSON.stringify(c.expect.source)} is not a namespace_source enum value`,
    );
  }
});

test("every canonical_remote expectation is already lowercase and scheme-free", () => {
  const data = loadFixture();
  for (const c of data.canonical_remote) {
    assert.equal(typeof c.input, "string", "canonical_remote case is missing input");
    assert.equal(typeof c.expect, "string", `${c.input}: expect must be a string`);
    assert.ok(c.expect.length > 0, `${c.input}: expect must not be empty`);
    assert.equal(c.expect, c.expect.toLowerCase(), `${c.input}: expect must be lowercase`);
    assert.ok(!/^[a-z][a-z0-9+.-]*:\/\//.test(c.expect), `${c.input}: expect must not carry a URL scheme`);
    assert.ok(!c.expect.includes("@"), `${c.input}: expect must not carry user-info`);
    assert.ok(!c.expect.endsWith("/"), `${c.input}: expect must not carry a trailing slash`);
    assert.ok(!c.expect.toLowerCase().endsWith(".git"), `${c.input}: expect must not carry a .git suffix`);
  }
});

// ─── the actual cross-language parity gate ─────────────────────────
//
// Everything above only guards the fixture's own shape. These tests are the
// real consumer: every derivation case run through THIS package's
// deriveLocalNamespace must land on the exact namespace+source the fixture
// says, since Go's internal/nsresolve.TestDerivationVectors runs the SAME
// cases through its own Resolve — the two languages can never quietly drift
// apart without one of these failing.

test("deriveLocalNamespace matches every derivation case in the fixture", () => {
  const data = loadFixture();
  for (const c of data.derivation) {
    const rawFacts = c.facts as Record<string, string | undefined>;
    const facts: ProjectFacts = {
      cwd_basename: rawFacts.cwd_basename ?? "",
      remote_url: rawFacts.remote_url,
      toplevel_path: rawFacts.toplevel_path,
      toplevel_basename: rawFacts.toplevel_basename,
      agent: rawFacts.agent,
      env_namespace: rawFacts.env_namespace,
      declared_namespace: rawFacts.declared_namespace,
    };
    const scope = c.scope === "owner_repo" ? "owner_repo" : "repo";

    const got = deriveLocalNamespace(facts, scope);
    assert.equal(got.namespace, c.expect.namespace, `${c.name}: namespace`);
    assert.equal(got.source, c.expect.source as LocalSource, `${c.name}: source`);
  }
});

test("repoNameFromRemote/repoSlugFromRemote match the remote-sourced fixture cases directly", () => {
  const data = loadFixture();
  for (const c of data.derivation) {
    if (c.expect.source !== "remote") continue;
    const remote = (c.facts as { remote_url?: string }).remote_url;
    const scope = c.scope === "owner_repo" ? "owner_repo" : "repo";
    const got = scope === "owner_repo" ? repoSlugFromRemote(remote) : repoNameFromRemote(remote);
    // The fixture's expected namespace may carry an agent suffix
    // ("phoenix/reviewer"); the bare remote-derived name is always its
    // first path segment.
    const base = c.expect.namespace.split("/")[0];
    assert.equal(got, base, `${c.name}: remote-derived base name`);
  }
});
