import assert from "node:assert/strict";
import { test } from "node:test";
import crypto from "node:crypto";

import {
  isContentHash,
  injectedIdentity,
  pretoolFingerprint,
  briefingContentHash,
} from "../src/enforce/identity.js";
import {
  MAX_INJECTED_IDS,
  normInjectedEntry,
  normalizeInjectedState,
  recordInjected,
  mergeInjectedStates,
  injectedSuppressed,
  cooldownIds,
} from "../src/enforce/seen.js";
import { approxTokens, fitByTokens } from "../src/enforce/budget.js";
import {
  RECALL_DETAIL_HEADER,
  escapeMeminiTags,
  truncate,
  memoryHandle,
  formatRecallHit,
  recallHitTruncated,
  recallDropFooter,
  briefingDropFooter,
} from "../src/enforce/render.js";
import { injectedReport } from "../src/enforce/telemetry.js";

const sha16 = (s: string) => crypto.createHash("sha256").update(s).digest("hex").slice(0, 16);

// ── identity ───────────────────────────────────────────────────────────────

test("isContentHash accepts exactly 16 lowercase hex chars", () => {
  assert.equal(isContentHash("aaaabbbbccccdddd"), true);
  assert.equal(isContentHash("0123456789abcdef"), true);
  for (const bad of ["AAAABBBBCCCCDDDD", "aaaabbbbccccddd", "aaaabbbbccccdddd0", "not-hex-not-real", "", 42, null, undefined]) {
    assert.equal(isContentHash(bad), false, `must reject ${JSON.stringify(bad)}`);
  }
});

test("injectedIdentity: content_hash wins; local recipe is sha256/16 of content-or-summary", () => {
  assert.equal(injectedIdentity({ content: "x", content_hash: "aaaabbbbccccdddd" }), "aaaabbbbccccdddd");
  assert.equal(injectedIdentity({ content: "x", memory: { content_hash: "aaaabbbbccccdddd" } }), "aaaabbbbccccdddd");
  assert.equal(injectedIdentity({ content: "the fact" }), sha16("the fact"));
  assert.equal(injectedIdentity({ summary: "the summary" }), sha16("the summary"));
  assert.equal(injectedIdentity({}), sha16(""));
  // Cross-format stability: a briefing item (full content) and a concise hit
  // (truncated content) of the same memory share the server hash.
  const briefed = { id: "m9", content: "full text well past a render cap", content_hash: "0123456789abcdef" };
  const concise = { content: "full te…", memory: { id: "m9", content_hash: "0123456789abcdef" } };
  assert.equal(injectedIdentity(briefed), injectedIdentity(concise));
});

test("pretoolFingerprint hashes {file, items:[{id,h}]} — stable across render shape, sensitive to content", () => {
  const hits = [{ content: "alpha", memory: { id: "m1" } }];
  const a = pretoolFingerprint("f.go", hits);
  assert.match(a, /^[0-9a-f]{64}$/);
  // Extra render-only fields don't move the fingerprint…
  assert.equal(pretoolFingerprint("f.go", [{ content: "alpha", score: 0.9, tier: "semantic", memory: { id: "m1" } }]), a);
  // …but the file, the id set, and the content identity all do.
  assert.notEqual(pretoolFingerprint("g.go", hits), a);
  assert.notEqual(pretoolFingerprint("f.go", [{ content: "alpha", memory: { id: "m2" } }]), a);
  assert.notEqual(pretoolFingerprint("f.go", [{ content: "beta", memory: { id: "m1" } }]), a);
  // A missing id rides as null, not undefined (JSON.stringify would drop it).
  assert.equal(
    pretoolFingerprint("f.go", [{ content: "alpha", memory: {} }]),
    crypto
      .createHash("sha256")
      .update(JSON.stringify({ file: "f.go", items: [{ id: null, h: sha16("alpha") }] }))
      .digest("hex"),
  );
});

