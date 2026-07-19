// Tests for the opencode v2 plugin (memini-v2.js): the Plugin.define / setup
// wiring around the shared helpers in memini.js. The memini server is stubbed
// via globalThis.fetch; ctx is a hand-rolled mock of the documented v2 API.

import { test } from "node:test";
import assert from "node:assert/strict";

import meminiV2, { setup, extractQueryFromRequest, injectContext } from "./memini-v2.js";

const tick = () => new Promise((r) => setImmediate(r));
const drain = async () => {
  for (let i = 0; i < 5; i++) await tick();
};

// installFetch stubs globalThis.fetch with a path-routed fake memini. `search`
// is the results array returned by /v1/search; `posts` collects capture bodies.
function installFetch({ search = [] } = {}) {
  const posts = [];
  const original = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    const json = (body, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    if (path.endsWith("/healthz")) return new Response("ok", { status: 200 });
    if (path.endsWith("/v1/handshake")) return json({});
    if (path.endsWith("/v1/search")) return json({ results: search });
    if (path.endsWith("/v1/memories")) {
      posts.push(JSON.parse(init.body));
      return json({ id: "mem_test" });
    }
    return json({}, 404);
  };
  return { posts, restore: () => (globalThis.fetch = original) };
}

// makeCtx builds a mock v2 plugin context. Set withHooks:false to simulate a
// build whose ctx lacks session/tool/event (the current beta).
function makeCtx({ options = {}, messages = [], withHooks = true } = {}) {
  const state = { requestHook: null, tool: null, events: [], messages };
  const ctx = { options };
  if (withHooks) {
    ctx.session = {
      hook: async (name, cb) => {
        if (name === "request") state.requestHook = cb;
        return { dispose() {} };
      },
      messages: async () => ({ data: state.messages }),
    };
    ctx.tool = {
      transform: async (fn) => {
        fn({ add: (decl) => (state.tool = decl) });
        return { dispose() {} };
      },
    };
    ctx.event = {
      subscribe: (type) => {
        state.events.push(type);
        return (async function* () {
          for (const e of state.emit || []) yield e;
        })();
      },
    };
  }
  return { ctx, state };
}

const BASE_ENV = () => {
  process.env.MEMINI_BASE_URL = "http://memini.test";
  delete process.env.MEMINI_API_KEY;
  delete process.env.MEMINI_NAMESPACE;
};

test("default export is the v2 { id, setup } module shape", () => {
  assert.equal(meminiV2.id, "memini");
  assert.equal(typeof meminiV2.setup, "function");
  assert.equal(meminiV2.server, undefined); // not the v1 shape
});

test("extractQueryFromRequest reads the latest user message across shapes", () => {
  assert.equal(
    extractQueryFromRequest({ messages: [{ role: "user", content: "hello world" }] }),
    "hello world",
  );
  assert.equal(
    extractQueryFromRequest({
      messages: [
        { role: "user", content: "old" },
        { role: "assistant", content: "reply" },
        { role: "user", content: [{ type: "text", text: "newest" }] },
      ],
    }),
    "newest",
  );
  assert.equal(extractQueryFromRequest({ messages: [] }), "");
});

test("injectContext prefers system[], falls back to a system message", () => {
  const withSystem = { system: [], messages: [] };
  assert.equal(injectContext(withSystem, "BLOCK"), true);
  assert.deepEqual(withSystem.system, ["BLOCK"]);

  const noSystem = { messages: [{ role: "user", content: "q" }] };
  assert.equal(injectContext(noSystem, "BLOCK"), true);
  assert.equal(noSystem.messages[0].role, "system");
  assert.equal(noSystem.messages[0].content, "BLOCK");

  assert.equal(injectContext({}, "BLOCK"), false);
});

test("request hook injects a recalled memory into event.system", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch({
    search: [{ memory: { id: "m1", content: "the deploy key lives in vault", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    assert.equal(typeof state.requestHook, "function", "request hook registered");

    const event = { sessionID: "s1", system: [], messages: [{ role: "user", content: "where is the deploy key" }] };
    await state.requestHook(event);

    assert.equal(event.system.length, 1);
    assert.match(event.system[0], /Relevant long-term memory from memini/);
    assert.match(event.system[0], /the deploy key lives in vault/);
    assert.equal(posts.length, 0, "recall must not write");
  } finally {
    restore();
  }
});

test("request hook suppresses a memory already injected this session", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one time fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    const mk = () => ({ sessionID: "s1", system: [], messages: [{ role: "user", content: "q" }] });

    const first = mk();
    await state.requestHook(first);
    assert.equal(first.system.length, 1, "first turn injects");

    const second = mk();
    await state.requestHook(second);
    assert.equal(second.system.length, 0, "second turn suppresses the repeat");
  } finally {
    restore();
  }
});

test("recall:false skips injection entirely", async () => {
  BASE_ENV();
  const { restore } = installFetch({ search: [{ memory: { id: "m1", content: "x" }, score: 1 }] });
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns", recall: false } });
    await setup(ctx);
    const event = { sessionID: "s1", system: [], messages: [{ role: "user", content: "q" }] };
    await state.requestHook(event);
    assert.equal(event.system.length, 0);
  } finally {
    restore();
  }
});

