/**
 * Injection token budget: the cheap token estimator and the head-keeping
 * list trimmer.
 *
 * Ported VERBATIM from plugin/scripts/_shared.mjs; semantics are pinned by
 * vectors/enforcement.json. Pure: no fs, no env, no clock.
 */

/**
 * approxTokens is a cheap token estimator. ~0.75 tokens/word for English-ish
 * content, with a floor of 1 so a single non-empty line never reports 0.
 */
export function approxTokens(text: string): number {
  if (!text) return 0;
  const words = String(text).trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.ceil((words * 4) / 3));
}

/** What fitByTokens returns: the surviving items, their cost, the drop count. */
export interface TokenFit {
  items: string[];
  tokens: number;
  dropped: number;
}

/**
 * fitByTokens trims a list of pre-formatted strings to fit under `maxTokens`,
 * keeping the head (the most-relevant entries first). Returns the trimmed
 * list and the running token total, so callers can render a "[… truncated]"
 * footer when items were dropped.
 */
export function fitByTokens(items: string[], maxTokens: number): TokenFit {
  if (!Array.isArray(items) || items.length === 0) return { items: [], tokens: 0, dropped: 0 };
  if (!Number.isFinite(maxTokens) || maxTokens <= 0) {
    const tokens = items.reduce((sum, s) => sum + approxTokens(s), 0);
    return { items: items.slice(), tokens, dropped: 0 };
  }
  const out: string[] = [];
  let used = 0;
  let dropped = 0;
  for (const s of items) {
    const t = approxTokens(s);
    if (used + t > maxTokens) {
      // If the item fits partially, truncate at the last newline boundary so
      // bullet points stay intact (~4 chars/token).
      const charBudget = (maxTokens - used) * 4;
      if (charBudget > 20) {
        let cut = s.slice(0, charBudget);
        const lastNL = cut.lastIndexOf("\n");
        if (lastNL > 20) cut = cut.slice(0, lastNL);
        if (cut.length > 20) {
          out.push(cut + "\n[...truncated]");
          used += approxTokens(cut);
          continue;
        }
      }
      dropped++;
      continue;
    }
    out.push(s);
    used += t;
  }
  return { items: out, tokens: used, dropped };
}