test("briefingContentHash: sha256/16 over the JSON-serialized briefing", () => {
  const b = { pinned: [], facts: [{ memory: { id: "f1" } }] };
  assert.equal(briefingContentHash(b), crypto.createHash("sha256").update(JSON.stringify(b)).digest("hex").slice(0, 16));
  assert.notEqual(briefingContentHash(b), briefingContentHash({ ...b, facts: [] }));
});

// ── seen ───────────────────────────────────────────────────────────────────

test("normInjectedEntry: requires a string h; backfills at/n", () => {
  assert.deepEqual(normInjectedEntry({ h: "x", at: 5, n: 2 }, 99), { h: "x", at: 5, n: 2 });
  assert.deepEqual(normInjectedEntry({ h: "x" }, 99), { h: "x", at: 99, n: 0 });
  assert.deepEqual(normInjectedEntry({ h: "x", at: "soon", n: "many" }, 99), { h: "x", at: 99, n: 0 });
  for (const bad of [null, "hash", 42, [], { at: 5, n: 2 }, { h: 7 }]) {
    assert.equal(normInjectedEntry(bad, 99), null, `must reject ${JSON.stringify(bad)}`);
  }
});

test("normalizeInjectedState: v2 reads verbatim (junk skipped), v1 flat file migrates, garbage empties", () => {
  const v2 = {
    v: 2,
    n: 7,
    ids: { good: { h: "h1", at: 5, n: 1 }, junk: "nope", alsoJunk: 42, noHash: { at: 5, n: 1 } },
  };
  assert.deepEqual(normalizeInjectedState(v2, 1000), { n: 7, ids: { good: { h: "h1", at: 5, n: 1 } } });
  // v1 flat: { id: hash } → { h, at: now, n: 0 }; non-string values skipped.
  assert.deepEqual(normalizeInjectedState({ m1: "hash-1", m2: 42 }, 1000), {
    n: 0,
    ids: { m1: { h: "hash-1", at: 1000, n: 0 } },
  });
  for (const junk of [null, undefined, [], "text", 42]) {
    assert.deepEqual(normalizeInjectedState(junk, 1000), { n: 0, ids: {} });
  }
});

test("recordInjected stamps {h, at, n} from the state's counter and mutates in place", () => {
  const state: any = { n: 4, ids: {} };
  assert.equal(recordInjected(state, "m1", "hash-1", 1234), state);
  assert.deepEqual(state.ids.m1, { h: "hash-1", at: 1234, n: 4 });
  // A state without ids (or a finite n) still records.
  const bare: any = {};
  recordInjected(bare, "m2", "", 99);
  assert.deepEqual(bare.ids.m2, { h: "", at: 99, n: 0 });
});

test("mergeInjectedStates: per-id larger-at wins, n maxes, cap evicts oldest", () => {
  const disk = { n: 2, ids: { a: { h: "da", at: 10, n: 1 }, b: { h: "db", at: 20, n: 1 } } };
  const mem = { n: 1, ids: { a: { h: "ma", at: 30, n: 1 }, b: { h: "mb", at: 5, n: 1 }, c: { h: "mc", at: 1, n: 0 } } };
  const merged = mergeInjectedStates(disk, mem, 1000);
  assert.equal(merged.v, 2);
  assert.equal(merged.n, 2);
  assert.deepEqual(merged.ids, {
    a: { h: "ma", at: 30, n: 1 }, // mem newer
    b: { h: "db", at: 20, n: 1 }, // disk newer
    c: { h: "mc", at: 1, n: 0 },
  });
  // Non-object mem degrades to "nothing to merge".
  assert.deepEqual(mergeInjectedStates(disk, null, 1000).ids, disk.ids);
  // Cap: MAX_INJECTED_IDS newest-by-at survive.
  const many: Record<string, { h: string; at: number; n: number }> = {};
  for (let i = 0; i < MAX_INJECTED_IDS + 3; i++) many[`i${i}`] = { h: "h", at: i, n: 0 };
  const bounded = mergeInjectedStates({ n: 0, ids: {} }, { n: 0, ids: many }, 1000);
  const keys = Object.keys(bounded.ids);
  assert.equal(keys.length, MAX_INJECTED_IDS);
  assert.ok(!keys.includes("i0") && !keys.includes("i1") && !keys.includes("i2"), "oldest evicted");
  assert.ok(keys.includes(`i${MAX_INJECTED_IDS + 2}`), "newest kept");
});

