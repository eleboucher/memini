// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  resolveConfig,
  deriveNamespace,
  extractPartsText,
  formatResults,
  extractLastTurn,
  createPlaintextBearerAuthGuard,
} from "./memini.js";

test("namespace derives from the git worktree basename", () => {
  assert.equal(deriveNamespace("/home/me/dev/memini"), "memini");
  assert.equal(deriveNamespace("/home/me/dev/memini/"), "memini");
  assert.equal(deriveNamespace(""), "");
});

test("config defaults: recall and capture on, project-scoped namespace", () => {
  const cfg = resolveConfig({}, undefined, "/home/me/dev/my-project");
  assert.equal(cfg.base_url, "http://localhost:8080");
  assert.equal(cfg.namespace, "my-project");
  assert.equal(cfg.recall, true);
  assert.equal(cfg.capture, true);
  assert.equal(cfg.recall_limit, 5);
  assert.equal(cfg.fallback_on_error, true);
});

test("env overrides defaults; options override env", () => {
  const env = { MEMINI_BASE_URL: "http://memini:9000", MEMINI_NAMESPACE: "team", MEMINI_RECALL: "0" };
  const fromEnv = resolveConfig(env, undefined, "/repo/ignored");
  assert.equal(fromEnv.base_url, "http://memini:9000");
  assert.equal(fromEnv.namespace, "team");
  assert.equal(fromEnv.recall, false);

  const fromOpts = resolveConfig(env, { namespace: "explicit", base_url: "http://x" }, "/repo");
  assert.equal(fromOpts.namespace, "explicit");
  assert.equal(fromOpts.base_url, "http://x");
});

test("namespace falls back to the default when nothing resolves", () => {
  assert.equal(resolveConfig({}, undefined, "").namespace, "opencode");
});

test("capture can be disabled via env", () => {
  assert.equal(resolveConfig({ MEMINI_CAPTURE: "false" }, undefined, "/r").capture, false);
});

test("extractPartsText skips synthetic and ignored parts", () => {
  const parts = [
    { type: "text", text: "real question", synthetic: false },
    { type: "text", text: "injected memory", synthetic: true },
    { type: "text", text: "muted", ignored: true },
    { type: "tool", text: "not text" },
  ];
  assert.equal(extractPartsText(parts), "real question");
  assert.equal(extractPartsText(undefined), "");
});

test("formatResults renders a bullet list with tier and truncation", () => {
  const results = [
    { memory: { summary: "uses postgres", tier: "semantic" } },
    { memory: { content: "fixed the race", tier: "episodic" } },
  ];
  assert.equal(formatResults(results, 5), "- (semantic) uses postgres\n- (episodic) fixed the race");
  assert.equal(formatResults([], 5), "");
  assert.equal(formatResults(undefined, 5), "");
});

test("formatResults respects the limit", () => {
  const results = Array.from({ length: 8 }, (_, i) => ({ memory: { content: `m${i}`, tier: "t" } }));
  assert.equal(formatResults(results, 3).split("\n").length, 3);
});

test("extractLastTurn takes the latest user and assistant text plus assistant id", () => {
  const messages = [
    { info: { role: "user" }, parts: [{ type: "text", text: "first" }] },
    { info: { role: "assistant", id: "a1" }, parts: [{ type: "text", text: "ans1" }] },
    { info: { role: "user" }, parts: [{ type: "text", text: "second" }] },
    {
      info: { role: "assistant", id: "a2" },
      parts: [
        { type: "text", text: "injected", synthetic: true },
        { type: "text", text: "ans2" },
      ],
    },
  ];
  assert.deepEqual(extractLastTurn(messages), {
    userText: "second",
    assistantText: "ans2",
    assistantID: "a2",
  });
});

test("extractLastTurn handles empty input", () => {
  assert.deepEqual(extractLastTurn(undefined), { userText: "", assistantText: "", assistantID: "" });
});

test("plaintext bearer guard warns once for http to a non-loopback host", () => {
  const warnings = [];
  const guard = createPlaintextBearerAuthGuard((m) => warnings.push(m), {});
  guard("http://memini.example.com", "secret");
  guard("http://memini.example.com", "secret");
  assert.equal(warnings.length, 1);
});

test("plaintext bearer guard is silent for loopback and for https", () => {
  const warnings = [];
  const guard = createPlaintextBearerAuthGuard((m) => warnings.push(m), {});
  guard("http://localhost:8080", "secret");
  guard("https://memini.example.com", "secret");
  guard("http://memini.example.com", ""); // no secret
  assert.equal(warnings.length, 0);
});

test("plaintext bearer guard throws when MEMINI_REQUIRE_HTTPS=1", () => {
  const guard = createPlaintextBearerAuthGuard(() => {}, { MEMINI_REQUIRE_HTTPS: "1" });
  assert.throws(() => guard("http://memini.example.com", "secret"), /plaintext HTTP/);
});
