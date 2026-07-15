import { test } from "node:test";
import assert from "node:assert/strict";

import {
  resolveStaticConfig,
  resolveLiveConfig,
  createSessionContext,
  sessionLive,
  memoizeAsync,
  pinKeyFacts,
  registerMeminiCommands,
  buildWarnings,
  formatResults,
  fitByTokens,
  approxTokens,
  meminiListPath,
  briefingPath,
  extractMessageText,
  extractLastAssistantText,
  buildTurnContent,
} from "../src/index.ts";
import { execSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// A developer shell may export the real memini config; clear it so the
// resolution tests see the documented defaults (an exported MEMINI_NAMESPACE —
// the fish-universal-variable case this feature exists for — would otherwise
// fail every default-namespace assertion below).
for (const k of ["MEMINI_NAMESPACE", "MEMINI_BASE_URL", "MEMINI_URL", "MEMINI_API_KEY", "MEMINI_HOME", "MEMINI_FALLBACK"]) {
  delete process.env[k];
}

function tmpProject(withGit = true): string {
  const dir = mkdtempSync(join(tmpdir(), "pi-memini-proj-"));
  if (withGit) {
    execSync("git init -q", { cwd: dir });
    execSync("git remote add origin https://example.com/acme/widget.git", { cwd: dir });
  }
  return dir;
}

function fakeHandshake(overrides: Record<string, any> = {}) {
  return {
    namespace: "acme/widget",
    namespace_source: "derived",
    identity: { authenticated: false },
    settings: {},
    settings_sources: {},
    read_set: [],
    server: { version: "test", default_namespace: "default" },
    ...overrides,
  };
}

// --- resolveStaticConfig ------------------------------------------------------

test("resolveStaticConfig: defaults and env overrides", () => {
  const base = resolveStaticConfig({});
  assert.equal(base.home, undefined);
  assert.equal(base.timeout_ms, 30000);
  assert.equal(base.fallback_on_error, true);

  const env = resolveStaticConfig({ MEMINI_HOME: "personal/acme", MEMINI_TIMEOUT_MS: "1234", MEMINI_FALLBACK: "0" });
  assert.equal(env.home, "personal/acme");
  assert.equal(env.timeout_ms, 1234);
  assert.equal(env.fallback_on_error, false);
});

// --- resolveLiveConfig: namespace precedence ----------------------------------

test("resolveLiveConfig: a handshake wins outright, degraded is false", () => {
  const boot = { baseUrl: "http://x", apiKey: "", requireHttps: false, debug: false, agent: "", namespaceEnv: "pinned", homeEnv: "" };
  const facts = { cwd_basename: "proj" };
  const hs = fakeHandshake({ namespace: "acme/api", namespace_source: "pin" });
  const live = resolveLiveConfig(boot as any, facts, hs as any, {});
  assert.equal(live.namespace, "acme/api");
  assert.equal(live.namespace_source, "server:pin");
  assert.equal(live.degraded, false);
});

test("resolveLiveConfig: no handshake falls to MEMINI_NAMESPACE, then local derivation — both degraded", () => {
  const boot = { baseUrl: "http://x", apiKey: "", requireHttps: false, debug: false, agent: "", namespaceEnv: "pinned", homeEnv: "" };
  const facts = { cwd_basename: "proj" };
  const withEnv = resolveLiveConfig(boot as any, facts, undefined, {});
  assert.equal(withEnv.namespace, "pinned");
  assert.equal(withEnv.namespace_source, "env");
  assert.equal(withEnv.degraded, true);

  const noEnvBoot = { ...boot, namespaceEnv: "" };
  const local = resolveLiveConfig(noEnvBoot as any, facts, undefined, {});
  assert.equal(local.namespace, "proj");
  assert.equal(local.namespace_source, "local-cwd");
  assert.equal(local.degraded, true);
});

// --- resolveLiveConfig: behavior knobs ----------------------------------------

test("resolveLiveConfig: behavior knobs — env-override beats server beats built-in default", () => {
  const boot = { baseUrl: "http://x", apiKey: "", requireHttps: false, debug: false, agent: "", namespaceEnv: "", homeEnv: "" };
  const facts = { cwd_basename: "proj" };

  // No handshake, no env: built-in defaults.
  const def = resolveLiveConfig(boot as any, facts, undefined, {});
  assert.equal(def.recall, true);
  assert.equal(def.capture, true);
  assert.equal(def.recall_limit, 3);

  // Server settings win over the built-in default.
  const hs = fakeHandshake({ settings: { recall: false, capture: false, recall_limit: 8 } });
  const server = resolveLiveConfig(boot as any, facts, hs as any, {});
  assert.equal(server.recall, false);
  assert.equal(server.capture, false);
  assert.equal(server.recall_limit, 8);

  // A local env override still wins over the server's value.
  const envOverride = resolveLiveConfig(boot as any, facts, hs as any, { MEMINI_RECALL: "1", MEMINI_RECALL_LIMIT: "2" });
  assert.equal(envOverride.recall, true);
  assert.equal(envOverride.recall_limit, 2);
  // capture still comes from the server — only recall/recall_limit were overridden.
  assert.equal(envOverride.capture, false);
});

// --- memoizeAsync: TTL + invalidate --------------------------------------------

test("memoizeAsync: calls fn once within the TTL window", async () => {
  let calls = 0;
  let t = 1000;
  const memo = memoizeAsync(async () => { calls++; return calls; }, 1000, () => t);
  assert.equal(await memo.get(), 1);
  t += 500; // still inside the window
  assert.equal(await memo.get(), 1);
  assert.equal(calls, 1);
});

test("memoizeAsync: re-calls fn after the TTL expires", async () => {
  let calls = 0;
  let t = 1000;
  const memo = memoizeAsync(async () => { calls++; return calls; }, 1000, () => t);
  assert.equal(await memo.get(), 1);
  t += 1000; // exactly at expiry — treated as expired
  assert.equal(await memo.get(), 2);
  assert.equal(calls, 2);
});

test("memoizeAsync: invalidate() forces the very next get() to refresh", async () => {
  let calls = 0;
  const memo = memoizeAsync(async () => { calls++; return calls; }, 60_000);
  assert.equal(await memo.get(), 1);
  assert.equal(await memo.get(), 1, "still memoized");
  memo.invalidate();
  assert.equal(await memo.get(), 2, "invalidate forces a refresh");
});

// --- fail-soft: a guard throw honors MEMINI_FALLBACK --------------------------

test("sessionLive: MEMINI_REQUIRE_HTTPS guard throw degrades to local derivation when fallback is on (default)", async () => {
  const cwd = tmpProject();
  const prev = { url: process.env.MEMINI_BASE_URL, key: process.env.MEMINI_API_KEY, https: process.env.MEMINI_REQUIRE_HTTPS };
  try {
    process.env.MEMINI_BASE_URL = "http://example.com"; // plaintext, non-loopback
    process.env.MEMINI_API_KEY = "sk-test-token";
    process.env.MEMINI_REQUIRE_HTTPS = "1";
    const ctx = createSessionContext(cwd, process.env);
    const live = await sessionLive(ctx);
    assert.equal(live.degraded, true, "the guard throw must not escape — it degrades like any other handshake failure");
  } finally {
    if (prev.url === undefined) delete process.env.MEMINI_BASE_URL; else process.env.MEMINI_BASE_URL = prev.url;
    if (prev.key === undefined) delete process.env.MEMINI_API_KEY; else process.env.MEMINI_API_KEY = prev.key;
    if (prev.https === undefined) delete process.env.MEMINI_REQUIRE_HTTPS; else process.env.MEMINI_REQUIRE_HTTPS = prev.https;
  }
});

test("sessionLive: with MEMINI_FALLBACK=0, the guard throw propagates instead of degrading silently", async () => {
  const cwd = tmpProject();
  const prev = {
    url: process.env.MEMINI_BASE_URL,
    key: process.env.MEMINI_API_KEY,
    https: process.env.MEMINI_REQUIRE_HTTPS,
    fallback: process.env.MEMINI_FALLBACK,
  };
  try {
    process.env.MEMINI_BASE_URL = "http://example.com";
    process.env.MEMINI_API_KEY = "sk-test-token";
    process.env.MEMINI_REQUIRE_HTTPS = "1";
    process.env.MEMINI_FALLBACK = "0";
    const ctx = createSessionContext(cwd, process.env);
    await assert.rejects(() => sessionLive(ctx), /plaintext HTTP/);
  } finally {
    if (prev.url === undefined) delete process.env.MEMINI_BASE_URL; else process.env.MEMINI_BASE_URL = prev.url;
    if (prev.key === undefined) delete process.env.MEMINI_API_KEY; else process.env.MEMINI_API_KEY = prev.key;
    if (prev.https === undefined) delete process.env.MEMINI_REQUIRE_HTTPS; else process.env.MEMINI_REQUIRE_HTTPS = prev.https;
    if (prev.fallback === undefined) delete process.env.MEMINI_FALLBACK; else process.env.MEMINI_FALLBACK = prev.fallback;
  }
});

test("sessionLive: a network failure always degrades regardless of MEMINI_FALLBACK", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  const prev = { url: process.env.MEMINI_BASE_URL, fallback: process.env.MEMINI_FALLBACK };
  globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
  try {
    process.env.MEMINI_BASE_URL = "http://localhost:19999";
    process.env.MEMINI_FALLBACK = "0"; // even with fallback off — the handshake itself is always fail-soft for network errors
    const ctx = createSessionContext(cwd, process.env);
    const live = await sessionLive(ctx);
    assert.equal(live.degraded, true);
  } finally {
    globalThis.fetch = realFetch;
    if (prev.url === undefined) delete process.env.MEMINI_BASE_URL; else process.env.MEMINI_BASE_URL = prev.url;
    if (prev.fallback === undefined) delete process.env.MEMINI_FALLBACK; else process.env.MEMINI_FALLBACK = prev.fallback;
  }
});