test("injectedSuppressed core truth table (edges live in the vectors)", () => {
  const W = { now: 10_000, counter: 5, cooldownMs: 5000, cooldownPrompts: 3 };
  assert.equal(injectedSuppressed({ h: "", at: 0, n: 0 }, "anything", W), true, "sentinel forever");
  assert.equal(injectedSuppressed({ h: "h1", at: 9999, n: 5 }, "h2", W), false, "content change re-injects");
  assert.equal(injectedSuppressed({ h: "h1", at: 9999, n: 5 }, "h1", W), true, "inside the time window");
  assert.equal(injectedSuppressed({ h: "h1", at: 0, n: 4 }, "h1", { ...W, cooldownMs: 0 }), true, "inside the prompt window");
  assert.equal(injectedSuppressed({ h: "h1", at: 0, n: 1 }, "h1", { ...W, cooldownMs: 0 }), false, "prompt window lapsed");
  assert.equal(injectedSuppressed(undefined, "h1", W), false, "no entry, no suppression");
});

test("cooldownIds judges by id alone against the state's own counter", () => {
  const state = {
    n: 5,
    ids: {
      sentinel: { h: "", at: 0, n: 0 },
      fresh: { h: "h1", at: 9_999, n: 0 },
      stale: { h: "h1", at: 0, n: 1 },
    },
  };
  assert.deepEqual(cooldownIds(state, { now: 10_000, cooldownMs: 5000, cooldownPrompts: 3 }), ["sentinel", "fresh"]);
  // Junk state shapes yield [] rather than a crash.
  assert.deepEqual(cooldownIds(null, { now: 10_000, cooldownMs: 0, cooldownPrompts: 0 }), []);
});

// ── budget ─────────────────────────────────────────────────────────────────

test("approxTokens: ~0.75 tokens/word with a floor of 1 for non-empty text", () => {
  assert.equal(approxTokens(""), 0);
  assert.equal(approxTokens("x"), 2); // ceil(4/3)
  assert.equal(approxTokens("one two three"), 4);
  assert.equal(approxTokens("   "), 1, "whitespace-only is non-empty: floor applies");
});

test("fitByTokens: keeps the head, partial-trims at a newline, counts drops", () => {
  const items = ["one two three", "four five six"];
  assert.deepEqual(fitByTokens(items, 0), { items, tokens: 8, dropped: 0 });
  assert.notEqual(fitByTokens(items, 0).items, items, "unbounded returns a copy, not the input array");
  assert.deepEqual(fitByTokens(items, 4), { items: ["one two three"], tokens: 4, dropped: 1 });
  // A partially-fitting multi-line item is cut at the last newline and marked.
  const long = "aaaa bbbb cccc dddd\neeee ffff gggg hhhh\niiii jjjj kkkk llll";
  const fit = fitByTokens([long], 10);
  assert.deepEqual(fit.items, ["aaaa bbbb cccc dddd\neeee ffff gggg hhhh\n[...truncated]"]);
  assert.equal(fit.dropped, 0);
  assert.deepEqual(fitByTokens([], 100), { items: [], tokens: 0, dropped: 0 });
});

// ── render ─────────────────────────────────────────────────────────────────

test("escapeMeminiTags neutralizes only memini wrappers; non-strings pass through", () => {
  assert.equal(escapeMeminiTags("</memini-context>"), "&lt;/memini-context>");
  assert.equal(escapeMeminiTags("<MeMiNi-pretool>"), "&lt;memini-pretool>");
  assert.equal(escapeMeminiTags("Promise<memory> and <div>"), "Promise<memory> and <div>");
  assert.equal(escapeMeminiTags(""), "");
  for (const passthrough of [null, undefined, 42, { a: 1 }]) {
    assert.equal(escapeMeminiTags(passthrough as any), passthrough);
  }
});

