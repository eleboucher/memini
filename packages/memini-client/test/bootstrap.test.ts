import assert from "node:assert/strict";
import { test } from "node:test";

import {
  readBootstrap,
  assertBearerTransportSafe,
  isPlaintextBearerUnsafe,
  envEnabled,
} from "../src/bootstrap.js";

// ─── envEnabled ─────────────────────────────────────────────────────

test("envEnabled: default-on unless explicitly opted out, matching _shared.mjs semantics", () => {
  assert.equal(envEnabled(undefined, true), true, "unset -> default");
  assert.equal(envEnabled(undefined, false), false, "unset -> default");
  assert.equal(envEnabled("", true), true, "empty -> default");
  for (const v of ["0", "false", "no", "off", "OFF", " Off "]) {
    assert.equal(envEnabled(v, true), false, `${v} -> false`);
  }
  for (const v of ["1", "true", "yes", "on", "anything"]) {
    assert.equal(envEnabled(v, false), true, `${v} -> true`);
  }
});

// ─── readBootstrap ──────────────────────────────────────────────────

test("readBootstrap: defaults when nothing is set", () => {
  const b = readBootstrap({});
  assert.equal(b.baseUrl, "http://localhost:8080");
  assert.equal(b.apiKey, "");
  assert.equal(b.requireHttps, false);
  assert.equal(b.debug, false);
  assert.equal(b.agent, "");
  assert.equal(b.namespaceEnv, "");
  assert.equal(b.homeEnv, "");
});

test("readBootstrap: MEMINI_URL and MEMINI_TOKEN are NOT aliases — only the new names are read", () => {
  const b = readBootstrap({
    MEMINI_URL: "https://alias.example.com",
    MEMINI_TOKEN: "alias-secret",
  });
  assert.equal(b.baseUrl, "http://localhost:8080", "MEMINI_URL must be ignored");
  assert.equal(b.apiKey, "", "MEMINI_TOKEN must be ignored");
});

test("readBootstrap: reads the real vars, with the documented trim/raw semantics", () => {
  const b = readBootstrap({
    MEMINI_BASE_URL: "https://memini.example.com",
    MEMINI_API_KEY: "sk-real",
    MEMINI_REQUIRE_HTTPS: "1",
    MEMINI_DEBUG: "true",
    MEMINI_AGENT: "  reviewer  ",
    MEMINI_NAMESPACE: "  acme/phoenix  ",
    MEMINI_NAMESPACE_PREFIX: "  work  ",
    MEMINI_HOME: "  personal/kit  ",
  });
  assert.equal(b.baseUrl, "https://memini.example.com");
  assert.equal(b.apiKey, "sk-real");
  assert.equal(b.requireHttps, true);
  assert.equal(b.debug, true);
  // agent is raw — NOT trimmed here (sanitization happens where a namespace is built).
  assert.equal(b.agent, "  reviewer  ");
  // namespaceEnv/namespacePrefixEnv/homeEnv ARE trimmed.
  assert.equal(b.namespaceEnv, "acme/phoenix");
  assert.equal(b.namespacePrefixEnv, "work");
  assert.equal(b.homeEnv, "personal/kit");
});

// ─── isPlaintextBearerUnsafe ────────────────────────────────────────

test("isPlaintextBearerUnsafe: loopback is always fine, a blank secret is always fine", () => {
  assert.equal(isPlaintextBearerUnsafe("http://localhost:8080", "secret"), false);
  assert.equal(isPlaintextBearerUnsafe("http://127.0.0.1:8080", "secret"), false);
  assert.equal(isPlaintextBearerUnsafe("http://[::1]:8080", "secret"), false);
  assert.equal(isPlaintextBearerUnsafe("http://remote.example.com", ""), false);
  assert.equal(isPlaintextBearerUnsafe("not a url", "secret"), false);
});

test("isPlaintextBearerUnsafe: http + non-loopback + a secret is unsafe; https never is", () => {
  assert.equal(isPlaintextBearerUnsafe("http://remote.example.com", "secret"), true);
  assert.equal(isPlaintextBearerUnsafe("https://remote.example.com", "secret"), false);
});

// ─── assertBearerTransportSafe guard matrix ────────────────────────

test("assertBearerTransportSafe: loopback never throws, regardless of MEMINI_REQUIRE_HTTPS", () => {
  assert.doesNotThrow(() => assertBearerTransportSafe("http://localhost:8080", "secret", {}));
  assert.doesNotThrow(() =>
    assertBearerTransportSafe("http://localhost:8080", "secret", { MEMINI_REQUIRE_HTTPS: "1" }),
  );
});

test("assertBearerTransportSafe: http + non-loopback + secret does not throw by default (unset)", () => {
  assert.doesNotThrow(() => assertBearerTransportSafe("http://remote.example.com", "secret", {}));
});

test("assertBearerTransportSafe: MEMINI_REQUIRE_HTTPS turns the same unsafe combo into a throw", () => {
  assert.throws(
    () => assertBearerTransportSafe("http://remote.example.com", "secret", { MEMINI_REQUIRE_HTTPS: "1" }),
    /plaintext HTTP/,
  );
  // Broader envEnabled-style values also trigger it, consistent with Bootstrap.requireHttps.
  for (const v of ["true", "yes", "on"]) {
    assert.throws(() =>
      assertBearerTransportSafe("http://remote.example.com", "secret", { MEMINI_REQUIRE_HTTPS: v }),
    );
  }
});

test("assertBearerTransportSafe: an explicit MEMINI_REQUIRE_HTTPS=0 overrides back to no-throw", () => {
  assert.doesNotThrow(() =>
    assertBearerTransportSafe("http://remote.example.com", "secret", { MEMINI_REQUIRE_HTTPS: "0" }),
  );
});

test("assertBearerTransportSafe: no secret, no throw even over plaintext to a remote host", () => {
  assert.doesNotThrow(() =>
    assertBearerTransportSafe("http://remote.example.com", "", { MEMINI_REQUIRE_HTTPS: "1" }),
  );
});

test("assertBearerTransportSafe: https to a remote host never throws", () => {
  assert.doesNotThrow(() =>
    assertBearerTransportSafe("https://remote.example.com", "secret", { MEMINI_REQUIRE_HTTPS: "1" }),
  );
});
