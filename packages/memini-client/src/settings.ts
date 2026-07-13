/**
 * "What is this plugin actually doing right now?"
 *
 * The value of a settings dump is not the values — it is the *provenance*. A
 * flat list of effective settings would not have caught the case that motivated
 * this module: MEMINI_NAMESPACE exported as a fish **universal** variable, set
 * once months ago, silently collapsing every repo on the machine into a single
 * shared namespace. Only a "namespace: default  <- env:MEMINI_NAMESPACE (git
 * would give: memini)" line makes that visible.
 *
 * So every knob carries where it came from, and the namespace additionally
 * carries what it *would* have been without each layer.
 *
 * This module reports; it does not resolve. Each harness has its own resolution
 * chain (the Claude hooks consult a self-healing project map that the shared
 * resolver does not, for instance), so the caller injects its own resolver and
 * we describe whatever it produces. Trying to unify the five chains here would
 * mean reporting something other than what the harness actually does — which is
 * the one thing a diagnostic must never do.
 */

import { isSensitive, redactValue } from "./redact.js";
import { readOverride, defaultOverridesPath, type NamespaceOverride } from "./override.js";

export type NamespaceSource =
  | "override"
  | "env"
  | "config"
  | "project-map"
  | "git-remote"
  | "git-toplevel"
  | "cwd"
  | "default";

export interface ResolvedNamespace {
  namespace: string;
  source: NamespaceSource;
}

export interface ResolveOpts {
  /**
   * Skip the project override and resolve as if none were set.
   *
   * Needed because the counterfactual lines ("what would this be without the
   * override?") cannot be produced by doctoring the environment: the override
   * lives in a file, not an env var, so a resolver handed a stripped env would
   * still return it. Without this flag `withoutOverride` reports the override
   * back to you — which is precisely the line that is supposed to reveal what
   * the override is masking.
   */
  ignoreOverride?: boolean;
}

/** Resolve a namespace the way *this harness* does, for a given environment. */
export type HarnessResolver = (
  env: Record<string, string | undefined>,
  opts?: ResolveOpts,
) => ResolvedNamespace;

export type KnobKind = "string" | "int" | "float" | "bool" | "list";

export interface KnobSpec {
  name: string;
  kind: KnobKind;
  /** Rendered default when the variable is unset. */
  default: string;
  /** Which side of the plugin reads it — the hooks (REST), the MCP tools, or both. */
  usedBy: "hooks" | "MCP" | "hooks + MCP";
  description: string;
}

/**
 * Every environment variable the *client* reads. Deliberately explicit rather
 * than scraped from process.env: the point is to show a knob and its default
 * even when it is unset, since "MEMINI_HOME is unset" is itself the finding.
 *
 * Mirrors the tables in plugin/README.md. Server-only variables
 * (MEMINI_SQLITE_PATH, MEMINI_EMBED_*, …) are intentionally absent — they are
 * read by the `memini` process, not by the plugin, and listing them here would
 * imply the plugin honors them. docs/reference/env-vars.md exists to keep that
 * distinction straight; four names mean different things on each side.
 */