// --- pinKeyFacts ---------------------------------------------------------------

test("pinKeyFacts: only remote_url/toplevel_path key a pin", () => {
  assert.deepEqual(pinKeyFacts({ cwd_basename: "x" }), {});
  assert.deepEqual(pinKeyFacts({ cwd_basename: "x", remote_url: "https://example.com/a/b.git" }), {
    remote_url: "https://example.com/a/b.git",
  });
  assert.deepEqual(pinKeyFacts({ cwd_basename: "x", toplevel_path: "/a/b" }), { toplevel_path: "/a/b" });
});

// --- registerMeminiCommands: memini:namespace / memini:status -----------------

function fakePi() {
  const commands: Record<string, (args: string, ctx: any) => Promise<void>> = {};
  const shown: string[] = [];
  const notified: string[] = [];
  const pi = {
    registerCommand(name: string, options: any) {
      commands[name] = options.handler;
    },
    sendMessage(message: any) {
      shown.push(String(message.content));
    },
  };
  const ctx = { ui: { notify: (m: string) => notified.push(m) } };
  return { pi, commands, shown, notified, ctx };
}

function mockPinsAndHandshake(handshakeResult: any, opts: { pinOk?: boolean; pinStatus?: number; pinBody?: any } = {}) {
  const calls: { url: string; method: string; body?: any }[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    const u = String(url);
    const method = init?.method || "GET";
    const body = init?.body ? JSON.parse(init.body) : undefined;
    calls.push({ url: u, method, body });
    if (u.endsWith("/v1/handshake")) {
      if (!handshakeResult) return { ok: false, status: 500, async json() { return {}; }, async text() { return ""; } };
      return { ok: true, status: 200, async json() { return handshakeResult; } };
    }
    if (u.endsWith("/v1/pins")) {
      const ok = opts.pinOk ?? true;
      const status = opts.pinStatus ?? (ok ? 200 : 400);
      return {
        ok,
        status,
        async json() {
          if (opts.pinBody !== undefined) return opts.pinBody;
          return ok ? { namespace: body?.namespace, key: `remote:${body?.remote_url}` } : { error: "bad" };
        },
      };
    }
    if (u.includes("/v1/namespaces/readset")) {
      return { ok: true, status: 200, async json() { return { entries: [] }; } };
    }
    return { ok: false, status: 404, async json() { return {}; }, async text() { return ""; } };
  }) as any;
  return calls;
}

