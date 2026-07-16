/**
 * Reading the client's own environment, once, into a plain value.
 *
 * Every other module in the config-handshake redesign (facts, resolve,
 * handshake) takes a `Bootstrap` rather than poking at `process.env` itself —
 * that is what keeps them unit-testable with an injected env and keeps the
 * "what env var reads what" knowledge in exactly one place.
 *
 * Deliberately narrower than settings.ts's BEHAVIOR_KNOBS: this is only the
 * handful of vars that gate *transport* (where to connect, whether to send a
 * bearer token, whether to require HTTPS) plus per-session identity
 * (MEMINI_AGENT, MEMINI_NAMESPACE, MEMINI_HOME). No aliases: MEMINI_URL and
 * MEMINI_TOKEN existed only for back-compat with the pre-handshake plugin
 * hooks (plugin/scripts/_shared.mjs) — this package is new, additive code
 * with no prior callers to keep compatible, so there is nothing to alias.
 */

export interface Bootstrap {
  /** MEMINI_BASE_URL only — no MEMINI_URL alias. Defaults to http://localhost:8080. */
  baseUrl: string;
  /** MEMINI_API_KEY only — no MEMINI_TOKEN alias. "" when unset. */
  apiKey: string;
  /** MEMINI_REQUIRE_HTTPS, parsed with the same accept-1/true/yes/on semantics as envEnabled. */
  requireHttps: boolean;
  /** MEMINI_DEBUG, same boolean semantics. */
  debug: boolean;
  /** MEMINI_AGENT, raw — "" when unset. Sanitization happens where a namespace is built, not here. */
  agent: string;
  /** MEMINI_NAMESPACE, trimmed — "" when unset. */
  namespaceEnv: string;
  /**
   * MEMINI_NAMESPACE_PREFIX, trimmed — "" when unset. A client-side override
   * of the namespace_prefix setting: prepended to a *derived* namespace
   * (personal/<repo>), letting one credential serve several namespace trees selected
   * per shell/directory. Sent to the server as a fact for the online path;
   * used here for the degraded (server-down) fallback in resolve.ts.
   */
  namespacePrefixEnv: string;
  /** MEMINI_HOME, trimmed — "" when unset. */
  homeEnv: string;
  /**
   * MEMINI_TIMEOUT_MS — how long one memini HTTP call may take before the
   * client gives up. Defaults to DEFAULT_TIMEOUT_MS.
   *
   * A request timeout is transport, so it belongs here rather than only in
   * settings.ts — and it has to work *before* a handshake, since the handshake
   * is what fetches settings. The same knob is also a BEHAVIOR_KNOB
   * (request_timeout_ms), so a server can push it to every client; this env var
   * is the local override of that. Both name the same default, so there is
   * exactly one number in the system.
   */
  timeoutMs: number;
}

/**
 * The client's default per-request timeout, and the value MEMINI_TIMEOUT_MS
 * already means in the pi and opencode integrations — this package adopts their
 * number rather than minting a second one, so the knob means the same thing
 * whichever client resolves it (the rule BEHAVIOR_KNOBS states in settings.ts).
 *
 * It must stay ABOVE the server's own MEMINI_RERANK_TIMEOUT (default 10s).
 * Layered timeouts only degrade gracefully when the outermost one is the
 * longest: the server bounds a slow reranker and falls back to composite order
 * so recall never fails on the reranker's account, but a client that hangs up
 * first never receives that fallback — it gets nothing at all. This was a real
 * bug: the Claude/Codex hooks (and openclaw, openwebui, hermes) hardcoded 5s
 * while the server would happily spend 10s reranking, so turning on a
 * cross-encoder returned zero memories instead of unranked ones.
 *
 * It is a ceiling, not a target — the server degrades to composite order at its
 * own deadline, so a healthy client never waits anywhere near this.
 */
export const DEFAULT_TIMEOUT_MS = 30000;

/**
 * The floor for a configured timeout, matching the ClientSettings schema's
 * `minimum` for request_timeout_ms. 0 is deliberately not overloaded as "no
 * timeout": a client that never gives up hangs forever on a wedged server
 * instead of failing soft, which is the one outcome every hook is written to
 * avoid.
 */
export const MIN_TIMEOUT_MS = 100;

const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

