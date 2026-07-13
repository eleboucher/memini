import { test } from "node:test";
import assert from "node:assert/strict";

import {
  deriveNamespace,
  sanitizeNamespace,
  resolveConfig,
  resolveProjectNamespace,
  registerMeminiCommands,
  formatResults,
  fitByTokens,
  approxTokens,
  meminiListPath,
  extractMessageText,
  extractLastAssistantText,
  buildTurnContent,
} from "../src/index.ts";
import { readOverride, writeOverride } from "@memini/client";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Hermetic: the resolver reads $XDG_CONFIG_HOME/memini/config.json and the
// override lives in $XDG_CONFIG_HOME/memini/overrides.json, so point both at a
// temp dir instead of the developer's real config.
const xdgDir = mkdtempSync(join(tmpdir(), "pi-memini-test-"));
process.env["XDG_CONFIG_HOME"] = xdgDir;
// …and a developer who exports MEMINI_NAMESPACE (the fish-universal-variable
// case this whole feature exists for) would otherwise see every default-namespace
// assertion below fail.
delete process.env["MEMINI_NAMESPACE"];

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

test("MEMINI_NAMESPACE is used raw-trimmed, not per-segment sanitized", () => {
  // The server validates the header; an explicit hierarchical value must pass
  // through untouched (only trimmed), matching the canonical resolver.
  assert.equal(resolveConfig({ MEMINI_NAMESPACE: "  team space/eu  " }, "/x/proj").namespace, "team space/eu");
  assert.equal(resolveConfig({ MEMINI_NAMESPACE: "team/eu" }, undefined).namespace, "team/eu");
});

test("resolveConfig: home resolves from MEMINI_HOME env, unset -> undefined", () => {
  assert.equal(resolveConfig({}, "/x/proj").home, undefined);
  assert.equal(resolveConfig({ MEMINI_HOME: "personal/acme" }, "/x/proj").home, "personal/acme");
  assert.equal(resolveConfig({ MEMINI_HOME: "  " }, "/x/proj").home, undefined);
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

test("requests carry X-Memini-Home when MEMINI_HOME is set, omit it otherwise", async () => {
  const { default: meminiExtension } = await import("../src/index.ts?cb=home-" + Date.now());
  const hooks: Record<string, any> = {};
  const requests: any[] = [];
  const realFetch = globalThis.fetch;
  const prevHome = process.env.MEMINI_HOME;
  globalThis.fetch = (async (url: any, init: any) => {
    requests.push({ url: String(url), headers: init?.headers });
    return {
      ok: true,
      status: 200,
      async json() {
        return { results: [] };
      },
      async text() {
        return "";
      },
    };
  }) as any;
  try {
    process.env.MEMINI_HOME = "personal/acme";
    meminiExtension({
      on(name: string, h: any) {
        hooks[name] = h;
      },
      registerTool() {},
    } as any);
    const ctx = { sessionManager: { getSessionId: () => "sess-home", getLeafId: () => "leaf-1" } };
    await hooks.before_agent_start({ prompt: "hello" }, ctx);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].headers["X-Memini-Home"], "personal/acme");
  } finally {
    if (prevHome === undefined) delete process.env.MEMINI_HOME;
    else process.env.MEMINI_HOME = prevHome;
    globalThis.fetch = realFetch;
  }
});

// --- namespace override + commands -------------------------------------------

// A fresh XDG home per test: the override file lives under it, so this keeps the
// developer's real ~/.config/memini/overrides.json out of the assertions.
function tmpEnv(extra: Record<string, string> = {}): NodeJS.ProcessEnv {
  return { XDG_CONFIG_HOME: mkdtempSync(join(tmpdir(), "pi-memini-xdg-")), ...extra };
}

function tmpProject(): string {
  return mkdtempSync(join(tmpdir(), "pi-memini-proj-"));
}

test("the override beats MEMINI_NAMESPACE — the whole point of having one", () => {
  const env = tmpEnv({ MEMINI_NAMESPACE: "global-pin" });
  const cwd = tmpProject();
  assert.equal(resolveProjectNamespace(env, cwd).source, "env");

  writeOverride(cwd, "acme/api", { env: env as Record<string, string | undefined> });

  // A globally exported MEMINI_NAMESPACE pins every repo on the machine; if the
  // env beat the override, /memini:namespace would silently no-op on exactly the
  // machines that need it.
  const eff = resolveProjectNamespace(env, cwd);
  assert.equal(eff.namespace, "acme/api");
  assert.equal(eff.source, "override");
  // …and what the extension actually sends must agree with what it reports.
  assert.equal(resolveConfig(env, cwd).namespace, "acme/api");

  // The counterfactual: the override is a file, so only ignoreOverride can see
  // past it — which is what makes describeSettings' "without the override" line real.
  const without = resolveProjectNamespace(env, cwd, { ignoreOverride: true });
  assert.equal(without.namespace, "global-pin");
  assert.equal(without.source, "env");
});

test("with no override, the chain is env > cwd, and cwd carries its provenance", () => {
  const env = tmpEnv();
  const cwd = tmpProject();
  const derived = resolveProjectNamespace(env, cwd);
  assert.equal(derived.namespace, sanitizeNamespace(cwd.split("/").pop()!));
  assert.equal(derived.source, "cwd");
  assert.equal(resolveProjectNamespace(tmpEnv({ MEMINI_NAMESPACE: "pinned" }), cwd).source, "env");
  // No cwd at all: the "pi" default, honestly labelled.
  assert.deepEqual(resolveProjectNamespace(env, undefined), { namespace: "pi", source: "default" });
});

