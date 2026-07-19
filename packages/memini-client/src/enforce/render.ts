/**
 * Injection rendering: the recall-hit bullet formatter, the [m:id8] handle,
 * the memini-tag escape, the truncation helper, and the shared drop-footer /
 * detail-header wordings.
 *
 * Ported VERBATIM from plugin/scripts/_shared.mjs (with the footer wordings
 * lifted out of the hooks' inline template literals); semantics are pinned by
 * vectors/enforcement.json. Pure: no fs, no env, no clock.
 */

/**
 * The one comment line a recall/pretool block adds — after its opening
 * comment — when anything in it was truncated (server-concise or the client's
 * 240-char cap) or dropped by the token budget: it teaches the model that the
 * bullets are summaries and memory_get with an [m:…] id recovers full text.
 * ONE shared byte-identical constant across surfaces, deliberately: any
 * per-block variation would bust the prompt prefix cache for zero information.
 * Never emitted on the briefing — its ~21 fires/day don't earn the
 * instruction cost; truncation lives on the recall surfaces.
 */
export const RECALL_DETAIL_HEADER = "<!-- summaries; full text: memory_get with the id from [m:…] -->";

/**
 * Neutralize memini wrapper tags inside untrusted stored content before it is
 * rendered into an injected block. Memory-poisoning defense (Unit 42 / MINJA):
 * a memory whose content carries `</memini-context>` or `<memini-memory-directive>`
 * could otherwise break out of its wrapper and masquerade as a harness directive.
 * We entity-escape only the leading "<" of any `<memini` / `</memini` sequence
 * (case-insensitive); generic angle brackets stay as-is so real code snippets
 * (e.g. `Promise<memory>`, `<div>`) render unmangled.
 */
export function escapeMeminiTags(content: string): string;
export function escapeMeminiTags<T>(content: T): T;
export function escapeMeminiTags(content: any): any {
  if (typeof content !== "string") return content;
  return content.replace(/<(\/?)memini/gi, "&lt;$1memini");
}

/**
 * Truncate to `max` bytes, suffix with a marker. Same shape as
 * agentmemory's truncate helper.
 */
export function truncate(value: string, max: number): string;
export function truncate<T>(value: T, max: number): T | string;
export function truncate(value: any, max: number): any {
  if (typeof value === "string") {
    return value.length > max ? value.slice(0, max) + "\n[...truncated]" : value;
  }
  if (value && typeof value === "object") {
    let str;
    try {
      str = JSON.stringify(value);
    } catch {
      return value;
    }
    return str.length > max ? str.slice(0, max) + "...[truncated]" : str;
  }
  return value;
}

/**
 * The [m:<first 8 id chars>] handle a rendered memory carries — what the
 * block header's memory_get teaching points at. Ids are server-minted
 * hex/uuid, safe to render verbatim. "" when there is no usable id.
 */
export function memoryHandle(id: unknown): string {
  return typeof id === "string" && id ? `[m:${id.slice(0, 8)}]` : "";
}

/**
 * Render one recall hit as a "- (score) [labels] text [m:id8]" bullet.
 * Neutralizes memini wrapper tags in the untrusted recalled content BEFORE
 * the 240-char truncate, so a forged closing tag can't break out of the
 * enclosing injection block (memory-poisoning defense — same rationale as
 * the briefing's formatMemory). The trailing [m:id8] handle (only when the
 * hit carries an id) is what the block header's memory_get teaching points
 * at. Returns null when the hit has no renderable text.
 */
export function formatRecallHit(h: any, labels: Set<string>): string | null {
  const text = escapeMeminiTags(h?.content || h?.summary || "");
  if (!text) return null;
  const h8 = memoryHandle(h?.memory?.id);
  const handle = h8 ? ` ${h8}` : "";
  if (labels.size === 0) {
    return `- (${h.score.toFixed(2)}) ${truncate(text, 240)}${handle}`;
  }
  const tagParts: string[] = [];
  if (labels.has("tier") && h.tier) tagParts.push(h.tier);
  if (labels.has("confidence") && typeof h.memory?.confidence === "number") {
    tagParts.push(`conf=${h.memory.confidence.toFixed(2)}`);
  }
  if (labels.has("reason")) tagParts.push("relevant memory");
  const prefix = tagParts.length ? `[${tagParts.join(" · ")}] ` : "";
  return `- (${h.score.toFixed(2)}) ${prefix}${truncate(text, 240)}${handle}`;
}

/**
 * Did this hit's rendered text lose content? True when the server says so
 * (content_truncated — set only when its concise form actually cut), or when
 * the client's own 240-char render cap in formatRecallHit fires (an old
 * server serves full content; the cap is then the truncation). Checked
 * against the same escaped text formatRecallHit renders, so the two can't
 * disagree about where the cap lands.
 */
export function recallHitTruncated(h: any): boolean {
  if (h?.content_truncated === true || h?.memory?.content_truncated === true) return true;
  const text = escapeMeminiTags(h?.content || h?.summary || "");
  return typeof text === "string" && text.length > 240;
}

/**
 * The recall surfaces' drop footer: what a pretool/prompt block appends when
 * the token budget (server max_tokens and/or the client fitByTokens guard)
 * dropped ranked hits the model never saw.
 */
export function recallDropFooter(dropped: number): string {
  return `[+${dropped} more — memory_recall for detail]`;
}

/**
 * The briefing surface's drop footer: appended per starved section (and once
 * for the server's own max_tokens drops). Visible by design — a trimmed
 * briefing must say so, whichever layer trimmed it.
 */
export function briefingDropFooter(dropped: number): string {
  return `[... ${dropped} item(s) truncated by token budget]`;
}