test("memini:namespace (no args) shows the live handshake namespace + pin details", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  try {
    mockPinsAndHandshake(
      fakeHandshake({ namespace: "acme/widget", namespace_source: "pin", pin: { key: "remote:x", updated_at: "2026-01-01T00:00:00Z" } }),
    );
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:namespace"]("", ctx);
    assert.match(shown.at(-1)!, /namespace: acme\/widget\s+\(source: pin\)/);
    assert.match(shown.at(-1)!, /pin:\s+key remote:x/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace <ns>: PUTs a pin keyed by git facts and invalidates the memo", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  try {
    const calls = mockPinsAndHandshake(fakeHandshake({ namespace: "old", namespace_source: "derived" }));
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});

    // Prime the memo with a first handshake so we can prove it gets dropped.
    await sessionCtx.memo.get();
    const handshakeCallsBefore = calls.filter((c) => c.url.endsWith("/v1/handshake")).length;

    await commands["memini:namespace"]("acme/api", ctx);
    assert.match(shown.at(-1)!, /namespace pinned: acme\/api/);
    const put = calls.find((c) => c.method === "PUT" && c.url.endsWith("/v1/pins"));
    assert.ok(put, "expected a PUT /v1/pins call");
    assert.equal(put!.body.namespace, "acme/api");
    assert.equal(put!.body.remote_url, "https://example.com/acme/widget.git");

    // The memo was dropped: the very next get() re-handshakes rather than
    // reusing the stale cached value.
    await sessionCtx.memo.get();
    const handshakeCallsAfter = calls.filter((c) => c.url.endsWith("/v1/handshake")).length;
    assert.ok(handshakeCallsAfter > handshakeCallsBefore, "pin write must invalidate the in-memory memo");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace <ns>: refuses when the project has no git remote or toplevel", async () => {
  const cwd = tmpProject(false); // not a git repo
  const realFetch = globalThis.fetch;
  try {
    const calls = mockPinsAndHandshake(undefined);
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, notified, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:namespace"]("acme/api", ctx);
    assert.match(notified.at(-1)!, /no git remote or toplevel/);
    assert.equal(calls.filter((c) => c.method === "PUT").length, 0, "no PUT should be issued");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace <ns>: refuses a header-injecting namespace instead of normalizing it", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  try {
    const calls = mockPinsAndHandshake(fakeHandshake());
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, notified, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:namespace"]("evil\r\nX-Evil: 1", ctx);
    assert.match(notified.at(-1)!, /invalid namespace/);
    assert.equal(shown.length, 0);
    assert.equal(calls.filter((c) => c.method === "PUT").length, 0);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace --clear: 404 reports nothing to clear; success invalidates the memo", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  try {
    mockPinsAndHandshake(fakeHandshake(), { pinStatus: 404, pinOk: false });
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:namespace"]("--clear", ctx);
    assert.match(shown.at(-1)!, /nothing to clear/);
  } finally {
    globalThis.fetch = realFetch;
  }

  const cwd2 = tmpProject();
  try {
    mockPinsAndHandshake(fakeHandshake());
    const ctx2 = createSessionContext(cwd2, process.env);
    const { pi, commands, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, ctx2, resolveStaticConfig(process.env), () => {});
    await commands["memini:namespace"]("--clear", ctx);
    assert.match(shown.at(-1)!, /namespace pin cleared/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:namespace: an unreachable server on a pin write shows the offline help", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
  try {
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, notified, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:namespace"]("acme/api", ctx);
    assert.match(notified.at(-1)!, /Could not reach the memini server/);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:status reports an unreachable server rather than throwing into the host", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
  try {
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, shown, notified, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:status"]("", ctx);
    assert.match(shown.at(-1)!, /reachable\s+NO/);
    assert.deepEqual(notified, []);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memini:status redacts the bearer and shows the read set when reachable", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  const prevKey = process.env.MEMINI_API_KEY;
  try {
    process.env.MEMINI_API_KEY = "sk-abcdefghijklmnop4f2a";
    globalThis.fetch = (async (url: any) => {
      const u = String(url);
      if (u.endsWith("/v1/handshake")) {
        return { ok: true, status: 200, async json() { return fakeHandshake({ namespace: "acme/widget" }); } };
      }
      if (u.includes("/v1/namespaces/readset")) {
        return { ok: true, status: 200, async json() { return { entries: [{ namespace: "acme/widget", origin: "self", tiers: [] }] }; } };
      }
      return { ok: false, status: 404, async json() { return {}; }, async text() { return ""; } };
    }) as any;
    const sessionCtx = createSessionContext(cwd, process.env);
    const { pi, commands, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, sessionCtx, resolveStaticConfig(process.env), () => {});
    await commands["memini:status"]("", ctx);
    const out = shown.at(-1)!;
    assert.match(out, /reachable\s+yes/);
    assert.match(out, /READ SET/);
    assert.doesNotMatch(out, /sk-abcdefghijklmnop4f2a/);
    assert.match(out, /sk-…4f2a/);
  } finally {
    globalThis.fetch = realFetch;
    if (prevKey === undefined) delete process.env.MEMINI_API_KEY; else process.env.MEMINI_API_KEY = prevKey;
  }
});

// --- buildWarnings -------------------------------------------------------------

test("buildWarnings: flags a global MEMINI_NAMESPACE pin only when the server hasn't confirmed a stronger pin", () => {
  const boot = { baseUrl: "http://x", apiKey: "", requireHttps: false, debug: false, agent: "", namespaceEnv: "global", homeEnv: "home/x" };
  const facts = { cwd_basename: "proj" };
  const ctx = { boot, facts, memo: null as any };
  const live = resolveLiveConfig(boot as any, facts, undefined, {});
  const warningsNoPin = buildWarnings(ctx as any, live, undefined);
  assert.ok(warningsNoPin.some((w) => w.code === "global-namespace-pin"));

  const hsWithPin = fakeHandshake({ namespace_source: "pin" });
  const liveWithPin = resolveLiveConfig(boot as any, facts, hsWithPin as any, {});
  const warningsWithPin = buildWarnings(ctx as any, liveWithPin, hsWithPin as any);
  assert.ok(!warningsWithPin.some((w) => w.code === "global-namespace-pin"));
});

test("buildWarnings: home-unset only fires when MEMINI_HOME is unset", () => {
  const withHome = { baseUrl: "http://x", apiKey: "", requireHttps: false, debug: false, agent: "", namespaceEnv: "", homeEnv: "personal/me" };
  const withoutHome = { ...withHome, homeEnv: "" };
  const facts = { cwd_basename: "proj" };
  const live = resolveLiveConfig(withHome as any, facts, undefined, {});
  assert.ok(!buildWarnings({ boot: withHome, facts } as any, live, undefined).some((w) => w.code === "home-unset"));
  assert.ok(buildWarnings({ boot: withoutHome, facts } as any, live, undefined).some((w) => w.code === "home-unset"));
});

// --- unchanged helpers ---------------------------------------------------------

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
  const messages = [
    { role: "user", content: "q1" },
    { role: "assistant", content: "first reply" },
    { role: "user", content: "q2" },
    { role: "assistant", content: [{ type: "text", text: "second reply" }] },
  ];
  assert.equal(extractLastAssistantText(messages), "second reply");

  assert.equal(
    extractLastAssistantText([
      { role: "assistant", content: "the answer" },
      { role: "toolResult", content: "tool output" },
    ]),
    "the answer",
  );

  assert.equal(extractLastAssistantText([]), "");
});

test("buildTurnContent bounds each side by the passed-in (server-resolved) caps", () => {
  const content = buildTurnContent("u".repeat(2000), "a".repeat(5000), 1000, 3000);
  const [user, assistant] = content.split("\n\n");
  // Each side is its cap plus the truncation marker, which is what tells a
  // later reader the turn is a fragment.
  assert.equal(user, "u".repeat(1000) + "\n[...truncated]");
  assert.equal(assistant, "a".repeat(3000) + "\n[...truncated]");
});

test("buildTurnContent: a 0 cap captures that side whole rather than emptying it", () => {
  const content = buildTurnContent("uuu", "aaa", 0, 0);
  assert.equal(content, "uuu\n\naaa");
});

test("briefingPath only forwards a known scope; a hallucinated one is dropped", () => {
  assert.equal(briefingPath({}), "/v1/namespaces/briefing");
  assert.equal(briefingPath({ scope: "everywhere" }), "/v1/namespaces/briefing?scope=everywhere");
  assert.equal(briefingPath({ scope: "acme/phoenix" }), "/v1/namespaces/briefing");
  assert.equal(briefingPath({ scope: "subtree" }), "/v1/namespaces/briefing");
});

// --- recall / capture / tools (via the default extension export) -------------

function mockRecallFetch() {
  globalThis.fetch = (async (url: any) => {
    const u = String(url);
    if (u.endsWith("/v1/handshake")) {
      return { ok: false, status: 500, async json() { return {}; }, async text() { return ""; } };
    }
    const body = u.endsWith("/v1/search")
      ? { results: [{ memory: { id: "m1", summary: "prior note", tier: "semantic" }, score: 0.9 }] }
      : { id: "w1" };
    return {
      ok: true,
      status: 200,
      async json() { return body; },
      async text() { return JSON.stringify(body); },
    };
  }) as any;
}

test("recall does not re-inject memories already shown in the same session", async () => {
  const cwd = tmpProject();
  const prevCwd = process.cwd();
  process.chdir(cwd);
  const { default: meminiExtension } = await import("../src/index.ts?cb=recall-" + Date.now());
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  mockRecallFetch();
  try {
    meminiExtension({ on(name: string, h: any) { hooks[name] = h; }, registerTool() {} } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-1", getLeafId: () => "leaf-1" } };
    const first = await hooks.before_agent_start({ prompt: "what did we decide?" }, ctx);
    assert.match(first.message.content, /prior note/);
    const second = await hooks.before_agent_start({ prompt: "and what else?" }, ctx);
    assert.equal(second, undefined, "already-shown memory must not re-inject");
  } finally {
    globalThis.fetch = realFetch;
    process.chdir(prevCwd);
  }
});

// The per-session dedupe window is capped: oldest ids age out (and may
// re-inject); recent ids stay suppressed.
test("per-session injected-id window is bounded (oldest ids age out)", async () => {
  const { default: meminiExtension } = await import("../src/index.ts");
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  let nextResults: any[] = [];
  globalThis.fetch = (async (url: any) => {
    const body = String(url).endsWith("/v1/search") ? { results: nextResults } : { id: "w1" };
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
  const hit = (id: string) => ({ memory: { id, summary: `note ${id}`, tier: "semantic" }, score: 0.9 });
  try {
    meminiExtension({
      on(name: string, h: any) {
        hooks[name] = h;
      },
      registerTool() {},
    } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-w", getLeafId: () => "leaf-1" } };
    // Push 206 distinct ids through the window (cap is 200): m0..m205.
    for (let i = 0; i < 206; i++) {
      nextResults = [hit(`m${i}`)];
      const out = await hooks.before_agent_start({ prompt: `q${i}` }, ctx);
      assert.match(out.message.content, new RegExp(`m${i}\\b`), `call ${i} should inject its fresh memory`);
    }
    // m0 was evicted from the 200-id window -> allowed to re-inject.
    nextResults = [hit("m0")];
    const old = await hooks.before_agent_start({ prompt: "old" }, ctx);
    assert.ok(old, "an id evicted from the window must be allowed to re-inject");
    assert.match(old.message.content, /m0\b/);
    // m205 is still inside the window -> suppressed.
    nextResults = [hit("m205")];
    const recent = await hooks.before_agent_start({ prompt: "recent" }, ctx);
    assert.equal(recent, undefined, "a recent id must stay suppressed");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Already-shown ids ride to the server as exclude_ids; an older server that
// 400s on the unknown field gets one retry without it, then it stops.
test("recall sends exclude_ids and falls back when the server rejects them", async () => {
  const { default: meminiExtension } = await import("../src/index.ts");
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  const requests: any[] = [];
  let rejectExcludeIds = false;
  globalThis.fetch = (async (url: any, init: any) => {
    const body = init?.body ? JSON.parse(init.body) : {};
    requests.push({ url: String(url), body });
    if (rejectExcludeIds && body.exclude_ids) {
      return {
        ok: false,
        status: 400,
        async json() {
          return {};
        },
        async text() {
          return 'unknown field "exclude_ids"';
        },
      };
    }
    const res = String(url).endsWith("/v1/search")
      ? { results: [{ memory: { id: "m1", summary: "prior note", tier: "semantic" }, score: 0.9 }] }
      : { id: "w1" };
    return {
      ok: true,
      status: 200,
      async json() {
        return res;
      },
      async text() {
        return JSON.stringify(res);
      },
    };
  }) as any;
  const searches = () => requests.filter((r) => r.url.endsWith("/v1/search"));
  try {
    meminiExtension({
      on(name: string, h: any) {
        hooks[name] = h;
      },
      registerTool() {},
    } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-x", getLeafId: () => "leaf-1" } };
    // First recall: nothing shown yet, so no exclude_ids on the wire.
    await hooks.before_agent_start({ prompt: "q1" }, ctx);
    assert.equal(searches()[0].body.exclude_ids, undefined);
    // Second recall: m1 was shown, so it must ride along as exclude_ids.
    await hooks.before_agent_start({ prompt: "q2" }, ctx);
    assert.deepEqual(searches()[1].body.exclude_ids, ["m1"]);

    // Old server: 400 on exclude_ids -> one retry without it, then never again.
    rejectExcludeIds = true;
    await hooks.before_agent_start({ prompt: "q3" }, ctx);
    const [, , withField, retry] = searches();
    assert.deepEqual(withField.body.exclude_ids, ["m1"], "first attempt still carries exclude_ids");
    assert.equal(retry.body.exclude_ids, undefined, "the retry must drop exclude_ids");
    await hooks.before_agent_start({ prompt: "q4" }, ctx);
    assert.equal(searches().length, 5, "after the fallback each recall is a single request");
    assert.equal(searches()[4].body.exclude_ids, undefined, "exclude_ids is never sent again");
  } finally {
    globalThis.fetch = realFetch;
  }
});

// Same bound for the captured dedup keys.
test("captured dedup-key window is bounded (an aged-out turn can re-capture)", async () => {
  const { default: meminiExtension } = await import("../src/index.ts");
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  const posts: any[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    if (String(url).endsWith("/v1/memories")) posts.push(JSON.parse(init.body));
    const body = String(url).endsWith("/v1/search") ? { results: [] } : { id: "w1" };
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
    let leaf = "leaf-0";
    const ctx = { sessionManager: { getSessionId: () => "sess-c", getLeafId: () => leaf } };
    const endEvent = { messages: [{ role: "assistant", content: "reply" }] };
    // Each turn: before_agent_start buffers the prompt, agent_end captures it.
    const runTurn = async () => {
      await hooks.before_agent_start({ prompt: "hello" }, ctx);
      await hooks.agent_end(endEvent, ctx);
    };
    await runTurn();
    assert.equal(posts.length, 1, "first agent_end captures the turn");
    // A re-fired agent_end for the same leaf is still deduped (pendingUser was
    // consumed, so re-buffer the prompt to isolate the dedup-key check).
    await hooks.before_agent_start({ prompt: "hello" }, ctx);
    await hooks.agent_end(endEvent, ctx);
    assert.equal(posts.length, 1, "same turn must not capture twice");
    // Push 200 more distinct dedup keys through the window (cap is 200).
    for (let i = 1; i <= 200; i++) {
      leaf = `leaf-${i}`;
      await runTurn();
    }
    assert.equal(posts.length, 201);
    // leaf-0 has aged out of the window: a re-fired agent_end captures again.
    leaf = "leaf-0";
    await runTurn();
    assert.equal(posts.length, 202, "a key evicted from the window is re-capturable");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("an HTTP error on recall is logged even when fallback_on_error degrades it", async () => {
  const cwd = tmpProject();
  const prevCwd = process.cwd();
  process.chdir(cwd);
  const { default: meminiExtension } = await import("../src/index.ts?cb=recall-err-" + Date.now());
  const hooks: Record<string, any> = {};
  const realFetch = globalThis.fetch;
  const realError = console.error;
  const logged: string[] = [];
  console.error = (m: any) => logged.push(String(m));
  globalThis.fetch = (async (url: any) => {
    const u = String(url);
    if (u.endsWith("/v1/handshake")) return { ok: false, status: 500, async json() { return {}; }, async text() { return ""; } };
    return { ok: false, status: 500, async json() { return {}; }, async text() { return "boom"; } };
  }) as any;
  try {
    meminiExtension({ on(name: string, h: any) { hooks[name] = h; }, registerTool() {} } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-err", getLeafId: () => "leaf-1" } };
    const out = await hooks.before_agent_start({ prompt: "anything" }, ctx);
    assert.equal(out, undefined, "recall failure degrades to no injection");
    assert.ok(logged.some((m) => m.includes("failed: 500")), `expected a failed-status warn, got: ${JSON.stringify(logged)}`);
  } finally {
    globalThis.fetch = realFetch;
    console.error = realError;
    process.chdir(prevCwd);
  }
});

test("requests carry X-Memini-Home when MEMINI_HOME is set, omit it otherwise", async () => {
  const cwd = tmpProject();
  const prevCwd = process.cwd();
  process.chdir(cwd);
  const prevHome = process.env.MEMINI_HOME;
  const { default: meminiExtension } = await import("../src/index.ts?cb=home-" + Date.now());
  const hooks: Record<string, any> = {};
  const requests: any[] = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (url: any, init: any) => {
    const u = String(url);
    if (u.endsWith("/v1/handshake")) return { ok: false, status: 500, async json() { return {}; }, async text() { return ""; } };
    requests.push({ url: u, headers: init?.headers });
    return { ok: true, status: 200, async json() { return { results: [] }; }, async text() { return ""; } };
  }) as any;
  try {
    process.env.MEMINI_HOME = "personal/acme";
    meminiExtension({ on(name: string, h: any) { hooks[name] = h; }, registerTool() {} } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-home", getLeafId: () => "leaf-1" } };
    await hooks.before_agent_start({ prompt: "hello" }, ctx);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].headers["X-Memini-Home"], "personal/acme");
  } finally {
    if (prevHome === undefined) delete process.env.MEMINI_HOME;
    else process.env.MEMINI_HOME = prevHome;
    globalThis.fetch = realFetch;
    process.chdir(prevCwd);
  }
});

// --- explicit tools: scope / visibility --------------------------------------

async function collectTools(): Promise<{
  byName: Record<string, any>;
  calls: { url: string; method: string; body: any }[];
  reply: (body: any, ok?: boolean) => void;
}> {
  const cwd = tmpProject();
  const prevCwd = process.cwd();
  process.chdir(cwd);
  const { default: meminiExtension } = await import("../src/index.ts?cb=tools-" + Math.random());
  const calls: { url: string; method: string; body: any }[] = [];
  let next: { body: any; ok: boolean } = { body: {}, ok: true };
  globalThis.fetch = (async (url: any, init: any) => {
    const u = String(url);
    if (u.endsWith("/v1/handshake")) return { ok: false, status: 500, async json() { return {}; }, async text() { return ""; } };
    calls.push({
      url: u,
      method: init?.method || "GET",
      body: init?.body ? JSON.parse(init.body) : undefined,
    });
    return {
      ok: next.ok,
      status: next.ok ? 200 : 400,
      async json() { return next.body; },
      async text() { return typeof next.body === "string" ? next.body : JSON.stringify(next.body); },
    };
  }) as any;
  const tools: any[] = [];
  meminiExtension({
    on() {},
    registerTool(t: any) { tools.push(t); },
  } as any);
  process.chdir(prevCwd);
  return {
    byName: Object.fromEntries(tools.map((t) => [t.name, t])),
    calls,
    reply: (body: any, ok = true) => { next = { body, ok }; },
  };
}

test("memory_recall exposes scope and passes it to /v1/search", async () => {
  const realFetch = globalThis.fetch;
  try {
    const { byName, calls, reply } = await collectTools();
    const schema: any = byName.memory_recall.parameters;
    assert.deepEqual(schema.properties.scope.enum, ["project", "full", "everywhere"]);
    assert.ok(!schema.required?.includes("scope"));

    reply({ results: [] });
    await byName.memory_recall.execute("id", { query: "auth", scope: "everywhere" });
    assert.equal(calls.at(-1)!.body.scope, "everywhere");

    await byName.memory_recall.execute("id", { query: "auth" });
    assert.equal("scope" in calls.at(-1)!.body, false);

    await byName.memory_recall.execute("id", { query: "auth", scope: "subtree" });
    assert.equal("scope" in calls.at(-1)!.body, false);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memory_recall surfaces read provenance (namespace + from) on each hit", async () => {
  const realFetch = globalThis.fetch;
  try {
    const { byName, reply } = await collectTools();
    reply({
      results: [
        { memory: { id: "m1", content: "own", tier: "semantic", namespace: "acme/api" }, score: 0.9 },
        { memory: { id: "m2", content: "inherited", tier: "semantic", namespace: "acme" }, score: 0.5, from: "acme" },
      ],
    });
    const out = await byName.memory_recall.execute("id", { query: "q" });
    const { results } = JSON.parse(out.content[0].text);
    assert.equal("from" in results[0], false);
    assert.equal(results[0].namespace, "acme/api");
    assert.equal(results[1].from, "acme");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("memory_remember forwards visibility verbatim — the server owns the ancestor vocabulary", async () => {
  const realFetch = globalThis.fetch;
  try {
    const { byName, calls, reply } = await collectTools();
    reply({ id: "m1" });
    await byName.memory_remember.execute("id", { content: "fact", visibility: "personal" });
    assert.equal(calls.at(-1)!.body.visibility, "personal");

    await byName.memory_remember.execute("id", { content: "fact", visibility: "acme" });
    assert.equal(calls.at(-1)!.body.visibility, "acme");

    await byName.memory_remember.execute("id", { content: "fact" });
    assert.equal("visibility" in calls.at(-1)!.body, false);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("a rejected visibility returns the server's error (it enumerates the valid chain)", async () => {
  const realFetch = globalThis.fetch;
  const realError = console.error;
  console.error = () => {};
  try {
    const { byName, reply } = await collectTools();
    reply('{"error":"remember: visibility \\"widgets\\" not in scope; valid: project, personal, acme"}', false);
    const out = await byName.memory_remember.execute("id", { content: "fact", visibility: "widgets" });
    const res = JSON.parse(out.content[0].text);
    assert.equal(res.success, false);
    assert.match(res.error, /valid: project, personal, acme/);
  } finally {
    globalThis.fetch = realFetch;
    console.error = realError;
  }
});

test("memory_briefing orients from the Scope line without naming a namespace", async () => {
  const realFetch = globalThis.fetch;
  try {
    const { byName, calls, reply } = await collectTools();
    reply({
      namespace: "acme/api",
      scope_header: "Scope: acme/api ← acme(4) ← personal(2)",
      pinned: [{ memory: { id: "p1", content: "pinned", tier: "semantic" } }],
      facts: [{ memory: { id: "f1", content: "org fact", tier: "semantic", namespace: "acme" }, from: "acme" }],
    });
    const out = await byName.memory_briefing.execute("id", { scope: "full" });
    assert.match(calls.at(-1)!.url, /\/v1\/namespaces\/briefing\?scope=full$/);
    const res = JSON.parse(out.content[0].text);
    assert.equal(res.scope_header, "Scope: acme/api ← acme(4) ← personal(2)");
    assert.equal(res.pinned[0].id, "p1");
    assert.equal(res.facts[0].from, "acme");
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("a briefing against an unreachable server answers instead of throwing into the host", async () => {
  const realFetch = globalThis.fetch;
  const realError = console.error;
  console.error = () => {};
  try {
    const { byName } = await collectTools();
    globalThis.fetch = (async () => { throw new Error("ECONNREFUSED"); }) as any;
    const out = await byName.memory_briefing.execute("id", {});
    assert.equal(JSON.parse(out.content[0].text).briefing, null);
  } finally {
    globalThis.fetch = realFetch;
    console.error = realError;
  }
});

test("memory_remember surfaces reinforced so a no-op write is not reported as a new save", async () => {
  const realFetch = globalThis.fetch;
  try {
    const { byName, reply } = await collectTools();
    reply({ id: "existing-1", reinforced: true });
    let out = await byName.memory_remember.execute("id", { content: "known fact" });
    assert.deepEqual(JSON.parse(out.content[0].text), { id: "existing-1", success: true, reinforced: true });

    reply({ id: "new-1" });
    out = await byName.memory_remember.execute("id", { content: "novel fact" });
    assert.equal("reinforced" in JSON.parse(out.content[0].text), false);
  } finally {
    globalThis.fetch = realFetch;
  }
});
