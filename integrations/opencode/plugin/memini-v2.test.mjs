// Tests for the opencode v2 plugin (memini-v2.js): the Plugin.define / setup
// wiring around the shared helpers in memini.js. The memini server is stubbed
// via globalThis.fetch; ctx is a hand-rolled mock of the v2 API as verified
// against opencode2 v0.0.0-next-16502 (see the header of memini-v2.js):
//   - the recall hook fires as "context" (docs wrongly say "request"),
//   - system holds { type: "text", text } parts,
//   - there is no session.idle: turns are reconstructed from
//     session.input.admitted / session.text.ended / session.execution.*.

import { test } from "node:test";
import assert from "node:assert/strict";

import meminiV2, { setup, extractQueryFromRequest, injectContext, resetForTests, turnKey } from "./memini-v2.js";

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
// build whose ctx lacks session/tool/event. session.get mimics v2's
// ctx.session.get({ sessionID }) returning a bare SessionInfo ({ id, parentID? }).
function makeCtx({ options = {}, withHooks = true, sessionInfo = {}, ancestryResponse } = {}) {
  const state = { hooks: {}, tool: null, emitted: [] };
  const ctx = { options };
  if (withHooks) {
    ctx.session = {
      hook: async (name, cb) => {
        state.hooks[name] = cb;
        return { dispose() {} };
      },
      get: async (input) => ancestryResponse ?? { ...sessionInfo, id: input?.sessionID || "s1" },
    };
    ctx.tool = {
      transform: async (fn) => {
        fn({ add: (decl) => (state.tool = decl) });
        return { dispose() {} };
      },
    };
    ctx.event = {
      subscribe: () =>
        (async function* () {
          for (const e of state.emitted) yield e;
        })(),
    };
  }
  return { ctx, state };
}

// fireHook invokes the registered context hook (the name this build fires).
const fireHook = (state, event) => state.hooks["context"](event);

// turnEvent builds a context-hook event. The user message id identifies the
// TURN: tool-loop continuations re-fire with the same last user message.
function turnEvent({ sessionID = "s1", userID = "u1", text = "q", system }) {
  return {
    sessionID,
    system: system ?? [],
    messages: [{ id: userID, role: "user", content: [{ type: "text", text }] }],
  };
}

// turnEvents is the event-stream sequence for one completed user turn.
function turnEvents({ sessionID = "s1", userText = "q", assistantText = "a", aid = "a1", end = "session.execution.succeeded" }) {
  const events = [
    { type: "session.input.admitted", data: { sessionID, input: { type: "user", data: { text: userText } } } },
    { type: "session.text.ended", data: { sessionID, assistantMessageID: aid, ordinal: 0, text: assistantText } },
  ];
  if (end) events.push({ type: end, data: { sessionID } });
  return events;
}

