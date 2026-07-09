import { test } from "node:test";
import assert from "node:assert/strict";

import {
  deriveNamespace,
  sanitizeNamespace,
  resolveConfig,
  formatResults,
  fitByTokens,
  approxTokens,
  meminiListPath,
  extractMessageText,
  extractLastAssistantText,
  buildTurnContent,
} from "../src/index.ts";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Hermetic: the resolver reads $XDG_CONFIG_HOME/memini/config.json, so point
// it at a temp dir instead of the developer's real config.
const xdgDir = mkdtempSync(join(tmpdir(), "pi-memini-test-"));
process.env["XDG_CONFIG_HOME"] = xdgDir;

test("deriveNamespace takes the cwd basename and sanitizes it", () => {
  assert.equal(deriveNamespace("/home/me/dev/My Repo"), "My-Repo");
  assert.equal(deriveNamespace("/home/me/dev/memini/"), "memini");
  assert.equal(deriveNamespace(""), "");
  assert.equal(deriveNamespace(undefined), "");
});

test("sanitizeNamespace keeps header-safe chars and trims dashes", () => {
  assert.equal(sanitizeNamespace("  a/b c "), "a-b-c");
  assert.equal(sanitizeNamespace("ok.name_1"), "ok.name_1");
});

test("resolveConfig defaults, with env overriding and cwd fallback", () => {
  const base = resolveConfig({}, "/x/proj");
  assert.equal(base.namespace, "proj");
  assert.equal(base.base_url, "http://localhost:8080");
  assert.equal(base.recall, true);
  assert.equal(base.capture, true);
  assert.equal(base.recall_limit, 3);

  const env = resolveConfig(
    { MEMINI_NAMESPACE: "shared", MEMINI_BASE_URL: "http://h:9", MEMINI_RECALL: "0", MEMINI_RECALL_LIMIT: "8" },
    "/x/proj",
  );
  assert.equal(env.namespace, "shared");
  assert.equal(env.base_url, "http://h:9");
  assert.equal(env.recall, false);
  assert.equal(env.recall_limit, 8);
});

test("resolveConfig falls back to the 'pi' default namespace with no cwd or env", () => {
  assert.equal(resolveConfig({}, undefined).namespace, "pi");
});

test("resolveConfig honours MEMINI_NAMESPACE even when cwd is unavailable", () => {
  assert.equal(resolveConfig({ MEMINI_NAMESPACE: "forced-ns" }, undefined).namespace, "forced-ns");
});

test("formatResults renders bullets and respects labels", () => {
  const results = [
    { memory: { tier: "semantic", content: "fact one" }, score: 0.9 },
    { memory: { tier: "episodic", summary: "did a thing" }, score: 0.5 },
  ];
  assert.deepEqual(formatResults(results, 3), ["- (semantic) fact one", "- (episodic) did a thing"]);

  const labeled = formatResults(results, 3, new Set(["tier"]));
  assert.equal(labeled[0], "[semantic] fact one");

  assert.deepEqual(formatResults([], 3), []);
});

test("fitByTokens trims to budget and reports dropped", () => {
  const items = ["one two three", "four five six", "seven eight nine"];
  const all = fitByTokens(items, 0);
  assert.equal(all.items.length, 3);
  assert.equal(all.dropped, 0);

  const tight = fitByTokens(items, approxTokens(items[0]));
  assert.equal(tight.items.length, 1);
  assert.equal(tight.dropped, 2);
});

test("meminiListPath encodes tiers, tags, metadata, and limit", () => {
  assert.equal(meminiListPath({}), "/v1/memories");
  assert.equal(
    meminiListPath({ tiers: ["procedural"], tags: ["x"], metadata: { category: "bug_fixes" }, limit: 5 }),
    "/v1/memories?tier=procedural&tag=x&meta=category%3Dbug_fixes&limit=5",
  );
  // limit=0 means "all" — omitted from the query string.
  assert.equal(meminiListPath({ limit: 0 }), "/v1/memories");
});

