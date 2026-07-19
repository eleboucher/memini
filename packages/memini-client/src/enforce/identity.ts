/**
 * Content-identity primitives for the injection-enforcement core.
 *
 * Ported VERBATIM from plugin/scripts/_shared.mjs (injectedIdentity /
 * isContentHash), pre-tool-use.mjs (the per-file fingerprint recipe), and
 * session-start.mjs (the briefing content hash) — semantics are pinned by
 * vectors/enforcement.json; do not "improve" behavior here without a
 * coordinated change to every consumer.
 *
 * Pure: no fs, no env, no clock. crypto is used only for hashing.
 */

import crypto from "node:crypto";

/**
 * True for a well-formed server-minted content hash: 16 lowercase hex chars
 * (the server's sha256(content||summary).slice(0,16) — the same recipe as the
 * local fallback in injectedIdentity, so the two are interchangeable).
 */
export function isContentHash(s: unknown): s is string {
  return typeof s === "string" && /^[0-9a-f]{16}$/.test(s);
}

/**
 * Content-identity hash for the injected-memory state. Prefers the
 * server-minted `content_hash` when present and well-formed — read off the
 * object itself (a briefing memory) or its nested `memory` (a recall hit) —
 * because the server hashes the FULL content even when it serves a concise
 * form: a briefing's full item and a concise recall hit of the same memory
 * must be the SAME identity, or the seen-filter re-injects what the context
 * already carries. Falls back to hashing the text a recall surface would
 * render (content, falling back to summary) for old servers — the same
 * doctrine as the pretool fingerprint, so an in-place update past any render
 * cap still changes identity and re-injects.
 */
export function injectedIdentity(m: any): string {
  const ch = m?.content_hash ?? m?.memory?.content_hash;
  if (isContentHash(ch)) return ch;
  const text = m?.content || m?.summary || "";
  return crypto.createHash("sha256").update(text).digest("hex").slice(0, 16);
}

/**
 * Fingerprint the SEMANTIC content served for one file: the file path plus
 * the ordered (id, identity) pairs of the hits themselves. INVARIANT: two
 * injections that would show the user the same memories for the same file
 * must fingerprint identically regardless of which tool triggered them and
 * regardless of how the block is rendered — so the hash is built from the
 * hits directly, never from rendered bullet text or the outer wrapper.
 * Per-item identity is injectedIdentity (server content_hash preferred, local
 * hash of untruncated content/summary otherwise): truncation is a display
 * budget, never identity.
 */
export function pretoolFingerprint(file: string, hits: any[]): string {
  const fingerprintInput = JSON.stringify({
    file,
    items: hits.map((h) => ({ id: h.memory?.id || null, h: injectedIdentity(h) })),
  });
  return crypto.createHash("sha256").update(fingerprintInput).digest("hex");
}

/**
 * Content hash of a whole briefing response, for the cache-stable injection
 * guard: sha256/16 over the JSON-serialized briefing, so a byte-for-byte
 * unchanged briefing can be skipped on a repeat SessionStart fire.
 */
export function briefingContentHash(briefing: unknown): string {
  return crypto.createHash("sha256").update(JSON.stringify(briefing)).digest("hex").slice(0, 16);
}