// A minimal pi host: enough of ExtensionAPI for the commands to register and to
// capture what they would have shown the user.
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
  const ctx = (cwd: string) => ({ cwd, ui: { notify: (m: string) => notified.push(m) } });
  return { pi, commands, shown, notified, ctx };
}

test("memini:namespace shows, sets, and clears the override, and the plugin follows it live", async () => {
  const cwd = tmpProject();
  const prevXdg = process.env.XDG_CONFIG_HOME;
  process.env.XDG_CONFIG_HOME = mkdtempSync(join(tmpdir(), "pi-memini-xdg-"));
  try {
    const { pi, commands, shown, ctx } = fakePi();
    const cfg = resolveConfig(process.env, cwd);
    registerMeminiCommands(pi as any, cfg, () => {});
    assert.deepEqual(Object.keys(commands).sort(), ["memini:namespace", "memini:status"]);

    await commands["memini:namespace"]("", ctx(cwd));
    assert.match(shown.at(-1)!, /No override — resolving automatically/);

    await commands["memini:namespace"]("acme/api", ctx(cwd));
    assert.match(shown.at(-1)!, /namespace override set: .* -> acme\/api/);
    assert.equal(readOverride(cwd, { env: process.env })?.namespace, "acme/api");
    // The client reads cfg.namespace on every request, so the next recall/capture
    // already targets the override — no restart, no split brain.
    assert.equal(cfg.namespace, "acme/api");

    await commands["memini:namespace"]("--clear", ctx(cwd));
    assert.match(shown.at(-1)!, /namespace override cleared: acme\/api -> /);
    assert.equal(readOverride(cwd, { env: process.env }), undefined);
    assert.equal(cfg.namespace, resolveConfig(process.env, cwd).namespace);

    // Nothing to clear is stated, not silently succeeded.
    await commands["memini:namespace"]("--clear", ctx(cwd));
    assert.match(shown.at(-1)!, /nothing to clear/);
  } finally {
    if (prevXdg === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = prevXdg;
  }
});

test("memini:namespace refuses a header-injecting namespace instead of normalizing it", async () => {
  const cwd = tmpProject();
  const { pi, commands, notified, shown, ctx } = fakePi();
  registerMeminiCommands(pi as any, resolveConfig(process.env, cwd), () => {});
  // The namespace rides on the X-Memini-Namespace header: CR/LF would split it.
  await commands["memini:namespace"]("evil\r\nX-Evil: 1", ctx(cwd));
  assert.match(notified.at(-1)!, /invalid namespace/);
  assert.equal(shown.length, 0, "an invalid namespace must not be written or reported as set");
  assert.equal(readOverride(cwd, { env: process.env }), undefined);
});

test("memini:status reports the read set, redacts the bearer, and reads a 404 /healthz as not-exposed", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  const prev = { url: process.env.MEMINI_BASE_URL, key: process.env.MEMINI_API_KEY };
  const requests: { url: string; headers: any }[] = [];
  globalThis.fetch = (async (url: any, init: any) => {
    requests.push({ url: String(url), headers: init?.headers });
    if (String(url).includes("/v1/namespaces/read-set")) {
      return {
        ok: true,
        status: 200,
        async json() {
          return { entries: [{ namespace: "acme/api", origin: "self", tiers: [] }] };
        },
      };
    }
    // A remote memini behind an ingress routes only /v1 and /mcp: /healthz 404s
    // while the server is perfectly healthy.
    return { ok: false, status: 404, async json() { return {}; }, async text() { return ""; } };
  }) as any;
  try {
    process.env.MEMINI_BASE_URL = "http://localhost:8080";
    process.env.MEMINI_API_KEY = "sk-abcdefghijklmnop4f2a";
    const { pi, commands, shown, ctx } = fakePi();
    registerMeminiCommands(pi as any, resolveConfig(process.env, cwd), () => {});
    await commands["memini:status"]("", ctx(cwd));

    const out = shown.at(-1)!;
    // Reachability comes from the read set, never from /healthz.
    assert.match(out, /reachable\s+yes/);
    assert.match(out, /\/healthz not routed/);
    assert.match(out, /READ SET/);
    assert.match(out, /acme\/api\s+self/);
    // A settings dump is the likeliest place a token gets pasted into an issue.
    assert.doesNotMatch(out, /sk-abcdefghijklmnop4f2a/);
    assert.match(out, /sk-…4f2a/);
    // The probes carry the namespace and the bearer.
    const readSet = requests.find((r) => r.url.includes("read-set"))!;
    assert.equal(readSet.headers["X-Memini-Namespace"], resolveConfig(process.env, cwd).namespace);
    assert.equal(readSet.headers.Authorization, "Bearer sk-abcdefghijklmnop4f2a");
  } finally {
    globalThis.fetch = realFetch;
    if (prev.url === undefined) delete process.env.MEMINI_BASE_URL;
    else process.env.MEMINI_BASE_URL = prev.url;
    if (prev.key === undefined) delete process.env.MEMINI_API_KEY;
    else process.env.MEMINI_API_KEY = prev.key;
  }
});

test("memini:status reports an unreachable server rather than throwing into the host", async () => {
  const cwd = tmpProject();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => {
    throw new Error("ECONNREFUSED");
  }) as any;
  try {
    const { pi, commands, shown, notified, ctx } = fakePi();
    registerMeminiCommands(pi as any, resolveConfig(process.env, cwd), () => {});
    await commands["memini:status"]("", ctx(cwd));
    assert.match(shown.at(-1)!, /reachable\s+NO/);
    assert.deepEqual(notified, [], "an unreachable server is a finding, not a command failure");
  } finally {
    globalThis.fetch = realFetch;
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
