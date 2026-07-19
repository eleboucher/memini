/**
 * Injection telemetry: the pure wire-body builder for POST
 * /v1/activity/injected. The transport (postInjected) stays in each
 * integration's adapter — only the payload shape is shared.
 *
 * Ported VERBATIM from plugin/scripts/_shared.mjs. Pure: no fs, no env, no
 * clock, no network.
 */

/** The suppression counters the wire contract knows. */
const SUPPRESSED_KEYS = ["seen", "cooldown", "budget", "unchanged", "score"] as const;

/** The wire body for POST /v1/activity/injected. */
export interface InjectedReportBody {
  session_id: string;
  surface: string | undefined;
  source: string;
  injected_ids: string[];
  injected_tokens_est?: number;
  injected_chars?: number;
  suppressed?: Partial<Record<(typeof SUPPRESSED_KEYS)[number], number>>;
}

/**
 * Build the wire body for POST /v1/activity/injected. Pure. The endpoint's
 * required fields are always present — session_id, surface, source (always
 * "claude-code"), injected_ids ([] is allowed when only suppressions are
 * reported) — while zero/empty optionals are omitted: injected_tokens_est,
 * injected_chars, each zero count inside `suppressed`, and the whole
 * `suppressed` object when every count is zero.
 */
export function injectedReport(
  {
    surface,
    sessionId,
    ids,
    tokens,
    chars,
    suppressed,
  }: {
    surface?: string;
    sessionId?: string;
    ids?: unknown;
    tokens?: number;
    chars?: number;
    suppressed?: Record<string, number | undefined>;
  } = {},
): InjectedReportBody {
  const body: InjectedReportBody = {
    session_id: sessionId || "",
    surface,
    source: "claude-code",
    injected_ids: Array.isArray(ids) ? ids.filter((id) => typeof id === "string" && id) : [],
  };
  if (Number.isFinite(tokens) && (tokens as number) > 0) body.injected_tokens_est = tokens;
  if (Number.isFinite(chars) && (chars as number) > 0) body.injected_chars = chars;
  const sup: Record<string, number> = {};
  for (const k of SUPPRESSED_KEYS) {
    const v = suppressed?.[k];
    if (Number.isFinite(v) && (v as number) > 0) sup[k] = v as number;
  }
  if (Object.keys(sup).length > 0) body.suppressed = sup;
  return body;
}
