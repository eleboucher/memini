// Run: node --test (from this directory). Not shipped by install.sh.
import { test } from "node:test";
import assert from "node:assert/strict";
import { effectiveNamespace } from "./plugin.mjs";

test("namespace_per_agent off returns the base namespace", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: false };
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "openclaw");
});

test("default template is the bare agent id", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "alice");
});

test("template can prefix and substitute {namespace}", () => {
  const cfg = { namespace: "openclaw", namespace_per_agent: true, namespace_template: "{namespace}-{agent}" };
  assert.equal(effectiveNamespace(cfg, { agent: { name: "bob" } }), "openclaw-bob");
});

test("resolves alternate event shapes", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, { agentName: "carol" }), "carol");
  // raw session UUIDs are not identities (they would fragment namespaces);
  // only agent-keyed session keys resolve
  assert.equal(effectiveNamespace(cfg, { sessionId: "sess1" }), "ns");
  assert.equal(effectiveNamespace(cfg, { sessionId: "agent:bob:b7d2-uuid" }), "bob");
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
  assert.equal(effectiveNamespace(cfg, { agentId: "alice" }), "alice");
});

test("ctx identity wins and session keys parse from ctx", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}" };
  assert.equal(effectiveNamespace(cfg, {}, { agentId: "alice" }), "alice");
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "agent:carol:cron:daily" }), "carol");
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "heartbeat:gateway" }), "ns");
});

test("skip_without_agent skips gateway-level sessions but keeps agent crons", () => {
  const cfg = { namespace: "ns", namespace_per_agent: true, namespace_template: "{agent}", skip_without_agent: true };
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "agent:alice:heartbeat:hourly" }), "alice");
  assert.equal(effectiveNamespace(cfg, {}, { sessionKey: "heartbeat:gateway" }), null);
  assert.equal(effectiveNamespace(cfg, {}, {}), null);
});
