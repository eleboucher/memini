import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  TRUNCATION_MARKER,
  truncateForCapture,
  buildTurnCapture,
} from "../src/capture.js";

// The truncation contract lives in test/fixtures/capture-vectors.json and is
// replayed by every implementation of it (this one, the opencode plugin,
// hermes, the Open WebUI filter) — see the fixture's own comment for why four
// exist. Cases go in the fixture, not here, so a case added for one client is
// automatically enforced on the other three.
//
// __dirname isn't available under ESM; recover it from import.meta.url. The
// fixture lives under the SOURCE test/ tree — tsc only compiles .ts, so from
// the compiled dist/test/ location this walks back to the package root.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const fixturePath = path.join(__dirname, "..", "..", "test", "fixtures", "capture-vectors.json");

type Vector = { name: string; text: string; max: number | null; expect: string };
const fixture = JSON.parse(fs.readFileSync(fixturePath, "utf8")) as {
  marker: string;
  cases: Vector[];
};

test("the fixture's marker is this implementation's marker", () => {
  assert.equal(fixture.marker, TRUNCATION_MARKER);
});

for (const v of fixture.cases) {
  test(`capture vector: ${v.name}`, () => {
    // `null` in the fixture stands for "any non-numeric value this language can
    // express" — the case exists because effectiveSetting passes a server's
    // value through uncast.
    assert.equal(truncateForCapture(v.text, v.max as number), v.expect);
  });
}

// Cases below are JS-specific (other languages cannot express these inputs) or
// cover buildTurnCapture, which the fixture does not model.

test("truncateForCapture: every non-numeric cap fails open rather than destroying the text", () => {
  const body = "the entire user turn, every word of it";
  for (const bad of [undefined, null, NaN, "abc", "1000", {}, [], Infinity, -Infinity]) {
    assert.equal(
      truncateForCapture(body, bad as unknown as number),
      body,
      `cap ${JSON.stringify(bad)} must leave the text whole`,
    );
  }
});

test("truncateForCapture: a fractional cap floors rather than rounding up", () => {
  assert.equal(truncateForCapture("abcdef", 3.9), "abc" + TRUNCATION_MARKER);
});

test("truncateForCapture: no lone surrogate survives a cut mid-pair", () => {
  const got = truncateForCapture("👍👍👍👍", 3);
  for (const ch of got) {
    const cp = ch.codePointAt(0)!;
    assert.ok(cp < 0xd800 || cp > 0xdfff, `lone surrogate U+${cp.toString(16)}`);
  }
  assert.equal(Buffer.from(got, "utf8").toString("utf8"), got, "must be valid UTF-8");
});

test("truncateForCapture: finding a short prefix of a huge string does not scan the whole string", () => {
  // Regression: `[...s]` materialized one array entry per code point across the
  // entire input to take the first 1000, which measured ~24ms and tens of MB on
  // a 5MB turn — on every capture. Generous bound; the point is O(cap), not O(len).
  const big = "x".repeat(5_000_000);
  const t0 = process.hrtime.bigint();
  truncateForCapture(big, 1000);
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.ok(ms < 5, `truncating a 5MB string to 1000 chars took ${ms.toFixed(1)}ms — is it scanning the whole string?`);
});

test("buildTurnCapture: applies each side's own bound and joins with a blank line", () => {
  assert.equal(buildTurnCapture("uuuuu", "aaaaa", 2, 3), `uu${TRUNCATION_MARKER}\n\naaa${TRUNCATION_MARKER}`);
});

test("buildTurnCapture: 0 on one side leaves that side whole", () => {
  assert.equal(buildTurnCapture("uuuuu", "aaaaa", 0, 3), `uuuuu\n\naaa${TRUNCATION_MARKER}`);
  assert.equal(buildTurnCapture("uuuuu", "aaaaa", 2, 0), `uu${TRUNCATION_MARKER}\n\naaaaa`);
});
