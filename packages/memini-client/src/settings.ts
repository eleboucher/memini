/**
 * Behavioral settings resolution for the config-handshake redesign.
 *
 * BEHAVIOR_KNOBS/effectiveSetting cover the 29 behavioral fields of the
 * spec's ClientSettings (api/openapi.yaml) — everything a HandshakeResponse
 * can hand back as a server-merged default, and everything a local env var
 * can still override. namespace_scope/namespace_prefix are deliberately
 * excluded: they shape namespace *derivation* (resolve.ts), not runtime
 * behavior, so they are not "effective settings" in this sense.
 *
 * This module used to also carry CLIENT_KNOBS/describeSettings — a
 * connection/namespace/diagnostics report built around each harness's own
 * override-based resolver. That surface was retired once every TypeScript
 * consumer (pi, openclaw; the Claude Code + Codex hooks never used it) moved
 * onto the handshake's own provenance (bootstrap + facts + handshake +
 * effectiveSetting) — see integrations/pi and integrations/openclaw's
 * renderStatus/buildWarnings for what replaced it. redact.ts's redaction and
 * bootstrap.ts's isPlaintextBearerUnsafe still cover the same diagnostic
 * needs; each caller composes its own report now instead of this package
 * imposing one shape on every harness.
 */

import { DEFAULT_TIMEOUT_MS, MIN_TIMEOUT_MS } from "./bootstrap.js";

export type SettingSource = "env-override" | "server" | "default";

export interface BehaviorKnob {
  /** The MEMINI_* env var that overrides this field locally. */
  envName: string;
  /** The field name as it appears on the wire (HandshakeResponse.settings / ClientSettings). */
  wireKey: string;
  kind: "bool" | "int" | "float" | "list";
  /** The built-in default — used when neither an env override nor a server value is present. */
  default: unknown;
  /**
   * Optional floor for an "int" knob, mirroring the ClientSettings schema's
   * `minimum`. A configured value below it is raised to it rather than falling
   * back to the (possibly much larger) default, so a deliberately tight value
   * stays tight. Only request_timeout_ms has a non-zero floor today.
   */
  min?: number;
}

/**
 * Every behavioral field of the spec's ClientSettings, with the env var that
 * overrides it locally. envName/default here match the equivalent knobs
 * already shipped by the other integrations in this repo (opencode, pi,
 * hermes, openclaw) — see each integration's README under integrations/ —
 * so a setting means the same thing regardless of which client resolves it.
 */