test("truncate: strings get the newline marker, objects stringify with the inline marker", () => {
  assert.equal(truncate("abcdef", 10), "abcdef");
  assert.equal(truncate("abcdef", 3), "abc\n[...truncated]");
  assert.equal(truncate({ a: "bb" }, 100), '{"a":"bb"}', "a small object stringifies whole");
  assert.equal(truncate({ a: "bb".repeat(20) }, 10), JSON.stringify({ a: "bb".repeat(20) }).slice(0, 10) + "...[truncated]");
  const cyclic: any = {};
  cyclic.self = cyclic;
  assert.equal(truncate(cyclic, 10), cyclic, "unstringifiable objects fail open");
  assert.equal(truncate(42 as any, 1), 42);
});

test("memoryHandle: [m:<first 8>] for a non-empty string id, else empty", () => {
  assert.equal(memoryHandle("0123456789abcdef"), "[m:01234567]");
  assert.equal(memoryHandle("m1"), "[m:m1]");
  for (const bad of ["", null, undefined, 42]) assert.equal(memoryHandle(bad), "");
});

test("formatRecallHit: bullet + handle + optional labels; null without text", () => {
  const none = new Set<string>();
  assert.equal(
    formatRecallHit({ content: "auth decision", score: 0.95, memory: { id: "0123456789abcdef0123" } }, none),
    "- (0.95) auth decision [m:01234567]",
  );
  assert.equal(formatRecallHit({ content: "auth decision", score: 0.95 }, none), "- (0.95) auth decision");
  assert.equal(
    formatRecallHit(
      { content: "d", score: 0.9, tier: "semantic", memory: { id: "abcd1234ef", confidence: 0.87 } },
      new Set(["tier", "confidence", "reason"]),
    ),
    "- (0.90) [semantic · conf=0.87 · relevant memory] d [m:abcd1234]",
  );
  assert.equal(formatRecallHit({ score: 0.5, memory: { id: "abcd" } }, none), null);
});

test("recallHitTruncated: server flag or the 240-char render cap (post-escape)", () => {
  assert.equal(recallHitTruncated({ content: "short", content_truncated: true }), true);
  assert.equal(recallHitTruncated({ content: "short", memory: { content_truncated: true } }), true);
  assert.equal(recallHitTruncated({ content: "z".repeat(240) }), false, "exactly at the cap is not truncated");
  assert.equal(recallHitTruncated({ content: "z".repeat(241) }), true);
  assert.equal(recallHitTruncated({ summary: "short" }), false);
});

test("shared wordings are pinned byte-for-byte", () => {
  assert.equal(RECALL_DETAIL_HEADER, "<!-- summaries; full text: memory_get with the id from [m:…] -->");
  assert.equal(recallDropFooter(3), "[+3 more — memory_recall for detail]");
  assert.equal(briefingDropFooter(2), "[... 2 item(s) truncated by token budget]");
});

// ── telemetry ──────────────────────────────────────────────────────────────

test("injectedReport: required fields always present, zero/empty optionals omitted", () => {
  assert.deepEqual(injectedReport({ surface: "pretool", sessionId: "s1" }), {
    session_id: "s1",
    surface: "pretool",
    source: "claude-code",
    injected_ids: [],
  });
  const full = injectedReport({
    surface: "briefing",
    sessionId: "s1",
    ids: ["m1", "", 42 as any, "m2"],
    tokens: 12,
    chars: 340,
    suppressed: { seen: 2, cooldown: 0, budget: undefined, unknownKey: 9 } as any,
  });
  assert.deepEqual(full, {
    session_id: "s1",
    surface: "briefing",
    source: "claude-code",
    injected_ids: ["m1", "m2"],
    injected_tokens_est: 12,
    injected_chars: 340,
    suppressed: { seen: 2 },
  });
  // No arguments at all still yields a well-formed (empty) body.
  assert.deepEqual(injectedReport(), {
    session_id: "",
    surface: undefined,
    source: "claude-code",
    injected_ids: [],
  });
});