test("extractMessageText handles string and array content shapes", () => {
  assert.equal(extractMessageText({ content: "hello" }), "hello");
  assert.equal(
    extractMessageText({
      content: [
        { type: "text", text: "a" },
        { type: "tool_use", id: "t1" },
        { type: "text", text: "b" },
      ],
    }),
    "a\nb",
  );
  assert.equal(extractMessageText({ text: "fallback" }), "fallback");
  assert.equal(extractMessageText(null), "");
});

test("extractLastAssistantText returns only the latest assistant turn", () => {
  // agent_end carries the whole conversation; capture must take just the last
  // assistant reply, not a join of every earlier one.
  const messages = [
    { role: "user", content: "q1" },
    { role: "assistant", content: "first reply" },
    { role: "user", content: "q2" },
    { role: "assistant", content: [{ type: "text", text: "second reply" }] },
  ];
  assert.equal(extractLastAssistantText(messages), "second reply");

  // Skips a trailing toolResult to find the last assistant message.
  assert.equal(
    extractLastAssistantText([
      { role: "assistant", content: "the answer" },
      { role: "toolResult", content: "tool output" },
    ]),
    "the answer",
  );

  assert.equal(extractLastAssistantText([]), "");
});

test("buildTurnContent bounds each side", () => {
  const content = buildTurnContent("u".repeat(2000), "a".repeat(5000));
  const [user, assistant] = content.split("\n\n");
  assert.equal(user.length, 1000);
  assert.equal(assistant.length, 3000);
});

test("recall does not re-inject memories already shown in the same session", async () => {
  const { default: meminiExtension } = await import("../src/index.ts");
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (url: any) => {
    const body = String(url).endsWith("/v1/search")
      ? { results: [{ memory: { id: "m1", summary: "prior note", tier: "semantic" }, score: 0.9 }] }
      : { id: "w1" };
    return {
      ok: true,
      status: 200,
      async json() {
        return body;
      },
      async text() {
        return JSON.stringify(body);
      },
    };
  }) as any;
  try {
    meminiExtension({
      on(name: string, h: any) {
        hooks[name] = h;
      },
      registerTool() {},
    } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-1", getLeafId: () => "leaf-1" } };
    // The injected recall message persists in context, so an unchanged match
    // must not be re-injected on the next turn.
    const first = await hooks.before_agent_start({ prompt: "what did we decide?" }, ctx);
    assert.match(first.message.content, /prior note/);
    const second = await hooks.before_agent_start({ prompt: "and what else?" }, ctx);
    assert.equal(second, undefined, "already-shown memory must not re-inject");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("an HTTP error on recall is logged even when fallback_on_error degrades it", async () => {
  const { default: meminiExtension } = await import("../src/index.ts");
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  const realError = console.error;
  const logged: string[] = [];
  console.error = (m: any) => logged.push(String(m));
  globalThis.fetch = (async () => ({
    ok: false,
    status: 500,
    async json() {
      return {};
    },
    async text() {
      return "boom";
    },
  })) as any;
  try {
    meminiExtension({
      on(name: string, h: any) {
        hooks[name] = h;
      },
      registerTool() {},
    } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-err", getLeafId: () => "leaf-1" } };
    // A swallowed 500 looks like "memory isn't working"; the degrade path must
    // still say why on stderr.
    const out = await hooks.before_agent_start({ prompt: "anything" }, ctx);
    assert.equal(out, undefined, "recall failure degrades to no injection");
    assert.ok(
      logged.some((m) => m.includes("failed: 500")),
      `expected a failed-status warn, got: ${JSON.stringify(logged)}`,
    );
  } finally {
    globalThis.fetch = realFetch;
    console.error = realError;
  }
});

test("tenant path from a config file keeps its separator", () => {
  // Written last: earlier resolveConfig tests rely on no config being present.
  const dir = join(xdgDir, "memini");
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    join(dir, "config.json"),
    JSON.stringify({ tenantRoots: [{ path: "/x", tenant: "work" }], template: "{tenant}/{project}" }),
  );
  const cfg = resolveConfig({}, "/x/proj");
  assert.equal(cfg.namespace, "work/proj");
});