/**
 * A millisecond duration env var. Unset/empty/unparseable falls back to
 * `fallback`; a value below MIN_TIMEOUT_MS is raised to it rather than silently
 * becoming the (much larger) default, so a deliberately tight timeout stays
 * tight instead of surprising the caller with 15s.
 */
export function envTimeoutMs(raw: string | undefined, fallback: number = DEFAULT_TIMEOUT_MS): number {
  if (raw == null || raw.trim() === "") return fallback;
  const n = Number(raw.trim());
  if (!Number.isFinite(n)) return fallback;
  return Math.max(MIN_TIMEOUT_MS, Math.trunc(n));
}

/**
 * A boolean env var with an explicit default. Unset/empty falls back to
 * `defaultOn`; "0", "false", "no", "off" (case-insensitive, surrounding
 * whitespace ignored) are false; anything else is true. Mirrors
 * plugin/scripts/_shared.mjs's envEnabled exactly (minus the env-lookup step,
 * which callers here have already done).
 */
export function envEnabled(raw: string | undefined, defaultOn: boolean): boolean {
  if (raw == null || raw === "") return defaultOn;
  return !/^(0|false|no|off)$/i.test(raw.trim());
}

/** Read every env var this package's transport layer cares about, once. */
export function readBootstrap(env: Record<string, string | undefined> = process.env): Bootstrap {
  return {
    baseUrl: env["MEMINI_BASE_URL"] || "http://localhost:8080",
    apiKey: env["MEMINI_API_KEY"] || "",
    requireHttps: envEnabled(env["MEMINI_REQUIRE_HTTPS"], false),
    debug: envEnabled(env["MEMINI_DEBUG"], false),
    agent: env["MEMINI_AGENT"] || "",
    namespaceEnv: (env["MEMINI_NAMESPACE"] || "").trim(),
    namespacePrefixEnv: (env["MEMINI_NAMESPACE_PREFIX"] || "").trim(),
    homeEnv: (env["MEMINI_HOME"] || "").trim(),
    timeoutMs: envTimeoutMs(env["MEMINI_TIMEOUT_MS"]),
  };
}

/**
 * True when sending `secret` as a bearer token to `baseUrl` would cross the
 * network in plaintext: http:// to any host other than localhost/127.0.0.1/::1.
 * A blank secret is never unsafe — there is nothing to observe. Mirrors
 * plugin/scripts/_shared.mjs's usesPlaintextBearerAuth (and settings.ts's
 * former private copy, now refactored to call this).
 */
export function isPlaintextBearerUnsafe(baseUrl: string, secret: string): boolean {
  if (!secret) return false;
  try {
    const u = new URL(baseUrl);
    return u.protocol === "http:" && !LOOPBACK_HOSTS.has(u.hostname.replace(/^\[|\]$/g, "").toLowerCase());
  } catch {
    return false;
  }
}

/**
 * The plaintext-bearer guard. Ported from the guard plugin/scripts/_shared.mjs
 * (createPlaintextBearerAuthGuard, lines ~348-387) — every integration in this
 * repo (openclaw, pi, opencode, hermes, the Claude/Codex hooks) reimplements
 * the same rule: a bearer token bound for plaintext HTTP to a non-loopback
 * host is fine by default (today's behavior is silent/warn-only), but
 * MEMINI_REQUIRE_HTTPS turns it into a hard refusal.
 *
 * Unlike those older copies (which compare the raw env string to the literal
 * "1"), this one reuses `envEnabled`'s broader accept-1/true/yes/on parsing —
 * the same parsing `Bootstrap.requireHttps` uses — so a caller who already
 * has a `Bootstrap` and one who only has a raw `env` agree on what
 * "required" means. The default (unset) case is unchanged: no throw.
 *
 * A guard throw is a real throw — callers that need fail-soft behavior (like
 * performHandshake) call this OUTSIDE their try/catch on purpose.
 */
export function assertBearerTransportSafe(
  baseUrl: string,
  secret: string,
  env: Record<string, string | undefined> = process.env,
): void {
  if (!isPlaintextBearerUnsafe(baseUrl, secret)) return;
  if (!envEnabled(env["MEMINI_REQUIRE_HTTPS"], false)) return;
  throw new Error(
    `memini: a bearer token is configured for plaintext HTTP to ${baseUrl}. ` +
      "The token and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.",
  );
}
