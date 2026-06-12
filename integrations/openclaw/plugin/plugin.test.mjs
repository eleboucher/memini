// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import { effectiveNamespace, resolveConfig } from "./plugin.mjs";

test("per-agent isolation is on by default", () => {
  const cfg = resolveConfig(undefined);
  assert.equal(cfg.namespace_per_agent, true);
  assert.equal(cfg.namespace, "openclaw");
  // A named agent lands in its own namespace, not the shared pool.
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "openclaw-miso");
  // A session with no agent identity falls back to the shared base namespace
  // (so unattributable sessions still get memory rather than silently dropping).
  assert.equal(effectiveNamespace(cfg, {}), "openclaw");
});

test("namespace_per_agent can be explicitly disabled", () => {
  const cfg = resolveConfig({ namespace_per_agent: false });
  assert.equal(cfg.namespace_per_agent, false);
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "openclaw");
});

test("default per-agent template prefixes the configured base namespace", () => {
  const cfg = resolveConfig({ namespace: "team" });
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "team-miso");
});

test("namespace_per_agent off returns the base namespace", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: false };
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "openclaw");
});

test("default template is the bare agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "miso");
});

test("template can prefix and substitute {namespace}", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{namespace}-{agent}" };
  assert.equal(effectiveNamespace(cfg, { agent: { name: "saffron" } }), "openclaw-saffron");
});

test("resolves alternate event shapes", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentName: "matcha" }), "matcha");
  assert.equal(effectiveNamespace(cfg, { sessionId: "sess1" }), "sess1");
});

test("sanitizes the agent id", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentName: "My Agent/2!" }), "My-Agent-2");
});

test("falls back to base namespace when no agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, {}), "openclaw");
  assert.equal(effectiveNamespace(cfg, { agentId: "   " }), "openclaw");
});

test("skip_without_agent returns null when no agent id (per-agent mode)", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, {}), null);
});

test("skip_without_agent still resolves a present agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, { agentId: "miso" }), "miso");
});