const BASE_ENV = () => {
  resetForTests();
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

test("turnKey is stable per turn and differs across turns and sessions", () => {
  const a = turnEvent({ userID: "u1", text: "same" });
  const b = turnEvent({ userID: "u1", text: "same" });
  assert.equal(turnKey(a, "same"), turnKey(b, "same"), "same last user message id = same turn");
  const nextTurn = turnEvent({ userID: "u2", text: "same" });
  assert.notEqual(turnKey(a, "same"), turnKey(nextTurn, "same"));
  const otherSession = turnEvent({ sessionID: "s2", userID: "u1", text: "same" });
  assert.notEqual(turnKey(a, "same"), turnKey(otherSession, "same"));
});

test("injectContext prefers system[] as a text part, falls back to a system message", () => {
  const withSystem = { system: [], messages: [] };
  assert.equal(injectContext(withSystem, "BLOCK"), true);
  assert.deepEqual(withSystem.system, [{ type: "text", text: "BLOCK" }]);

  const noSystem = { messages: [{ role: "user", content: "q" }] };
  assert.equal(injectContext(noSystem, "BLOCK"), true);
  assert.equal(noSystem.messages[0].role, "system");
  assert.equal(noSystem.messages[0].content, "BLOCK");

  assert.equal(injectContext({}, "BLOCK"), false);
});

test("structured v2 logging does not write to console.error", async () => {
  BASE_ENV();
  const realError = console.error;
  const consoleErrors = [];
  const calls = [];
  console.error = (...args) => consoleErrors.push(args);
  try {
    const { ctx } = makeCtx({ withHooks: false });
    ctx.app = { log: async (entry) => calls.push(entry) };
    await setup(ctx);
    await drain();
    assert.ok(calls.length > 0, "structured logger was used");
    assert.deepEqual(calls[0], {
      body: { service: "memini", level: "warn", message: "recall unavailable: ctx.session.hook is not present on this opencode build" },
    });
    assert.deepEqual(consoleErrors, []);
  } finally {
    console.error = realError;
  }
});

test("v2 logging ignores rejected or absent structured loggers", async () => {
  BASE_ENV();
  const realError = console.error;
  const consoleErrors = [];
  console.error = (...args) => consoleErrors.push(args);
  try {
    const rejected = makeCtx({ withHooks: false });
    rejected.ctx.app = { log: async () => { throw new Error("logger unavailable"); } };
    await assert.doesNotReject(() => setup(rejected.ctx));
    await assert.doesNotReject(() => setup(makeCtx({ withHooks: false }).ctx));
    await drain();
    assert.deepEqual(consoleErrors, []);
  } finally {
    console.error = realError;
  }
});

test("context hook injects a recalled memory into event.system", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch({
    search: [{ memory: { id: "m1", content: "the deploy key lives in vault", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    assert.equal(typeof state.hooks["context"], "function", "context hook registered");
    assert.equal(typeof state.hooks["request"], "function", "docs-named hook registered too (inert on this build)");

    const event = turnEvent({ text: "where is the deploy key" });
    await fireHook(state, event);

    assert.equal(event.system.length, 1);
    assert.equal(event.system[0].type, "text");
    assert.match(event.system[0].text, /Relevant long-term memory from memini/);
    assert.match(event.system[0].text, /the deploy key lives in vault/);
    assert.equal(posts.length, 0, "recall must not write");
  } finally {
    restore();
  }
});

test("a tool-loop continuation of the same turn re-injects the cached block without a second search", async () => {
  BASE_ENV();
  let searches = 0;
  const original = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    const json = (body, status = 200) =>
      new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
    if (path.endsWith("/v1/handshake")) return json({});
    if (path.endsWith("/v1/search")) {
      searches++;
      return json({ results: [{ memory: { id: "m1", content: "continuity fact", tier: "semantic" }, score: 0.9 }] });
    }
    return json({}, 404);
  };
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);

    const first = turnEvent({ userID: "u1", text: "q" });
    await fireHook(state, first);
    assert.equal(first.system.length, 1, "the turn's first dispatch injects");

    // Same turn, second dispatch: messages gained tool results, last user message unchanged.
    const step = {
      sessionID: "s1",
      system: [],
      messages: [
        { id: "u1", role: "user", content: [{ type: "text", text: "q" }] },
        { role: "assistant", content: [{ type: "tool-call", id: "c1", name: "shell", input: {} }] },
        { role: "tool", content: [{ type: "tool-result", id: "c1", name: "shell", result: "ok" }] },
      ],
    };
    await fireHook(state, step);
    assert.equal(searches, 1, "no second search within one turn");
    assert.equal(step.system.length, 1, "the continuation dispatch keeps the memory block");
    assert.match(step.system[0].text, /continuity fact/);

    // The next user message is a new turn: search again.
    const next = turnEvent({ userID: "u2", text: "next question" });
    await fireHook(state, next);
    assert.equal(searches, 2, "a new user message starts a new turn");
  } finally {
    globalThis.fetch = original;
  }
});

test("a build firing both hook names injects a dispatch only once", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    const event = turnEvent({ text: "q" });
    await state.hooks["context"](event);
    await state.hooks["request"](event);
    assert.equal(event.system.length, 1, "double fire is neutralized");
  } finally {
    restore();
  }
});

