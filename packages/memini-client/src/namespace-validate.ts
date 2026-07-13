/**
 * Client-side namespace validation.
 *
 * The server accepts anything 1–256 bytes without a NUL (internal/httputil
 * ValidateNamespace). The client must be *stricter*, because a namespace does
 * not reach the server as a body field — it rides on the `X-Memini-Namespace`
 * HTTP header. A value containing CR or LF would split the header and let a
 * caller inject arbitrary headers, so those are rejected here rather than
 * normalized away.
 */

export const MAX_NAMESPACE_BYTES = 256;

/**
 * Canonical form, matching the server's NormalizeNamespace: trim whitespace,
 * strip leading/trailing slashes, collapse duplicate separators. " a//b/ "
 * addresses the same rows as "a/b".
 */
export function normalizeNamespace(ns: string): string {
  let out = String(ns ?? "").trim();
  out = out.replace(/^\/+|\/+$/g, "");
  while (out.includes("//")) out = out.replace(/\/\/+/g, "/");
  return out;
}

/**
 * Returns null when valid, or a human-readable reason when not. Callers should
 * normalize first — validation is deliberately not forgiving, so that what the
 * user typed is what gets stored and sent.
 */
export function validateNamespace(ns: string): string | null {
  if (!ns) return "namespace is empty";
  if (Buffer.byteLength(ns, "utf8") > MAX_NAMESPACE_BYTES) {
    return `namespace exceeds ${MAX_NAMESPACE_BYTES} bytes`;
  }
  // Header safety: CR/LF would split the X-Memini-Namespace header outright.
  if (/[\r\n]/.test(ns)) return "namespace contains a newline";
  // Any other control character (incl. NUL) is unrepresentable in a header.
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x1F\x7F]/.test(ns)) return "namespace contains a control character";
  // Header values are ASCII; anything else would need encoding and would not
  // round-trip identically through the server's string comparison.
  // eslint-disable-next-line no-control-regex
  if (/[^\x20-\x7E]/.test(ns)) return "namespace contains a non-ASCII character";
  return null;
}
