/**
 * Turn-capture text bounds.
 *
 * The user and assistant sides of a captured turn are truncated before the
 * write, under the server-resolved `capture_user_max_chars` /
 * `capture_assistant_max_chars` settings. Every JS/TS integration shares this
 * one implementation: the cut has three ways to go wrong and each of them is
 * silent, so it is not worth reproducing per client.
 */

/** The marker appended to a cut, matching the Go importer's truncateRunes. */
export const TRUNCATION_MARKER = "\n[...truncated]";

/**
 * Truncates `s` to `max` characters, appending TRUNCATION_MARKER when it cuts.
 * Returns `s` unchanged when `max <= 0` (uncapped) or when it already fits.
 *
 * Three details this exists to get right, all of which fail quietly:
 *
 *  - **`max = 0` means uncapped, not empty.** `"abc".slice(0, 0)` is `""`, so
 *    passing an uncapped setting straight to slice() captures nothing at all
 *    and the turn lands as an empty memory.
 *  - **Counts characters, not UTF-16 code units.** `String.prototype.slice`
 *    indexes code units, so a cut landing between the halves of an astral
 *    character's surrogate pair (emoji, some CJK extensions) emits a lone
 *    surrogate, which is not valid UTF-8 on the wire. Spreading into an array
 *    iterates by code point, so a character is never split. This also makes the
 *    bound agree with the server, which counts runes.
 *  - **A cut is marked.** Captured turns are recalled into an LLM's context
 *    later; a silently half-cut sentence reads exactly like a complete one, so
 *    the model answers confidently from a fragment. The marker is the only
 *    signal that the text is partial.
 */
export function truncateForCapture(s: string, max: number): string {
  // Anything that is not a positive finite number means "no cap". That covers 0
  // (uncapped by contract) and negatives, but also NaN, null, undefined, and
  // strings — effectiveSetting hands a server's value through as a bare cast
  // (`server[knob.wireKey] as T`), so a wrong type reaches here unvalidated. A
  // `max <= 0` test lets those through, and then `slice(0, NaN)` returns nothing
  // and the whole turn is replaced by the marker. Failing open (store the text)
  // is the only safe direction: this is the last step before the write.
  if (typeof max !== "number" || !Number.isFinite(max) || max <= 0) return s;
  const cap = Math.floor(max);

  // Fast path, and the reason this never allocates for a turn that fits: UTF-16
  // length is always >= the code-point count, so `s.length <= cap` proves it
  // fits without counting anything. Same trick as internal/embed/batched.go.
  if (s.length <= cap) return s;

  // Walk to the cut instead of spreading: `[...s]` materializes one array entry
  // per code point across the whole string to find an O(cap) prefix, which on a
  // multi-megabyte assistant turn is tens of milliseconds and tens of MB, on
  // every capture. Stepping by code point keeps a surrogate pair intact.
  let i = 0;
  for (let n = 0; i < s.length && n < cap; n++) {
    i += (s.codePointAt(i) as number) > 0xffff ? 2 : 1;
  }
  if (i >= s.length) return s; // cap lands past the end: nothing to cut, nothing to mark
  return s.slice(0, i) + TRUNCATION_MARKER;
}

/**
 * Builds the stored body of a captured turn from its two sides, applying each
 * side's bound. Callers pass the resolved settings values; 0 on either side
 * captures that side whole.
 */
export function buildTurnCapture(
  userText: string,
  assistantText: string,
  userMax: number,
  assistantMax: number,
): string {
  return `${truncateForCapture(userText, userMax)}\n\n${truncateForCapture(assistantText, assistantMax)}`;
}