test("context hook discards late recall results while budget zero remains blocking", async () => {
  BASE_ENV();
  const original = globalThis.fetch;
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  let calls = 0;
  globalThis.fetch = async (url) => {
    const path = new URL(url).pathname;
    if (path.endsWith("/v1/handshake")) return new Response("{}", { status: 200 });
    if (path.endsWith("/v1/search")) {
      calls++;
      if (calls === 1) await gate;
      return new Response(JSON.stringify({ results: [{ memory: { id: `m${calls}`, content: "late hit", tier: "semantic" }, score: 1 }] }), { status: 200 });
    }
    return new Response("{}", { status: 200 });
  };
  try {
    const fast = makeCtx({ options: { recall_budget_ms: 10 } });
    await setup(fast.ctx);
    const first = turnEvent({ sessionID: "s1", userID: "u1" });
    await fireHook(fast.state, first);
    assert.equal(first.system.length, 0);
    release();
    await new Promise((resolve) => setTimeout(resolve, 20));

    const blocking = makeCtx({ options: { recall_budget_ms: 0 } });
    await setup(blocking.ctx);
    const second = turnEvent({ sessionID: "s2", userID: "u1" });
    await fireHook(blocking.state, second);
    assert.equal(second.system.length, 1, "budget zero waits for same-turn recall");
  } finally {
    globalThis.fetch = original;
  }
});

// The floor rides the wire as min_rank_score (server-enforced final composite
// score), never the fused-scale min_score. A server that accepts it is
// authoritative: its result set is NOT re-filtered client-side.
test("context hook floors on min_rank_score (never min_score) and trusts an enforcing server", async () => {
  BASE_ENV();
  const searches = [];
  const original = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    const json = (b, status = 200) => new Response(JSON.stringify(b), { status, headers: { "content-type": "application/json" } });
    if (path.endsWith("/healthz")) return new Response("ok", { status: 200 });
    if (path.endsWith("/v1/handshake")) return json({});
    if (path.endsWith("/v1/search")) {
      searches.push(init && init.body ? JSON.parse(init.body) : {});
      return json({ results: [
        { memory: { id: "hi", content: "high relevance fact", tier: "semantic" }, score: 0.9 },
        { memory: { id: "lo", content: "low relevance kept", tier: "episodic" }, score: 0.1 },
      ] });
    }
    return json({}, 404);
  };
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns", recall_min_score: 0.4 } });
    await setup(ctx);
    const event = turnEvent({ text: "q" });
    await fireHook(state, event);
    assert.equal(searches[0].min_rank_score, 0.4, "the knob rides as min_rank_score");
    assert.equal(searches[0].min_score, undefined, "the fused-scale min_score is never sent");
    assert.equal(event.system.length, 1);
    assert.match(event.system[0].text, /high relevance fact/);
    assert.match(event.system[0].text, /low relevance kept/, "an enforcing server's result set is authoritative");
  } finally {
    globalThis.fetch = original;
  }
});

// Older server: it 400s min_rank_score, so one retry strips it and the client
// applies the composite floor as a fallback.
test("context hook retries without min_rank_score on an old server and applies the floor client-side", async () => {
  BASE_ENV();
  const searches = [];
  const original = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    const json = (b, status = 200) => new Response(JSON.stringify(b), { status, headers: { "content-type": "application/json" } });
    if (path.endsWith("/healthz")) return new Response("ok", { status: 200 });
    if (path.endsWith("/v1/handshake")) return json({});
    if (path.endsWith("/v1/search")) {
      const body = init && init.body ? JSON.parse(init.body) : {};
      searches.push(body);
      if (body.min_rank_score !== undefined) return json({ error: 'unknown field "min_rank_score"' }, 400);
      return json({ results: [
        { memory: { id: "hi", content: "high relevance fact", tier: "semantic" }, score: 0.9 },
        { memory: { id: "lo", content: "low should be filtered", tier: "episodic" }, score: 0.1 },
      ] });
    }
    return json({}, 404);
  };
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns", recall_min_score: 0.4 } });
    await setup(ctx);
    const event = turnEvent({ text: "q" });
    await fireHook(state, event);
    assert.equal(searches.length, 2, "one strip-and-retry, then it stops");
    assert.equal(searches[0].min_rank_score, 0.4, "the first attempt carries the floor");
    assert.equal(searches[1].min_rank_score, undefined, "the retry strips min_rank_score");
    assert.equal(searches[1].min_score, undefined, "the retry never resurrects min_score");
    assert.equal(event.system.length, 1);
    assert.match(event.system[0].text, /high relevance fact/);
    assert.doesNotMatch(event.system[0].text, /low should be filtered/, "the stripped floor is enforced client-side");
  } finally {
    globalThis.fetch = original;
  }
});