export const BEHAVIOR_KNOBS: BehaviorKnob[] = [
  { envName: "MEMINI_CAPTURE_TURNS", wireKey: "capture_turns", kind: "bool", default: true },
  { envName: "MEMINI_SESSION_DIGEST", wireKey: "session_digest", kind: "bool", default: true },
  { envName: "MEMINI_INLINE_EXTRACT", wireKey: "inline_extract", kind: "bool", default: true },
  { envName: "MEMINI_AUTO_SAVE", wireKey: "auto_save", kind: "bool", default: true },
  { envName: "MEMINI_AUTO_SAVE_INTERVAL", wireKey: "auto_save_interval", kind: "int", default: 10 },
  { envName: "MEMINI_AUTO_SAVE_MIN_EVENTS", wireKey: "auto_save_min_events", kind: "int", default: 3 },
  { envName: "MEMINI_INJECT_BRIEFING_PINNED", wireKey: "inject_briefing_pinned", kind: "int", default: 5 },
  { envName: "MEMINI_INJECT_BRIEFING_FACTS", wireKey: "inject_briefing_facts", kind: "int", default: 5 },
  { envName: "MEMINI_INJECT_BRIEFING_PROCEDURES", wireKey: "inject_briefing_procedures", kind: "int", default: 5 },
  { envName: "MEMINI_INJECT_BRIEFING_RECENT", wireKey: "inject_briefing_recent", kind: "int", default: 3 },
  { envName: "MEMINI_INJECT_BRIEFING_MAX_TOK", wireKey: "inject_briefing_max_tok", kind: "int", default: 0 },
  { envName: "MEMINI_INJECT_PRETOOL_ITEMS", wireKey: "inject_pretool_items", kind: "int", default: 3 },
  { envName: "MEMINI_INJECT_PRETOOL_MAX_TOK", wireKey: "inject_pretool_max_tok", kind: "int", default: 0 },
  { envName: "MEMINI_INJECT_PRETOOL_MIN_SCORE", wireKey: "inject_pretool_min_score", kind: "float", default: 0 },
  {
    envName: "MEMINI_INJECT_PRETOOL_TOOLS",
    wireKey: "inject_pretool_tools",
    kind: "list",
    default: ["Read", "Write", "Edit", "MultiEdit", "Glob", "Grep"],
  },
  { envName: "MEMINI_INJECT_PRETOOL_GATE_MS", wireKey: "inject_pretool_gate_ms", kind: "int", default: 90000 },
  { envName: "MEMINI_INJECT_DEDUPE", wireKey: "inject_dedupe", kind: "bool", default: true },
  { envName: "MEMINI_INJECT_COOLDOWN_MS", wireKey: "inject_cooldown_ms", kind: "int", default: 1800000 },
  { envName: "MEMINI_INJECT_COOLDOWN_PROMPTS", wireKey: "inject_cooldown_prompts", kind: "int", default: 3 },
  { envName: "MEMINI_INJECT_LABELS", wireKey: "inject_labels", kind: "list", default: [] },
  { envName: "MEMINI_RECALL", wireKey: "recall", kind: "bool", default: true },
  { envName: "MEMINI_CAPTURE", wireKey: "capture", kind: "bool", default: true },
  { envName: "MEMINI_RECALL_LIMIT", wireKey: "recall_limit", kind: "int", default: 3 },
  { envName: "MEMINI_INJECT_RECALL_MAX_TOK", wireKey: "inject_recall_max_tok", kind: "int", default: 0 },
  { envName: "MEMINI_INJECT_RECALL_MIN_SCORE", wireKey: "inject_recall_min_score", kind: "float", default: 0 },
  { envName: "MEMINI_MIN_CAPTURE_CHARS", wireKey: "min_capture_chars", kind: "int", default: 0 },
  { envName: "MEMINI_CAPTURE_USER_MAX_CHARS", wireKey: "capture_user_max_chars", kind: "int", default: 1000 },
  {
    envName: "MEMINI_CAPTURE_ASSISTANT_MAX_CHARS",
    wireKey: "capture_assistant_max_chars",
    kind: "int",
    default: 3000,
  },
  // Also read at the transport layer (bootstrap.ts's timeoutMs), because a
  // request timeout has to exist before the handshake that fetches settings.
  // Both spellings share DEFAULT_TIMEOUT_MS/MIN_TIMEOUT_MS, so there is exactly
  // one number: raising it here (server-side) or in the env both work.
  {
    envName: "MEMINI_TIMEOUT_MS",
    wireKey: "request_timeout_ms",
    kind: "int",
    default: DEFAULT_TIMEOUT_MS,
    min: MIN_TIMEOUT_MS,
  },
];

/**
 * Parses like plugin/scripts/_shared.mjs's intEnv: >=0 integer, else
 * `fallback`. A knob with a `min` raises an in-range-but-too-small value to
 * that floor instead of discarding it.
 */
function parseIntKnob(raw: string, fallback: number, min = 0): number {
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n < 0) return fallback;
  return Math.max(min, n);
}

/** Parses like plugin/scripts/_shared.mjs's floatEnv: >=0 float, else `fallback`. */
function parseFloatKnob(raw: string, fallback: number): number {
  const n = Number.parseFloat(raw);
  return Number.isFinite(n) && n >= 0 ? n : fallback;
}

/** Parses like plugin/scripts/_shared.mjs's listEnv: pipe/comma separated, trimmed, lowercased. */
function parseListKnob(raw: string): string[] {
  return raw
    .split(/[|,]/)
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);
}

/**
 * A single knob's effective value and where it came from: a local env
 * override beats a server-provided value beats the built-in default. Reuses
 * the exact parsing semantics of _shared.mjs's intEnv/envEnabled/listEnv (see
 * bootstrap.ts's envEnabled for the boolean case) so a value means the same
 * thing whether it came from an env var here or from one of the older
 * per-integration copies.
 */
export function effectiveSetting<T>(
  knob: BehaviorKnob,
  server: Record<string, unknown> | undefined,
  env: Record<string, string | undefined> = process.env,
): { value: T; source: SettingSource } {
  const raw = env[knob.envName];
  if (raw != null && raw !== "") {
    let value: unknown;
    switch (knob.kind) {
      case "bool":
        value = !/^(0|false|no|off)$/i.test(raw.trim());
        break;
      case "int":
        value = parseIntKnob(raw, knob.default as number, knob.min);
        break;
      case "float":
        value = parseFloatKnob(raw, knob.default as number);
        break;
      case "list":
        value = parseListKnob(raw);
        break;
    }
    return { value: value as T, source: "env-override" };
  }

  if (server && Object.prototype.hasOwnProperty.call(server, knob.wireKey)) {
    return { value: server[knob.wireKey] as T, source: "server" };
  }

  return { value: knob.default as T, source: "default" };
}
