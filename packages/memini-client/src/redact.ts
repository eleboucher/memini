/**
 * Secret redaction for anything the client prints about itself.
 *
 * Mirrors the server's policy in internal/redact/redact.go: redaction is always
 * on, never opt-in. (MEMINI_REDACT_SECRETS is a *deprecated* server var for
 * exactly that reason.) A settings dump is the one place a token is most likely
 * to be pasted into an issue, so the safe default matters more here than
 * anywhere else.
 */

/**
 * Names whose values must never be printed in full. Matches the whole name, not
 * a suffix, so MEMINI_API_KEYS_FILE (a path, not a secret) is deliberately NOT
 * caught — but MEMINI_API_KEY, MEMINI_TOKEN, MEMINI_MCP_BEARER,
 * MEMINI_LLM_API_KEY and MEMINI_POSTGRES_DSN (password in the userinfo) are.
 */
const SENSITIVE = /(^|_)(KEY|TOKEN|SECRET|PASSWORD|PASS|BEARER|DSN|CREDENTIALS?)$/i;

export function isSensitive(name: string): boolean {
  return SENSITIVE.test(name);
}

/**
 * Render a secret as a recognizable-but-useless fingerprint: enough to tell two
 * tokens apart and to confirm "yes, the one I set is the one in use", not enough
 * to use. Short values are elided entirely rather than half-revealed — a 6-char
 * secret showing 3 leading and 4 trailing chars would be no secret at all.
 */
export function redactValue(value: string): string {
  if (!value) return "";
  if (value.length <= 12) return "***";
  return `${value.slice(0, 3)}…${value.slice(-4)}`;
}

/** Redact when the name warrants it; otherwise pass the value through. */
export function redactByName(name: string, value: string): string {
  return isSensitive(name) ? redactValue(value) : value;
}