test("context hook suppresses a memory already injected this session", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one time fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);

    const first = turnEvent({ userID: "u1" });
    await fireHook(state, first);
    assert.equal(first.system.length, 1, "first turn injects");

    const second = turnEvent({ userID: "u2" });
    await fireHook(state, second);
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
    const event = turnEvent({ text: "q" });
    await fireHook(state, event);
    assert.equal(event.system.length, 0);
  } finally {
    restore();
  }
});

test("memini_status tool registers the v2 Tool.Info shape and renders the effective settings", async () => {
  BASE_ENV();
  const { restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);
    assert.equal(state.tool && state.tool.name, "memini_status");
    assert.equal(state.tool.input.type, "object", "raw JSON schema under `input`");
    assert.deepEqual(state.tool.options, { codemode: false });
    const res = await state.tool.execute({});
    const text = res.content[0].text;
    assert.match(text, /effective settings/);
    assert.match(text, /test-ns/);
  } finally {
    restore();
  }
});

test("capture writes the completed turn when the execution succeeds", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    state.emitted = turnEvents({ userText: "how do I deploy", assistantText: "run make deploy" });
    const cleanup = await setup(ctx);
    await drain();

    assert.equal(posts.length, 1, "one capture write");
    assert.match(posts[0].content, /how do I deploy/);
    assert.match(posts[0].content, /run make deploy/);
    assert.equal(posts[0].metadata.session_id, "s1");
    assert.equal(Object.hasOwn(posts[0], "tier"), false);
    assert.equal(posts[0].metadata.session_type, "root");
    assert.deepEqual(posts[0].tags, ["opencode"]);
    await cleanup();
  } finally {
    restore();
  }
});

test("capture marks failed and interrupted executions", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    for (const [end, aid] of [["session.execution.failed", "a1"], ["session.execution.interrupted", "a2"]]) {
      const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
      state.emitted = turnEvents({ aid, userText: `q-${aid}`, assistantText: "partial", end });
      const cleanup = await setup(ctx);
      await drain();
      await cleanup();
    }
    assert.equal(posts.length, 2);
    assert.equal(posts[0].metadata.failed, true);
    assert.equal(posts[1].metadata.failed, true);
  } finally {
    restore();
  }
});

test("capture joins multi-part assistant text by ordinal", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    state.emitted = [
      { type: "session.input.admitted", data: { sessionID: "s1", input: { type: "user", data: { text: "q" } } } },
      { type: "session.text.ended", data: { sessionID: "s1", assistantMessageID: "a1", ordinal: 1, text: "second half" } },
      { type: "session.text.ended", data: { sessionID: "s1", assistantMessageID: "a1", ordinal: 0, text: "first half" } },
      { type: "session.execution.succeeded", data: { sessionID: "s1" } },
    ];
    const cleanup = await setup(ctx);
    await drain();
    assert.equal(posts.length, 1);
    assert.match(posts[0].content, /first half\nsecond half/);
    await cleanup();
  } finally {
    restore();
  }
});

test("capture skips executions with no completed turn (compaction, pre-load turns)", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    state.emitted = [{ type: "session.execution.succeeded", data: { sessionID: "s1" } }];
    const cleanup = await setup(ctx);
    await drain();
    assert.equal(posts.length, 0);
    await cleanup();
  } finally {
    restore();
  }
});

test("capture deduplicates an already-captured assistant message", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    state.emitted = [...turnEvents({}), ...turnEvents({})];
    const cleanup = await setup(ctx);
    await drain();
    assert.equal(posts.length, 1, "a duplicate execution event for the same assistant message writes once");
    await cleanup();
  } finally {
    restore();
  }
});

