import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  injectedSuppressed,
  cooldownIds,
  mergeInjectedStates,
  approxTokens,
  fitByTokens,
  injectedIdentity,
  formatRecallHit,
  pretoolFingerprint,
  briefingContentHash,
} from "../src/enforce/index.js";

// The enforcement contract lives in vectors/enforcement.json, generated ONCE
// by running the original plugin/scripts/_shared.mjs implementations and
// pinned since. This suite VERIFIES the TypeScript port against that file —
// it never regenerates it — so any future port (or an accidental "improvement"
// here) fails against the same vectors the hooks were cut from.
//
// __dirname isn't available under ESM; recover it from import.meta.url. The
// vectors live under the package root — tsc compiles into dist/test/, so walk
// two levels back.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const vectorsPath = path.join(__dirname, "..", "..", "vectors", "enforcement.json");

type Vector = { fn: string; name: string; input: any; expected: any };
const vectors = JSON.parse(fs.readFileSync(vectorsPath, "utf8")) as Vector[];

// Per-fn adapters: the vectors hold JSON-representable inputs; anything
// richer (formatRecallHit's labels Set) is rebuilt here. Every adapter calls
// the REAL exported function — no reimplementation.
const run: Record<string, (input: any) => unknown> = {
  injectedSuppressed: (i) => injectedSuppressed(i.entry, i.identity, i.opts),
  cooldownIds: (i) => cooldownIds(i.state, i.opts),
  mergeInjectedStates: (i) => mergeInjectedStates(i.disk, i.mem, i.now),
  approxTokens: (i) => approxTokens(i.text),
  fitByTokens: (i) => fitByTokens(i.items, i.maxTokens),
  injectedIdentity: (i) => injectedIdentity(i.m),
  formatRecallHit: (i) => formatRecallHit(i.h, new Set<string>(i.labels)),
  pretoolFingerprint: (i) => pretoolFingerprint(i.file, i.hits),
  briefingContentHash: (i) => briefingContentHash(i.briefing),
};

test("every vector fn has an adapter (and no stale adapters linger)", () => {
  const fns = new Set(vectors.map((v) => v.fn));
  assert.deepEqual([...fns].sort(), Object.keys(run).sort());
});

for (const v of vectors) {
  test(`enforcement vector: ${v.fn} / ${v.name}`, () => {
    const adapter = run[v.fn];
    assert.ok(adapter, `no adapter for vector fn "${v.fn}"`);
    assert.deepEqual(adapter(v.input), v.expected);
  });
}