export const CLIENT_KNOBS: KnobSpec[] = [
  // Connection
  { name: "MEMINI_BASE_URL", kind: "string", default: "http://localhost:8080", usedBy: "hooks + MCP", description: "memini base URL (alias: MEMINI_URL)" },
  { name: "MEMINI_MCP_URL", kind: "string", default: "${MEMINI_BASE_URL}/mcp", usedBy: "MCP", description: "MCP endpoint; derived from the base URL unless set" },
  { name: "MEMINI_API_KEY", kind: "string", default: "", usedBy: "hooks + MCP", description: "bearer token sent to the server (alias: MEMINI_TOKEN)" },
  { name: "MEMINI_REQUIRE_HTTPS", kind: "bool", default: "0", usedBy: "hooks + MCP", description: "refuse to send a bearer token over plaintext HTTP" },

  // Namespace
  { name: "MEMINI_NAMESPACE", kind: "string", default: "(auto: git/cwd)", usedBy: "hooks + MCP", description: "pin the namespace; overrides git and directory detection" },
  { name: "MEMINI_NAMESPACE_SCOPE", kind: "string", default: "repo", usedBy: "hooks", description: "owner-repo derives owner-repo slugs from the git remote" },
  { name: "MEMINI_AGENT", kind: "string", default: "", usedBy: "hooks + MCP", description: "nest the namespace under a per-agent segment" },
  { name: "MEMINI_HOME", kind: "string", default: "", usedBy: "hooks + MCP", description: 'personal namespace; required for visibility:"personal" writes' },

  // Capture
  { name: "MEMINI_CAPTURE_TURNS", kind: "bool", default: "on", usedBy: "hooks", description: "capture each user→assistant turn as episodic memory" },
  { name: "MEMINI_SESSION_DIGEST", kind: "bool", default: "on", usedBy: "hooks", description: "record session digests (files edited, commands run); 0 to keep memory to durable facts only" },
  { name: "MEMINI_INLINE_EXTRACT", kind: "bool", default: "on", usedBy: "hooks", description: "inject the memory-save directive at SessionStart" },
  { name: "MEMINI_AUTO_SAVE", kind: "bool", default: "on", usedBy: "hooks", description: "periodic auto-save nudge on Stop" },
  { name: "MEMINI_AUTO_SAVE_INTERVAL", kind: "int", default: "10", usedBy: "hooks", description: "user messages between auto-save nudges" },

  // Injection budgets
  { name: "MEMINI_INJECT_BRIEFING_PINNED", kind: "int", default: "5", usedBy: "hooks", description: "max pinned memories at SessionStart (0 disables)" },
  { name: "MEMINI_INJECT_BRIEFING_FACTS", kind: "int", default: "5", usedBy: "hooks", description: "max durable facts at SessionStart (0 disables)" },
  { name: "MEMINI_INJECT_BRIEFING_PROCEDURES", kind: "int", default: "5", usedBy: "hooks", description: "max procedural how-tos at SessionStart (0 disables)" },
  { name: "MEMINI_INJECT_BRIEFING_RECENT", kind: "int", default: "3", usedBy: "hooks", description: "max recent episodic entries at SessionStart (0 disables)" },
  { name: "MEMINI_INJECT_BRIEFING_MAX_TOK", kind: "int", default: "uncapped", usedBy: "hooks", description: "token ceiling on the SessionStart briefing" },
  { name: "MEMINI_INJECT_PRETOOL_ITEMS", kind: "int", default: "3", usedBy: "hooks", description: "max hits surfaced per file on PreToolUse" },
  { name: "MEMINI_INJECT_PRETOOL_MAX_TOK", kind: "int", default: "uncapped", usedBy: "hooks", description: "token ceiling per file on PreToolUse" },
  { name: "MEMINI_INJECT_PRETOOL_MIN_SCORE", kind: "float", default: "0", usedBy: "hooks", description: "relevance floor for PreToolUse hits" },
  { name: "MEMINI_INJECT_PRETOOL_TOOLS", kind: "list", default: "Read|Write|Edit|Glob|Grep", usedBy: "hooks", description: "tool allowlist for PreToolUse recall" },
  { name: "MEMINI_INJECT_LABELS", kind: "list", default: "", usedBy: "hooks", description: "annotate injected bullets: tier, confidence, age, reason" },

  // Diagnostics
  { name: "MEMINI_DEBUG", kind: "bool", default: "0", usedBy: "hooks + MCP", description: "verbose hook logging to stderr" },
];

export interface SettingValue {
  name: string;
  /** Effective value, already redacted when sensitive. */
  value: string;
  source: "env" | "default";
  isDefault: boolean;
  sensitive: boolean;
  usedBy: KnobSpec["usedBy"];
  description: string;
}

export interface NamespaceReport {
  /** What the harness will actually use. */
  effective: string;
  source: NamespaceSource;
  /** Set when a per-project override is in force. */
  override?: NamespaceOverride;
  /** What it would be with the override removed — reveals what the override is masking. */
  withoutOverride: ResolvedNamespace;
  /** What it would be with the override AND MEMINI_NAMESPACE removed, i.e. pure git/cwd
   *  derivation. This is the line that exposes a global env pin. */
  derived: ResolvedNamespace;
  /** MEMINI_HOME. Undefined means no personal leg and personal writes will error. */
  home?: string;
}

export type WarningLevel = "warn" | "note";

export interface Warning {
  level: WarningLevel;
  code: string;
  message: string;
  /** What to do about it. */
  fix?: string;
}

export interface ClientSettings {
  cwd: string;
  namespace: NamespaceReport;
  settings: SettingValue[];
  paths: {
    overrides: string;
    cache?: string;
  };
  warnings: Warning[];
}

export interface DescribeOptions {
  cwd: string;
  env?: Record<string, string | undefined>;
  /** How this harness resolves a namespace. Called with doctored environments. */
  resolve: HarnessResolver;
  overridesPath?: string;
  cacheDir?: string;
}