test("memini_status tool renders the effective settings", async () => {
  BASE_ENV();
  const { restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    assert.equal(state.tool && state.tool.name, "memini_status");
    const res = await state.tool.execute({});
    const text = res.content[0].text;
    assert.match(text, /effective settings/);
    assert.match(text, /test-ns/);
    assert.equal(res.structured.namespace, "test-ns");
  } finally {
    restore();
  }
});

test("capture writes the completed turn on session.idle", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({
      options: { namespace: "test-ns" },
      messages: [
        { info: { role: "user", id: "u1" }, parts: [{ type: "text", text: "how do I deploy" }] },
        { info: { role: "assistant", id: "a1" }, parts: [{ type: "text", text: "run make deploy" }] },
      ],
    });
    state.emit = [{ type: "session.idle", properties: { sessionID: "s1" } }];
    const cleanup = await setup(ctx);
    await drain();

    assert.equal(posts.length, 1, "one capture write");
    assert.match(posts[0].content, /how do I deploy/);
    assert.match(posts[0].content, /run make deploy/);
    assert.equal(posts[0].metadata.session_id, "s1");
    assert.deepEqual(posts[0].tags, ["opencode"]);
    await cleanup();
  } finally {
    restore();
  }
});

test("request hook re-serves a suppressed memory once the cooldown windows lapse", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one time fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    // Time window off, prompt window 2: each request-hook fire is one prompt.
    const { ctx, state } = makeCtx({
      options: { namespace: "test-ns", inject_cooldown_ms: 0, inject_cooldown_prompts: 2 },
    });
    await setup(ctx);
    const mk = () => ({ sessionID: "s1", system: [], messages: [{ role: "user", content: "q" }] });

    const first = mk();
    await state.requestHook(first);
    assert.equal(first.system.length, 1, "prompt 1 injects (n=1)");

    const second = mk();
    await state.requestHook(second);
    assert.equal(second.system.length, 0, "prompt 2: delta 1 < 2, suppressed");

    const third = mk();
    await state.requestHook(third);
    assert.equal(third.system.length, 1, "prompt 3: delta 2 >= 2, re-served");

    const fourth = mk();
    await state.requestHook(fourth);
    assert.equal(fourth.system.length, 0, "the re-record restarts the window");
  } finally {
    restore();
  }
});

test("request hook re-injects an updated memory inside the window (content-change bypass)", async () => {
  BASE_ENV();
  const posts = [];
  const original = globalThis.fetch;
  let searches = 0;
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    const json = (body, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    if (path.endsWith("/healthz")) return new Response("ok", { status: 200 });
    if (path.endsWith("/v1/handshake")) return json({});
    if (path.endsWith("/v1/search")) {
      searches++;
      const content = searches <= 1 ? "version one" : "version two — updated";
      return json({ results: [{ memory: { id: "m1", content, tier: "semantic" }, score: 0.9 }] });
    }
    if (path.endsWith("/v1/memories")) {
      posts.push(JSON.parse(init.body));
      return json({ id: "mem_test" });
    }
    return json({}, 404);
  };
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    const mk = () => ({ sessionID: "s1", system: [], messages: [{ role: "user", content: "q" }] });

    const first = mk();
    await state.requestHook(first);
    assert.match(first.system[0], /version one/);

    const second = mk();
    await state.requestHook(second);
    assert.equal(second.system.length, 1, "an updated memory must re-inject inside the window");
    assert.match(second.system[0], /version two/);
  } finally {
    globalThis.fetch = original;
  }
});

test("request hook: both cooldown knobs at zero suppresses for the whole session", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one time fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({
      options: { namespace: "test-ns", inject_cooldown_ms: 0, inject_cooldown_prompts: 0 },
    });
    await setup(ctx);
    const mk = () => ({ sessionID: "s1", system: [], messages: [{ role: "user", content: "q" }] });
    const first = mk();
    await state.requestHook(first);
    assert.equal(first.system.length, 1);
    for (let i = 0; i < 5; i++) {
      const again = mk();
      await state.requestHook(again);
      assert.equal(again.system.length, 0, "legacy forever-dedupe: never re-served");
    }
  } finally {
    restore();
  }
});

test("setup degrades gracefully when ctx lacks session/tool/event", async () => {
  BASE_ENV();
  const { restore } = installFetch();
  try {
    const { ctx } = makeCtx({ options: { namespace: "test-ns" }, withHooks: false });
    const cleanup = await setup(ctx); // must not throw
    assert.equal(typeof cleanup, "function");
    await cleanup();
  } finally {
    restore();
  }
});