test("v2 capture skips child sessions by default and opts in with ancestry metadata", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    const first = makeCtx({ sessionInfo: { parentID: "root-1" } });
    first.state.emitted = turnEvents({ sessionID: "child-1" });
    const cleanup = await setup(first.ctx);
    await drain();
    assert.equal(posts.length, 0, "child capture is off by default");
    await cleanup();

    const opted = makeCtx({ options: { capture_child_sessions: true }, sessionInfo: { parentID: "root-1" } });
    opted.state.emitted = turnEvents({ sessionID: "child-1" });
    const cleanupOpted = await setup(opted.ctx);
    await drain();
    assert.equal(posts.length, 1);
    assert.equal(posts[0].metadata.session_type, "child");
    assert.equal(posts[0].metadata.parent_session_id, "root-1");
    await cleanupOpted();
  } finally {
    restore();
  }
});

test("v2 capture fails closed for malformed or mismatched ancestry records", async () => {
  BASE_ENV();
  const { posts, restore } = installFetch();
  try {
    for (const ancestryResponse of [{}, { data: {} }, { error: "not found" }, { data: { id: "other" } }, { data: { parentID: "root" } }]) {
      const made = makeCtx({ ancestryResponse });
      made.state.emitted = turnEvents({});
      const cleanup = await setup(made.ctx);
      await drain();
      await cleanup();
    }
    assert.equal(posts.length, 0);
  } finally {
    restore();
  }
});

test("context hook re-serves a suppressed memory once the cooldown windows lapse", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one time fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    // Time window off, prompt window 2: each user turn is one prompt.
    const { ctx, state } = makeCtx({
      options: { namespace: "test-ns", inject_cooldown_ms: 0, inject_cooldown_prompts: 2 },
    });
    await setup(ctx);

    const first = turnEvent({ userID: "u1" });
    await fireHook(state, first);
    assert.equal(first.system.length, 1, "prompt 1 injects (n=1)");

    const second = turnEvent({ userID: "u2" });
    await fireHook(state, second);
    assert.equal(second.system.length, 0, "prompt 2: delta 1 < 2, suppressed");

    const third = turnEvent({ userID: "u3" });
    await fireHook(state, third);
    assert.equal(third.system.length, 1, "prompt 3: delta 2 >= 2, re-served");

    const fourth = turnEvent({ userID: "u4" });
    await fireHook(state, fourth);
    assert.equal(fourth.system.length, 0, "the re-record restarts the window");
  } finally {
    restore();
  }
});

test("context hook re-injects an updated memory inside the window (content-change bypass)", async () => {
  BASE_ENV();
  const original = globalThis.fetch;
  let searches = 0;
  globalThis.fetch = async (url) => {
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
    return json({}, 404);
  };
  try {
    const { ctx, state } = makeCtx({ options: { namespace: "test-ns" } });
    await setup(ctx);

    const first = turnEvent({ userID: "u1" });
    await fireHook(state, first);
    assert.match(first.system[0].text, /version one/);

    const second = turnEvent({ userID: "u2" });
    await fireHook(state, second);
    assert.equal(second.system.length, 1, "an updated memory must re-inject inside the window");
    assert.match(second.system[0].text, /version two/);
  } finally {
    globalThis.fetch = original;
  }
});

test("context hook: both cooldown knobs at zero suppresses for the whole session", async () => {
  BASE_ENV();
  const { restore } = installFetch({
    search: [{ memory: { id: "m1", content: "one time fact", tier: "semantic" }, score: 0.9 }],
  });
  try {
    const { ctx, state } = makeCtx({
      options: { namespace: "test-ns", inject_cooldown_ms: 0, inject_cooldown_prompts: 0 },
    });
    await setup(ctx);
    const first = turnEvent({ userID: "u1" });
    await fireHook(state, first);
    assert.equal(first.system.length, 1);
    for (let i = 2; i <= 6; i++) {
      const again = turnEvent({ userID: `u${i}` });
      await fireHook(state, again);
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