/** Read a knob's effective value + provenance. */
function describeKnob(spec: KnobSpec, env: Record<string, string | undefined>): SettingValue {
  const raw = env[spec.name];
  const set = raw != null && raw !== "";
  const sensitive = isSensitive(spec.name);
  let value: string;
  if (set) {
    value = sensitive ? redactValue(raw!) : raw!;
  } else {
    value = spec.default === "" ? "(unset)" : spec.default;
  }
  return {
    name: spec.name,
    value,
    source: set ? "env" : "default",
    isDefault: !set,
    sensitive,
    usedBy: spec.usedBy,
    description: spec.description,
  };
}

const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1"]);

function isPlaintextToNonLoopback(baseUrl: string): boolean {
  try {
    const u = new URL(baseUrl);
    return u.protocol === "http:" && !LOOPBACK.has(u.hostname.replace(/^\[|\]$/g, "").toLowerCase());
  } catch {
    return false;
  }
}

/**
 * Build the full effective-settings report.
 *
 * The three namespace lines (effective / withoutOverride / derived) are produced
 * by calling the harness's own resolver against progressively stripped
 * environments. That is what turns "your namespace is default" into "your
 * namespace is default because MEMINI_NAMESPACE is exported, and git would
 * otherwise have given you memini" — which is the difference between a dump and
 * a diagnosis.
 */
export function describeSettings(opts: DescribeOptions): ClientSettings {
  const env = opts.env || (process.env as Record<string, string | undefined>);
  const cwd = opts.cwd;

  const override = readOverride(cwd, { env, overridesPath: opts.overridesPath });

  // Both counterfactuals must ignore the override explicitly. Stripping the
  // environment is not enough — the override is a file, so a resolver handed a
  // doctored env would hand the override straight back, and these two lines
  // would report the very thing they exist to see past.
  const withoutOverride = opts.resolve(env, { ignoreOverride: true });

  const envSansPin = { ...env };
  delete envSansPin["MEMINI_NAMESPACE"];
  const derived = opts.resolve(envSansPin, { ignoreOverride: true });

  const effective = override ? override.namespace : withoutOverride.namespace;
  const source: NamespaceSource = override ? "override" : withoutOverride.source;

  const home = (env["MEMINI_HOME"] || "").trim() || undefined;

  const settings = CLIENT_KNOBS.map((k) => describeKnob(k, env));
  const warnings: Warning[] = [];

  if (override) {
    warnings.push({
      level: "note",
      code: "override-active",
      message:
        `namespace is overridden to "${override.namespace}" for this project ` +
        `(set ${override.setAt}); without it this project would use "${withoutOverride.namespace}".`,
      fix: "Run the namespace command with --clear to return to automatic resolution.",
    });
  }

  // The finding this whole module exists for.
  const pin = (env["MEMINI_NAMESPACE"] || "").trim();
  if (pin && !override && derived.namespace && derived.namespace !== pin) {
    warnings.push({
      level: "warn",
      code: "global-namespace-pin",
      message:
        `MEMINI_NAMESPACE is set to "${pin}", which pins EVERY project on this machine to ` +
        `one namespace. This project would otherwise resolve to "${derived.namespace}". ` +
        `If this variable is exported from a shell rc (or a fish universal variable), every ` +
        `repo you work in is sharing one memory pool.`,
      fix: `Unset MEMINI_NAMESPACE and let each repo resolve on its own, or set a per-project override instead.`,
    });
  }

  if (!home) {
    warnings.push({
      level: "warn",
      code: "home-unset",
      message:
        'MEMINI_HOME is unset: there is no personal namespace, so visibility:"personal" ' +
        "writes will error and no personal leg merges into recall.",
      fix: "Export MEMINI_HOME=personal/<you>.",
    });
  }

  const baseUrl = env["MEMINI_BASE_URL"] || env["MEMINI_URL"] || "http://localhost:8080";
  const token = env["MEMINI_API_KEY"] || env["MEMINI_TOKEN"] || "";
  if (token && isPlaintextToNonLoopback(baseUrl)) {
    warnings.push({
      level: "warn",
      code: "plaintext-bearer",
      message:
        `a bearer token is configured for plaintext HTTP to ${baseUrl}; the token and your ` +
        "memory payloads can be observed on the network.",
      fix: "Use HTTPS, or tunnel over SSH. Set MEMINI_REQUIRE_HTTPS=1 to make this an error.",
    });
  }

  return {
    cwd,
    namespace: { effective, source, override, withoutOverride, derived, home },
    settings,
    paths: {
      overrides: opts.overridesPath || defaultOverridesPath(env),
      cache: opts.cacheDir,
    },
    warnings,
  };
}
