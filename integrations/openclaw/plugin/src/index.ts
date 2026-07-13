/**
 * memini memory-slot plugin for OpenClaw.
 *
 * Claims plugins.slots.memory via api.registerMemoryCapability, plus:
 *   - before_prompt_build: recall relevant memories, prepend as context
 *   - agent_end: capture the completed turn into memini
 *
 * Rather than wiring the slot's `runtime`/`flushPlanResolver` (the host-driven
 * pattern the file-backed memory-core plugin uses), memini drives recall and
 * capture itself over REST (/v1/search, /v1/memories), scoped by the
 * X-Memini-Namespace header — it's an external service, not a local corpus.
 * Default endpoint http://localhost:8080.
 *
 * Namespace and behavioral settings come from the config-handshake redesign
 * (POST /v1/handshake, api/openapi.yaml): openclaw imports @memini/client
 * directly and composes gatherFacts/performHandshake/effectiveSetting the same
 * way the Claude Code plugin's hooks do, with two deliberate departures from
 * the shared resolveNamespace precedence (see resolveBaseNamespace/
 * effectiveConfig below):
 *
 *   - The facts sent are gateway facts only — {cwd_basename, declared_namespace}
 *     — no git derivation. OpenClaw's cwd is the daemon's process directory,
 *     not a project's; deriving from it (or reporting it as a project's
 *     remote/toplevel) would risk colliding with an unrelated pin someone set
 *     for whatever repo the daemon happens to be checked out inside.
 *   - MEMINI_NAMESPACE beats the handshake outright (not just when the
 *     handshake is unreachable, as resolveNamespace does for a per-repo
 *     client): this is a long-lived, per-machine gateway install, and an
 *     operator's local env pin should never be silently shadowed by whatever
 *     the server resolves.
 *
 * The handshake is memoized in-memory per plugin instance (OpenClaw creates
 * one per session) for HANDSHAKE_TTL_MS, mirroring pi's design — no file
 * cache, since a gateway install has no second process that would need one.
 *
 * NOTE: agent_end is a raw-conversation hook. Non-bundled plugins only receive
 * event.messages on it when the operator sets
 * `plugins.entries.memini.hooks.allowConversationAccess: true` in openclaw.json
 * — without it, capture silently no-ops. See README "Install".
 */

import { basename } from "node:path";
import { readFileSync } from "node:fs";
import { Type } from "typebox";
import { buildJsonPluginConfigSchema, definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import {
  readBootstrap,
  gatherFacts,
  performHandshake,
  effectiveSetting,
  BEHAVIOR_KNOBS,
  HANDSHAKE_TTL_MS,
  normalizeNamespace,
  validateNamespace,
  redactValue,
  assertBearerTransportSafe,
  isPlaintextBearerUnsafe,
  type Bootstrap,
  type ProjectFacts,
  type HandshakeResult,
} from "@memini/client";

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 5000;
const DEFAULT_NAMESPACE = "openclaw";
// performHandshake's own default (2500ms) is used when this isn't overridden;
// named here only so callers reading this file see the actual value in force.
const HANDSHAKE_TIMEOUT_MS = 2500;
const VALID_TIERS = ["working", "episodic", "semantic", "procedural"];
// The LLM-facing semantic scope vocabulary, identical to the MCP server's
// (internal/api/mcp: scopeEnum). The deprecated REST aliases "exact"/"subtree"
// are deliberately NOT offered: the model makes a semantic choice, it does not
// speak the back-compat dialect.
const VALID_SCOPES = ["project", "full", "everywhere"];
// The status probes are diagnostics: fail fast rather than hang a slash command.
const STATUS_TIMEOUT_MS = 4000;

// The client identifies itself to /v1/handshake for logging/diagnostics only
// (api/openapi.yaml's HandshakeRequest.client). Version is read from this
// plugin's own package.json so it never has to be kept in sync by hand.
const CLIENT_NAME = "openclaw-memini";
function readPluginVersion(): string {
  try {
    const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
    return typeof pkg.version === "string" && pkg.version ? pkg.version : "0.0.0";
  } catch {
    return "0.0.0";
  }
}
const CLIENT_VERSION = readPluginVersion();

// Type.Object returns a TObject<TProperties>, not a plain JsonSchemaObject;
// `as any` bridges the gap (same trick the SDK's defineToolPlugin examples use).
const typeboxConfigSchema = Type.Object(
  {
    enabled: Type.Optional(Type.Boolean()),
    base_url: Type.Optional(Type.String()),
    namespace: Type.Optional(Type.String()),
    namespace_per_agent: Type.Optional(Type.Boolean()),
    namespace_template: Type.Optional(Type.String()),
    skip_without_agent: Type.Optional(Type.Boolean()),
    skip_system_turns: Type.Optional(Type.Boolean()),
    system_kinds: Type.Optional(Type.Array(Type.String())),
    fallback_on_error: Type.Optional(Type.Boolean()),
    timeout_ms: Type.Optional(Type.Number()),
    expose_tools: Type.Optional(Type.Boolean()),
    recall_limit: Type.Optional(Type.Number()),
    recall_max_tokens: Type.Optional(Type.Number()),
    min_capture_chars: Type.Optional(Type.Number()),
    namespace_prefix: Type.Optional(Type.String()),
    home: Type.Optional(Type.String()),
  },
  { additionalProperties: false },
);

const configSchema = buildJsonPluginConfigSchema(typeboxConfigSchema as any);

// Per-agent isolation is the default: each named agent gets its own namespace
// so subagents sharing one OpenClaw install do not poison each other's memory.
// The default template prefixes the configured base ("openclaw" -> "openclaw-miso")
// so per-agent namespaces are distinct from the shared fallback used for
// sessions that carry no agent identity.
const DEFAULT_NAMESPACE_TEMPLATE = "{namespace}-{agent}";

// OpenClaw's ctx.trigger values that mark a system-initiated run rather than a
// user message (PluginHookAgentContext.trigger is "user" | "heartbeat" |
// "cron" | …). These resolve an agent identity like any other turn, so
// skip_without_agent doesn't catch them — skip_system_turns does. Matched
// case-insensitively; override the set via the system_kinds config.
const DEFAULT_SYSTEM_KINDS = ["heartbeat", "cron"];

// harnessCwd is the directory a pin is best-effort keyed to (via gatherFacts'
// git derivation) when an operator writes one through /memini:namespace.
// OpenClaw is a gateway: its cwd is the daemon's, not a project's, so this is
// best-effort. When the gateway happens to run inside a repo, that repo's
// identity keys the pin; when it does not, the pin command reports that there
// is nothing to key it by.
function harnessCwd(): string | undefined {
  try {
    return process.cwd();
  } catch {
    return undefined;
  }
}

// --- inlined template engine (formerly @memini/namespace-resolver) ----------
//
// applyTemplate substitutes {tenant}/{project}/{agent}/{namespace} placeholders
// and collapses slashes left dangling by a dropped segment. Ported verbatim
// from the now-deleted @memini/namespace-resolver package: openclaw is its
// last consumer (tenant/project only ever mattered to that package's own
// config.json tenant-root feature, which openclaw never populated — it always
// called this with just {agent, namespace}), so carrying the whole shared
// template engine forward as a separate published package would keep three
// dead-for-everyone-else placeholders alive along with it. Inlining (rather
// than moving it into @memini/client) keeps it out of a package every other
// integration imports for something only this one uses.
export function applyTemplate(
  template: string,
  segments: { tenant?: string; project?: string; agent?: string; namespace?: string },
): string {
  const all: Record<string, string | undefined> = {
    tenant: segments.tenant,
    project: segments.project,
    agent: segments.agent,
    namespace: segments.namespace,
  };
  let result = template.replace(/\{(tenant|project|agent|namespace)\}/g, (_, key: string) => all[key] ?? "");
  result = result.replace(/\/{2,}/g, "/");
  result = result.replace(/^\/+|\/+$/g, "");
  return result;
}

// --- namespace resolution -----------------------------------------------------

function knob(wireKey: string) {
  const k = BEHAVIOR_KNOBS.find((b) => b.wireKey === wireKey);
  if (!k) throw new Error(`openclaw-memini: unknown behavior knob "${wireKey}"`);
  return k;
}

export type NamespaceSource = "env" | "config" | "default" | `server:${string}`;

/**
 * resolveBaseNamespace resolves the plugin's BASE namespace synchronously,
 * with NO handshake involved — this is the config+env-only baseline
 * (namespace_source is always "env"/"config"/"default" here; effectiveConfig
 * below is what layers a live handshake on top):
 *
 *   1. MEMINI_NAMESPACE
 *   2. the explicit `namespace` config value
 *   3. the "openclaw" default
 *
 * There is deliberately still NO git/cwd derivation here, and the default
 * stays the literal "openclaw": a gateway's cwd is usually meaningless, and
 * deriving a namespace from it would silently relocate every existing
 * install's memory. namespace_prefix / namespace_template / per-agent nesting
 * all apply on top of whatever this returns, exactly as before. Exported for
 * testing.
 */
export function resolveBaseNamespace(
  pluginConfig: any,
  env: Record<string, string | undefined> = process.env,
): { namespace: string; source: NamespaceSource } {
  const c = pluginConfig || {};

  const nsEnv = (env["MEMINI_NAMESPACE"] || "").trim();
  // Raw-trimmed: the server validates the header, and an explicit hierarchical
  // value (team/eu) must pass through untouched.
  if (nsEnv) return { namespace: nsEnv, source: "env" };

  const configured = typeof c.namespace === "string" ? c.namespace.trim() : "";
  if (configured) return { namespace: configured, source: "config" };

  return { namespace: DEFAULT_NAMESPACE, source: "default" };
}

// strEnv returns a trimmed string env var, or "" when unset/blank.
function strEnv(env: Record<string, string | undefined>, name: string) {
  const raw = env[name];
  return raw == null ? "" : raw.trim();
}

// intEnv parses a non-negative integer env var, falling back to `def` when
// unset or malformed (env values are operator input and must never throw).
function intEnv(env: Record<string, string | undefined>, name: string, def: number) {
  const raw = env[name];
  if (raw == null || raw === "") return def;
  const n = Number.parseInt(raw, 10);
  return Number.isFinite(n) && n >= 0 ? n : def;
}

// resolveConfig normalizes raw plugin config into the defaults the plugin runs
// with. This is the SYNCHRONOUS, no-handshake baseline: `namespace`/
// `namespace_source` are resolveBaseNamespace's config+env-only view (always
// `degraded: true` — a live handshake has not been consulted yet), and
// recall_limit/recall_max_tokens/min_capture_chars fall back through
// effectiveSetting with no server settings (env-override > built-in default)
// when config doesn't set them explicitly. effectiveConfig (below) overlays a
// live handshake on top of this once one is available. `env`/`cwd` are
// injectable for tests.
export function resolveConfig(
  pluginConfig: any,
  env: Record<string, string | undefined> = process.env,
  cwd: string | undefined = harnessCwd(),
) {
  const c = pluginConfig || {};
  const base = resolveBaseNamespace(c, env);

  const recallLimitExplicit = Number.isFinite(c.recall_limit) && c.recall_limit > 0;
  const recallMaxTokensExplicit = Number.isFinite(c.recall_max_tokens) && c.recall_max_tokens > 0;
  const minCaptureCharsExplicit = Number.isFinite(c.min_capture_chars) && c.min_capture_chars > 0;

  return {
    enabled: c.enabled !== false,
    // Config wins; otherwise the canonical MEMINI_BASE_URL (alias MEMINI_URL),
    // matching the other integrations so one env setup works everywhere.
    base_url: c.base_url || strEnv(env, "MEMINI_BASE_URL") || strEnv(env, "MEMINI_URL") || DEFAULT_BASE_URL,
    // env > config > "openclaw" (see resolveBaseNamespace). `degraded: true`
    // here always — this view never consults the handshake.
    namespace: base.namespace,
    namespace_source: base.source as NamespaceSource,
    degraded: true,
    namespace_per_agent: c.namespace_per_agent !== false,
    namespace_template: c.namespace_template || DEFAULT_NAMESPACE_TEMPLATE,
    skip_without_agent: c.skip_without_agent === true,
    // On by default: system-initiated turns (cron/heartbeat/scheduled polls) are
    // skipped for both recall and capture even when they carry an agent identity,
    // so scheduled-task chatter doesn't pull long-term memory or accumulate as
    // episodic noise. Set skip_system_turns:false to recall/capture on them as
    // before. system_kinds overrides the matched kinds.
    skip_system_turns: c.skip_system_turns !== false,
    system_kinds:
      Array.isArray(c.system_kinds) && c.system_kinds.length
        ? c.system_kinds.map((k: any) => String(k).toLowerCase())
        : DEFAULT_SYSTEM_KINDS,
    fallback_on_error: c.fallback_on_error !== false,
    timeout_ms: c.timeout_ms || DEFAULT_TIMEOUT_MS,
    // On by default. The memory slot's automatic recall/capture cannot express
    // the levers the tools carry — scope (how wide to read), visibility (who
    // should know a fact), and the session briefing with its ancestor Scope
    // line. Without the tools an agent here simply does not have those
    // capabilities, and the curl-based memory skill is the only fallback — which
    // sends the BASE namespace and so silently misses per-agent memory. Set
    // expose_tools:false to restore the pre-0.6.9 slot-only surface.
    expose_tools: c.expose_tools !== false,
    // recall_limit/recall_max_tokens/min_capture_chars: config wins outright
    // when explicitly set (it is the most-local, most-deliberate layer); else
    // effectiveSetting resolves env-override > (a later live handshake's
    // server settings, once one exists) > built-in default. No server settings
    // are available yet here, so this is env-or-default only; effectiveConfig
    // recomputes these three once a handshake is in hand.
    recall_limit: recallLimitExplicit ? c.recall_limit : effectiveSetting<number>(knob("recall_limit"), undefined, env).value,
    recall_max_tokens: recallMaxTokensExplicit
      ? c.recall_max_tokens
      : effectiveSetting<number>(knob("inject_recall_max_tok"), undefined, env).value,
    min_capture_chars: minCaptureCharsExplicit
      ? c.min_capture_chars
      : effectiveSetting<number>(knob("min_capture_chars"), undefined, env).value,
    namespace_prefix: c.namespace_prefix || "",
    // home: the caller's personal namespace, sent as X-Memini-Home. Config
    // wins over MEMINI_HOME env (same precedence style as base_url); no
    // git/cwd derivation — unset means "no home leg", not a guess.
    home: c.home || strEnv(env, "MEMINI_HOME") || undefined,
    // Recorded so effectiveConfig() can tell "config set this explicitly" apart
    // from "resolved from env/default" — only the latter may be filled in from
    // a live handshake's server settings.
    explicit: {
      recall_limit: recallLimitExplicit,
      recall_max_tokens: recallMaxTokensExplicit,
      min_capture_chars: minCaptureCharsExplicit,
    },
    cwd,
  };
}

export type ResolvedConfig = ReturnType<typeof resolveConfig>;

/**
 * effectiveConfig overlays a live handshake result on top of the synchronous
 * `cfg` from resolveConfig():
 *
 *   namespace: MEMINI_NAMESPACE (cfg.namespace_source === "env") wins OUTRIGHT
 *     — even over a successful handshake. This is the deliberate departure
 *     from @memini/client's resolveNamespace precedence: a long-lived gateway
 *     install's local env pin should never be silently shadowed by whatever
 *     the server resolves. Otherwise a successful handshake wins (namespace_source
 *     becomes "server:<hs.namespace_source>", degraded false); with no env pin
 *     and no handshake, cfg's own config/default view stands (fail-soft to the
 *     declared/config value).
 *   recall_limit / recall_max_tokens / min_capture_chars: cfg.explicit (config
 *     set it outright) wins; otherwise effectiveSetting recomputes with the
 *     handshake's `settings` now available (env-override > server > default).
 *
 * `hs` may be undefined (network error, non-2xx, timeout, or a guard throw
 * swallowed per MEMINI_FALLBACK — see attemptHandshake). Exported for testing.
 */
export function effectiveConfig(
  cfg: ResolvedConfig,
  hs: HandshakeResult | undefined,
  env: Record<string, string | undefined> = process.env,
): ResolvedConfig {
  let namespace = cfg.namespace;
  let namespace_source = cfg.namespace_source;
  let degraded = true;

  if (cfg.namespace_source === "env") {
    // MEMINI_NAMESPACE always wins for namespace purposes; `degraded` still
    // reflects whether the handshake itself succeeded (behavior settings below
    // still benefit from a reachable server even though its namespace opinion
    // is overridden).
    degraded = !hs;
  } else if (hs) {
    namespace = hs.namespace;
    namespace_source = `server:${hs.namespace_source}`;
    degraded = false;
  }

  const server = hs?.settings;
  const explicit = cfg.explicit;

  return {
    ...cfg,
    namespace,
    namespace_source,
    degraded,
    recall_limit: explicit.recall_limit ? cfg.recall_limit : effectiveSetting<number>(knob("recall_limit"), server, env).value,
    recall_max_tokens: explicit.recall_max_tokens
      ? cfg.recall_max_tokens
      : effectiveSetting<number>(knob("inject_recall_max_tok"), server, env).value,
    min_capture_chars: explicit.min_capture_chars
      ? cfg.min_capture_chars
      : effectiveSetting<number>(knob("min_capture_chars"), server, env).value,
  };
}

// sanitizeNsSegment keeps a namespace segment header-safe (the server sanitizes
// too, but the X-Memini-Namespace value should be clean): alnum, dot, dash,
// underscore; collapse the rest to dashes and trim.
function sanitizeNsSegment(s: any) {
  return String(s).trim().replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
}

// Session keys look like "agent:<id>:..." (e.g. agent:carol:cron:...);
// extract the agent segment. Raw session UUIDs are NOT identities — treating
// them as such fragments memory into per-session namespaces.
const SESSION_KEY_AGENT = /(?:^|[:/])agent[:/]([^:/]+)/;

function parseAgentFromSessionKey(value: any) {
  if (typeof value !== "string") return "";
  const match = value.match(SESSION_KEY_AGENT);
  return match ? match[1] : "";
}

// agentIdentity pulls a stable per-agent id from the hook context. OpenClaw
// passes identity on ctx (PluginHookAgentContext), never on the event payload:
// ctx.agentId is the direct id, and ctx.sessionKey ("agent:<id>:…") carries it
// otherwise. Returns "" when neither identifies an agent.
function agentIdentity(ctx: any) {
  const id = ctx?.agentId;
  if (typeof id === "string" && id.trim()) return id.trim();
  return parseAgentFromSessionKey(ctx?.sessionKey);
}

// effectiveNamespace returns the configured namespace, or a per-agent namespace
// when namespace_per_agent is enabled and ctx identifies an agent. The per-agent
// name comes from namespace_template (default "{namespace}-{agent}"), with
// {agent} and {namespace} substituted — e.g. "{namespace}-{agent}" ->
// "openclaw-alice", "{agent}" -> "alice". Falls back to the base namespace when no agent id is present,
// preserving the shared-memory behavior — unless skip_without_agent is set, in
// which case it returns null so the caller skips the operation entirely (no
// recall, no write, no fallback namespace). Useful for gateways where
// unattributable sessions (cron, heartbeat) should not pollute memory.
export function effectiveNamespace(cfg: ResolvedConfig, ctx: any) {
  // Apply namespace_prefix to the base namespace if set
  const baseNs = cfg.namespace_prefix
    ? cfg.namespace_prefix.replace(/\/+$/, "") + "/" + cfg.namespace
    : cfg.namespace;

  if (!cfg.namespace_per_agent) return baseNs;
  const id = sanitizeNsSegment(agentIdentity(ctx));
  if (!id) return cfg.skip_without_agent ? null : baseNs;
  const tmpl = cfg.namespace_template || DEFAULT_NAMESPACE_TEMPLATE;
  return applyTemplate(tmpl, { agent: id, namespace: baseNs });
}

// detectSystemKind returns the system-turn kind from the hook context, else "".
// OpenClaw sets PluginHookAgentContext.trigger on every run ("user" for a real
// message, "heartbeat"/"cron" for system polls). Heartbeat and cron runs reuse
// the agent's main session and carry no distinguishing prompt text, so trigger
// is the only reliable signal — there's no marker or session-key segment to parse.
export function detectSystemKind(ctx: any, kinds: string[] = DEFAULT_SYSTEM_KINDS) {
  const trigger = typeof ctx?.trigger === "string" ? ctx.trigger.toLowerCase() : "";
  return trigger && kinds.includes(trigger) ? trigger : "";
}

// shouldSkipSystemTurn reports whether this turn should be skipped (no recall,
// no capture) because skip_system_turns is on and ctx.trigger is a system kind.
export function shouldSkipSystemTurn(cfg: ResolvedConfig, ctx: any) {
  return cfg.skip_system_turns && detectSystemKind(ctx, cfg.system_kinds) !== "";
}

// sessionIdentity pulls a stable per-session id from the hook context, used to
// tag captured turns (metadata.session_id) and then exclude this session's own
// just-captured turns from its pre-turn auto-recall — otherwise a turn still in
// the live transcript is recalled back as "long-term memory" the very next turn.
// Unlike agentIdentity (per-agent, shared across an agent's sessions), this is
// per-session, so two sessions of one agent don't suppress each other. It
// deliberately does NOT fall back to the agent id: that is too coarse and would
// exclude the agent's entire history from recall. Returns "" when ctx identifies
// no session; recall then skips the exclusion and agent_end skips the capture
// (an untagged capture could never be excluded and would echo back forever).
export function sessionIdentity(ctx: any) {
  // No ctx.runId fallback: runId is per-run (docs/plugins/hooks.md), so a
  // capture tagged with it carries an identity the next run's exclusion can
  // never match — the exact echo the session guard exists to prevent.
  for (const c of [ctx?.sessionId, ctx?.sessionKey]) {
    if (typeof c === "string" && c.trim()) return sanitizeNsSegment(c);
  }
  return "";
}

function extractText(content: any) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .flatMap((block: any) => {
      if (!block || typeof block !== "object") return [];
      if (block.type === "text" && typeof block.text === "string") return [block.text];
      return [];
    })
    .join("\n")
    .trim();
}

function lastTextByRole(messages: any, role: any) {
  for (const message of [...messages].reverse()) {
    if (!message || typeof message !== "object" || message.role !== role) continue;
    const text = extractText(message.content);
    if (text) return text;
  }
  return "";
}

// OpenClaw prepends runtime plumbing to the user turn that the model sees:
// one or more "<Label> (untrusted metadata):" blocks (chat/sender info).
// Each block is followed by EITHER a fenced JSON object (```json ... ```)
// OR flat key=value lines (chat_id=...\nmessage_id=...). That metadata is
// explicitly untrusted and is not memory — captured verbatim it dominates a
// namespace and recalls at high similarity (it's templated), crowding out
// real memories. Strip the leading metadata blocks and keep the actual
// message that follows.
//
// The flat branch matches only lines that look like a bare `key=value`
// (identifier immediately before the `=`), so a following real message that
// merely contains an `=` — e.g. "User: set FOO=bar" — is NOT consumed: it
// starts with "User" then ":", not "=", so the key anchor fails and the run
// stops. Without this anchor a greedy `[^\n]*=[^\n]*` would eat that message.
const UNTRUSTED_METADATA_BLOCK =
  /^\s*[^\n]*\(untrusted metadata\):\s*(?:```(?:json)?\s*[\s\S]*?```\s*|(?:[ \t]*[A-Za-z_][\w.-]*=[^\n]*\n?)+)/;

// Noise prefixes to drop after preamble stripping (backstop for shouldSkipSystemTurn).
const NOISE_PREFIXES = ["[Subagent Context]", "[cron:"];

export function stripRuntimePreambles(text: any) {
  if (typeof text !== "string") return text;
  let out = text;
  while (UNTRUSTED_METADATA_BLOCK.test(out)) {
    out = out.replace(UNTRUSTED_METADATA_BLOCK, "");
  }
  return out.trim();
}

// OpenClaw's production turn text keeps a leading role label ("User: ") even
// after preamble stripping, so a raw startsWith would miss "User: [cron: …]".
// detectSystemKind (ctx.trigger) is the primary cron/heartbeat signal; this
// text backstop catches a NOISE_PREFIXES marker that still reaches the captured
// content, whether it leads the turn or sits just behind that role label. A
// real message that merely mentions a marker mid-sentence is not matched.
const ROLE_LABEL = /^(?:user|assistant|system)\s*:\s*/i;

export function startsWithNoisePrefix(text: string) {
  const body = text.replace(ROLE_LABEL, "");
  return NOISE_PREFIXES.some((p) => body.startsWith(p));
}

// Mirrors the opencode plugin's labels toggle. Default is plain bullets (no
// label prefix), matching the opencode/hermes/Claude Code plugins; set
// MEMINI_INJECT_LABELS=tier (or confidence/age) to add the annotation prefix.
const DEFAULT_RECALL_LABELS: string[] = [];

function recallLabels() {
  const raw = process.env.MEMINI_INJECT_LABELS;
  if (!raw) return DEFAULT_RECALL_LABELS;
  return raw
    .split(/[|,]/)
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);
}

function formatAge(createdAt: any) {
  if (!createdAt) return "";
  const t = new Date(createdAt).getTime();
  if (!Number.isFinite(t)) return "";
  const days = Math.floor((Date.now() - t) / 86400000);
  if (days < 0) return "";
  return days === 0 ? "today" : `${days}d`;
}

// formatResults renders the hits to an array of bullet lines; the caller fits
// the array under a token budget (fitByTokens) before joining. Returns [] for
// no hits.
function formatResults(results: any, labels: string[] = DEFAULT_RECALL_LABELS): string[] {
  if (!Array.isArray(results) || results.length === 0) return [];
  return results
    .map((result: any, index: number) => {
      const mem = result?.memory ?? {};
      const text = (mem.summary || mem.content || `Memory ${index + 1}`).trim();
      if (labels.length === 0) {
        const tier = (mem.tier || "memory").trim();
        return `- (${tier}) ${text.slice(0, 300)}`;
      }
      const tagParts: string[] = [];
      if (labels.includes("tier") && mem.tier) tagParts.push(mem.tier);
      if (labels.includes("confidence") && typeof mem.confidence === "number") {
        tagParts.push(`conf=${mem.confidence.toFixed(2)}`);
      }
      if (labels.includes("age")) {
        const age = formatAge(mem.created_at || mem.createdAt);
        if (age) tagParts.push(age);
      }
      if (labels.includes("reason") && Array.isArray(mem.tags) && mem.tags.includes("pinned")) {
        tagParts.push("pinned");
      }
      if (tagParts.length === 0) {
        const tier = (mem.tier || "memory").trim();
        return `- (${tier}) ${text.slice(0, 300)}`;
      }
      return `- [${tagParts.join(" · ")}] ${text.slice(0, 300)}`;
    })
    .filter(Boolean);
}

// approxTokens / fitByTokens mirror the Claude Code plugin's _shared.mjs so the
// recall block can honor a token ceiling. Keep contracts identical when both
// sides change. Exported for testing.
export function approxTokens(text: any) {
  if (!text) return 0;
  const words = String(text).trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.ceil((words * 4) / 3));
}

// fitByTokens trims a list of pre-formatted bullet lines to fit under
// `maxTokens`, keeping the head (most-relevant first). maxTokens <= 0 means
// unbounded. Returns the kept lines plus the dropped count for a footer.
export function fitByTokens(items: string[], maxTokens: number) {
  if (!Array.isArray(items) || items.length === 0) return { items: [] as string[], dropped: 0 };
  if (!Number.isFinite(maxTokens) || maxTokens <= 0) return { items: items.slice(), dropped: 0 };
  const out: string[] = [];
  let used = 0;
  let dropped = 0;
  for (const s of items) {
    const t = approxTokens(s);
    if (used + t > maxTokens) {
      dropped++;
      continue;
    }
    out.push(s);
    used += t;
  }
  return { items: out, dropped };
}

function plaintextBearerAuthMessage(baseUrl: any) {
  return `memini: MEMINI_API_KEY is configured for plaintext HTTP to ${baseUrl}. Bearer tokens and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
}

export function createPlaintextBearerAuthGuard(warn: any, env?: any) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl: any, secret: any) {
    if (!isPlaintextBearerUnsafe(baseUrl, secret || "")) return;
    const message = plaintextBearerAuthMessage(baseUrl);
    if ((env || process.env).MEMINI_REQUIRE_HTTPS === "1") throw new Error(message);
    if (!warned) {
      warned = true;
      warn(message);
    }
  };
}

interface MeminiClient {
  postJson: (path: string, payload: any, ns?: string) => Promise<any>;
  getJson: (path: string, ns?: string) => Promise<any>;
  deleteJson: (path: string, ns?: string) => Promise<any>;
  // postJsonResult is postJson without the degrade-to-null: it hands back the
  // server's own error text. The explicit write tool uses it, because a rejected
  // write is information the model can act on — a `visibility` naming an unknown
  // ancestor errors listing the valid chain, which is how the model learns the
  // topology. Swallowing that into `success: false` leaves it nothing to correct
  // against. It still never throws.
  postJsonResult: (path: string, payload: any, ns?: string) => Promise<{ ok: boolean; data?: any; error?: string }>;
  baseUrl: string;
}

function createClient(cfg: ResolvedConfig, api: any): MeminiClient {
  const baseUrl = String(cfg.base_url || DEFAULT_BASE_URL).replace(/\/+$/, "");
  const timeoutMs = Number(cfg.timeout_ms || DEFAULT_TIMEOUT_MS);
  const fallbackOnError = cfg.fallback_on_error !== false;
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
  const home = cfg.home;
  const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m: any) => api.logger.warn?.(m));

  async function postJson(path: string, payload: any, ns?: string) {
    if (process.env.MEMINI_REQUIRE_HTTPS === "1") guardPlaintextBearerAuth(baseUrl, secret);
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers: Record<string, string> = { "Content-Type": "application/json", "X-Memini-Namespace": ns || cfg.namespace };
    if (secret) headers.Authorization = `Bearer ${secret}`;
    if (home) headers["X-Memini-Home"] = home;
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (!res.ok) {
        if (fallbackOnError) {
          // Degrade but never silently: a swallowed 401/500 on a capture or
          // recall looks like "memory isn't working" with nothing to debug.
          api.logger.warn?.(`memini POST ${path} failed: ${res.status}`);
          return null;
        }
        const body = await res.text().catch(() => "");
        throw new Error(`memini ${path} failed: ${res.status} ${body}`);
      }
      return await res.json();
    } catch (error) {
      if (!fallbackOnError) throw error;
      api.logger.warn?.(`memini: ${String(error)}`);
      return null;
    }
  }

  async function getJson(path: string, ns?: string) {
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers: Record<string, string> = { "X-Memini-Namespace": ns || cfg.namespace };
    if (secret) headers.Authorization = `Bearer ${secret}`;
    if (home) headers["X-Memini-Home"] = home;
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "GET",
        headers,
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (!res.ok) {
        if (fallbackOnError) {
          api.logger.warn?.(`memini GET ${path} failed: ${res.status}`);
          return null;
        }
        const body = await res.text().catch(() => "");
        throw new Error(`memini GET ${path} failed: ${res.status} ${body}`);
      }
      return await res.json();
    } catch (error) {
      if (!fallbackOnError) throw error;
      api.logger.warn?.(`memini: ${String(error)}`);
      return null;
    }
  }

  async function deleteJson(path: string, ns?: string) {
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers: Record<string, string> = { "X-Memini-Namespace": ns || cfg.namespace };
    if (secret) headers.Authorization = `Bearer ${secret}`;
    if (home) headers["X-Memini-Home"] = home;
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "DELETE",
        headers,
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (!res.ok) {
        if (fallbackOnError) {
          api.logger.warn?.(`memini DELETE ${path} failed: ${res.status}`);
          return null;
        }
        const body = await res.text().catch(() => "");
        throw new Error(`memini DELETE ${path} failed: ${res.status} ${body}`);
      }
      // 204 No Content has an empty body; treat a successful status as ok.
      return await res.json().catch(() => ({ ok: true }));
    } catch (error) {
      if (!fallbackOnError) throw error;
      api.logger.warn?.(`memini: ${String(error)}`);
      return null;
    }
  }

  // See MeminiClient.postJsonResult. Never throws — a failure is reported as
  // {ok:false, error} regardless of fallback_on_error, so a tool call degrades
  // into an answer rather than an exception in the host.
  async function postJsonResult(
    path: string,
    payload: any,
    ns?: string,
  ): Promise<{ ok: boolean; data?: any; error?: string }> {
    try {
      guardPlaintextBearerAuth(baseUrl, secret);
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
        "X-Memini-Namespace": ns || cfg.namespace,
      };
      if (secret) headers.Authorization = `Bearer ${secret}`;
      if (home) headers["X-Memini-Home"] = home;
      const res = await fetch(`${baseUrl}${path}`, {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (!res.ok) {
        const detail = (await res.text().catch(() => "")).trim();
        api.logger.warn?.(`memini POST ${path} failed: ${res.status} ${detail}`);
        return { ok: false, error: detail || `HTTP ${res.status}` };
      }
      return { ok: true, data: await res.json().catch(() => ({})) };
    } catch (error) {
      api.logger.warn?.(`memini: ${String(error)}`);
      return { ok: false, error: String(error) };
    }
  }

  return { postJson, getJson, deleteJson, postJsonResult, baseUrl };
}

// meminiBriefingPath builds the GET /v1/namespaces/briefing query string for the
// memory_briefing tool. The endpoint is header-scoped (X-Memini-Namespace), so
// there is no namespace in the path — the model never names one. An unrecognized
// scope is dropped rather than forwarded: the server 400s on one, and a bad guess
// must not turn orientation into an error.
export function meminiBriefingPath(args: any) {
  const scope = String(args?.scope || "").trim();
  return VALID_SCOPES.includes(scope)
    ? `/v1/namespaces/briefing?scope=${encodeURIComponent(scope)}`
    : "/v1/namespaces/briefing";
}

// meminiListPath builds the GET /v1/memories query string for the memory_list
// tool: repeatable tier/tag params plus meta=key=value pairs. encodeURIComponent
// escapes the '=' inside each meta value, which the server decodes and splits on.
export function meminiListPath(args: any) {
  const parts: string[] = [];
  for (const t of args?.tiers || []) parts.push(`tier=${encodeURIComponent(String(t))}`);
  for (const tag of args?.tags || []) parts.push(`tag=${encodeURIComponent(String(tag))}`);
  for (const [k, v] of Object.entries(args?.metadata || {})) {
    parts.push(`meta=${encodeURIComponent(`${k}=${v}`)}`);
  }
  if (Number.isInteger(args?.limit) && args.limit > 0) parts.push(`limit=${args.limit}`);
  return parts.length ? `/v1/memories?${parts.join("&")}` : "/v1/memories";
}

// --- in-memory memoization (no file cache; one instance per session) --------

interface Memo<T> {
  get(): Promise<T>;
  invalidate(): void;
}

/**
 * Wrap a zero-arg async fn so it's called at most once per `ttlMs`, returning
 * the cached value in between. `now` is injectable so tests can drive expiry
 * without a real wait. Exported for testing.
 */
export function memoizeAsync<T>(fn: () => Promise<T>, ttlMs: number, now: () => number = Date.now): Memo<T> {
  let cached: { value: T; expiresAt: number } | null = null;
  return {
    async get() {
      const t = now();
      if (!cached || t >= cached.expiresAt) {
        const value = await fn();
        cached = { value, expiresAt: t + ttlMs };
      }
      return cached.value;
    },
    invalidate() {
      cached = null;
    },
  };
}

/**
 * gatewayFacts builds the handshake's project facts: cwd_basename (best-effort,
 * for logging only) plus declared_namespace — the operator's configured
 * `namespace` (or the literal "openclaw" default), NEVER whatever
 * MEMINI_NAMESPACE happens to be set to locally (that is a separate,
 * higher-precedence local override — see effectiveConfig). Deliberately no
 * remote_url/toplevel_path: this is a gateway, not a per-repo client, and its
 * cwd is the daemon's process directory, not a project's — sending git facts
 * derived from it could collide with an unrelated pin set for whatever repo
 * the daemon happens to be checked out inside. Exported for testing.
 */
export function gatewayFacts(pluginConfig: any, cwd: string | undefined): ProjectFacts {
  const c = pluginConfig || {};
  const configured = typeof c.namespace === "string" ? c.namespace.trim() : "";
  return {
    cwd_basename: basename(cwd || process.cwd() || "."),
    declared_namespace: configured || DEFAULT_NAMESPACE,
  };
}

/**
 * The subset of ProjectFacts that can key a server-side pin (PUT/DELETE
 * /v1/pins): remote_url and/or toplevel_path, best-effort derived from the
 * gateway's own cwd via @memini/client's gatherFacts. A pin set this way is
 * NOT picked up by this gateway's own handshake (gatewayFacts above sends no
 * git facts on purpose) — it exists for operator record-keeping and for other
 * tooling inspecting the same checkout (memini doctor, another client sharing
 * that directory), not as this plugin's own resolution lever. Exported for
 * testing.
 */
export function pinKeyFacts(
  cwd: string | undefined,
  env: Record<string, string | undefined> = process.env,
): { remote_url?: string; toplevel_path?: string } {
  const facts = gatherFacts(cwd || process.cwd(), env);
  const out: { remote_url?: string; toplevel_path?: string } = {};
  if (facts.remote_url) out.remote_url = facts.remote_url;
  if (facts.toplevel_path) out.toplevel_path = facts.toplevel_path;
  return out;
}

/**
 * Attempt a live handshake. performHandshake is already fail-soft for network
 * errors, non-2xx, malformed JSON, and timeouts (returns undefined) — the one
 * exception is the plaintext-bearer guard, which throws on purpose. Whether
 * THAT throw is swallowed here is what `fallbackOnError` (fallback_on_error,
 * itself sourced from MEMINI_FALLBACK / config) decides.
 */
async function attemptHandshake(
  boot: Bootstrap,
  facts: ProjectFacts,
  fallbackOnError: boolean,
): Promise<HandshakeResult | undefined> {
  try {
    return await performHandshake(boot, facts, {
      timeoutMs: HANDSHAKE_TIMEOUT_MS,
      clientName: CLIENT_NAME,
      clientVersion: CLIENT_VERSION,
    });
  } catch (error) {
    if (!fallbackOnError) throw error;
    return undefined;
  }
}

export interface SessionContext {
  boot: Bootstrap;
  cfg: ResolvedConfig;
  facts: ProjectFacts;
  memo: Memo<HandshakeResult | undefined>;
}

/**
 * Build a plugin instance's fixed inputs (bootstrap + the synchronous cfg
 * baseline + gateway facts) and its memoized handshake. `now` is injectable
 * for TTL tests. Exported for testing.
 */
export function createSessionContext(
  pluginConfig: any,
  env: Record<string, string | undefined> = process.env,
  cwd: string | undefined = harnessCwd(),
  now: () => number = Date.now,
): SessionContext {
  const boot = readBootstrap(env);
  const cfg = resolveConfig(pluginConfig, env, cwd);
  const facts = gatewayFacts(pluginConfig, cwd);
  const memo = memoizeAsync(() => attemptHandshake(boot, facts, cfg.fallback_on_error), HANDSHAKE_TTL_MS, now);
  return { boot, cfg, facts, memo };
}

/** Resolve `ctx`'s current effective config, triggering (or reusing) the memoized handshake. */
export async function sessionLive(
  ctx: SessionContext,
  env: Record<string, string | undefined> = process.env,
): Promise<ResolvedConfig> {
  const hs = await ctx.memo.get();
  return effectiveConfig(ctx.cfg, hs, env);
}

// --- pins (/memini:namespace) -------------------------------------------------

async function pinsRequest(
  boot: Bootstrap,
  method: "PUT" | "DELETE",
  body: unknown,
): Promise<{ ok: boolean; status: number; body: any }> {
  assertBearerTransportSafe(boot.baseUrl, boot.apiKey); // throws under MEMINI_REQUIRE_HTTPS
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (boot.apiKey) headers.Authorization = `Bearer ${boot.apiKey}`;
  if (boot.homeEnv) headers["X-Memini-Home"] = boot.homeEnv;
  const res = await fetch(`${boot.baseUrl}/v1/pins`, {
    method,
    headers,
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(5000),
  });
  let parsed: any = null;
  try {
    parsed = await res.json();
  } catch {
    // 204 (delete) and empty bodies parse to null — fine.
  }
  return { ok: res.ok, status: res.status, body: parsed };
}

function pinErrorMessage(res: { body: any; status: number }): string {
  return res.body?.error || res.body?.message || `HTTP ${res.status}`;
}

// offlineMessage points at the config `namespace` value — openclaw's own local
// override lever — rather than MEMINI_NAMESPACE: pi/the Claude plugin point at
// the env var because they have no config object of their own, but openclaw
// already has one, and (per effectiveConfig) MEMINI_NAMESPACE would win over a
// pin anyway, making it the wrong thing to suggest here.
function offlineMessage(boot: Bootstrap, error: unknown): string {
  const detail = String((error as any)?.message || error);
  return (
    `${detail}\n\nCould not reach the memini server at ${boot.baseUrl}. Pins live on the server, so ` +
    `setting one needs it reachable. Set the config \`namespace\` value instead for a static, ` +
    `machine-local choice.`
  );
}

// --- /memini:status + /memini:namespace --------------------------------------

// statusGet is the diagnostics-only GET. It bypasses the client's
// warn-and-return-null degrade path: a failed probe here is data for the report,
// not a fault to log. Silent on purpose — the caller decides what a miss means,
// and for /healthz a miss means "not exposed", not "server down".
async function statusGet(boot: Bootstrap, namespace: string, path: string): Promise<any> {
  const baseUrl = String(boot.baseUrl || DEFAULT_BASE_URL).replace(/\/+$/, "");
  const headers: Record<string, string> = { "X-Memini-Namespace": namespace };
  if (boot.apiKey) headers.Authorization = `Bearer ${boot.apiKey}`;
  if (boot.homeEnv) headers["X-Memini-Home"] = boot.homeEnv;
  try {
    const res = await fetch(`${baseUrl}${path}`, {
      method: "GET",
      headers,
      signal: AbortSignal.timeout(STATUS_TIMEOUT_MS),
    });
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null;
  }
}

interface ServerReport {
  reachable: boolean;
  latencyMs: number;
  readSet?: any;
  version?: string;
  deps?: any;
  healthExposed?: boolean;
}

// fetchServer probes the server the way the plugin actually uses it.
//
// Reachability is decided by /v1/namespaces/readset, not /healthz: a remote
// memini typically sits behind an ingress that routes only /v1 and /mcp, so
// /healthz 404s while the server is perfectly healthy, and calling that "server
// down" would be a false alarm on the most common deployment. The read set
// doubles as the probe — it is the server's own introspection of where a plain
// recall looks, so it cannot drift from what recall really does.
export async function fetchServer(boot: Bootstrap, namespace: string): Promise<ServerReport> {
  const started = Date.now();
  const readSet = await statusGet(boot, namespace, "/v1/namespaces/readset");
  const out: ServerReport = { reachable: readSet != null, latencyMs: Date.now() - started, readSet };
  // Dependency detail, when the deployment exposes it. A miss is "not routed",
  // not "broken" — so it never touches `reachable`.
  const health = await statusGet(boot, namespace, "/healthz?verbose=1");
  if (health) {
    out.version = health.version;
    out.deps = health.deps;
  } else {
    out.healthExposed = false;
  }
  return out;
}

const pad = (s: any, n: number) => String(s).padEnd(n);

interface Warning {
  level: "warn" | "note";
  code: string;
  message: string;
  fix?: string;
}

/** Build the warnings section from bootstrap + live config + handshake. Exported for testing. */
export function buildWarnings(boot: Bootstrap, live: ResolvedConfig, hs: HandshakeResult | undefined): Warning[] {
  const warnings: Warning[] = [];

  if (live.degraded) {
    warnings.push({
      level: "warn",
      code: "degraded-mode",
      message: `could not reach the memini server at ${boot.baseUrl}: recall_limit/recall_max_tokens/min_capture_chars fall back to config/env/built-in defaults, not the server's.`,
      fix: "Check base_url/MEMINI_BASE_URL and that the server is running.",
    });
  }

  if (live.namespace_source === "env") {
    warnings.push({
      level: "warn",
      code: "global-namespace-pin",
      message: `MEMINI_NAMESPACE is set to "${boot.namespaceEnv}", which ALWAYS overrides this gateway's namespace resolution — even a server-side pin cannot take effect while it is set. If it is exported from a shell rc, every repo (and every gateway) on this machine shares one memory pool.`,
      fix: "Unset MEMINI_NAMESPACE and use the `namespace` config value instead for a static, per-install choice.",
    });
  }

  if (!live.home) {
    warnings.push({
      level: "note",
      code: "home-unset",
      message: "no personal namespace is configured: no personal leg merges into recall.",
      fix: "Set the `home` config value or export MEMINI_HOME=personal/<you>.",
    });
  }

  if (isPlaintextBearerUnsafe(boot.baseUrl, boot.apiKey)) {
    warnings.push({
      level: "warn",
      code: "plaintext-bearer",
      message: `a bearer token is configured for plaintext HTTP to ${boot.baseUrl}; the token and your memory payloads can be observed on the network.`,
      fix: "Use HTTPS, or tunnel over SSH. Set MEMINI_REQUIRE_HTTPS=1 to make this an error.",
    });
  }

  return warnings;
}

// renderStatus formats the report. `effective` is the namespace this surface
// would actually send (base + prefix + per-agent template); the NAMESPACE block
// reports the BASE chain, because that is the only leg env/config participate
// in. Exported for testing — the assertion that matters (a bearer token is
// never printed in full) is on this string.
export function renderStatus(
  boot: Bootstrap,
  live: ResolvedConfig,
  hs: HandshakeResult | undefined,
  effective: string,
  server: ServerReport,
): string {
  const L: string[] = [];

  L.push(`memini — effective settings (openclaw)`);
  L.push("");

  L.push(`NAMESPACE`);
  L.push(`  ${pad("this surface sends", 28)} ${effective}`);
  L.push(`  ${pad("base", 28)} ${pad(live.namespace, 34)} <- ${live.namespace_source}`);
  if (live.namespace_source === "server:pin" && hs?.pin) {
    L.push(`  ${pad("pin", 28)} key ${hs.pin.key}${hs.pin.created_by ? `, set by ${hs.pin.created_by}` : ""}`);
  }
  L.push(`  ${pad("per-agent", 28)} ${live.namespace_per_agent ? live.namespace_template : "off"}`);
  if (live.namespace_prefix) L.push(`  ${pad("prefix", 28)} ${live.namespace_prefix}`);
  L.push(`  ${pad("home (personal)", 28)} ${live.home || "(unset)"}`);
  L.push("");

  // Behavior knobs relevant to this plugin, with provenance.
  L.push(`SETTINGS`);
  for (const wireKey of ["recall_limit", "inject_recall_max_tok", "min_capture_chars"]) {
    const k = knob(wireKey);
    const { value, source } = effectiveSetting(k, hs?.settings, process.env as Record<string, string | undefined>);
    const origin = source === "env-override" ? "<- env" : source === "server" ? "<- server" : "(default)";
    L.push(`  ${pad(k.envName.replace(/^MEMINI_/, "").toLowerCase(), 28)} ${pad(String(value), 22)} ${origin}`);
  }
  L.push("");

  // The plugin's own config, which the shared knob table knows nothing about. The
  // bearer is the one the requests actually carry — redacted, since a settings
  // dump is the likeliest place a token gets pasted into an issue.
  const secret = boot.apiKey;
  L.push(`PLUGIN`);
  L.push(`  ${pad("base_url", 28)} ${boot.baseUrl}`);
  L.push(`  ${pad("bearer", 28)} ${secret ? redactValue(secret) : "(none)"}`);
  L.push(`  ${pad("expose_tools", 28)} ${live.expose_tools ? "on" : "off"}`);
  L.push(`  ${pad("skip_system_turns", 28)} ${live.skip_system_turns ? "on" : "off"}`);
  L.push("");

  L.push(`SERVER`);
  if (!server.reachable) {
    L.push(`  ${pad("reachable", 28)} NO — could not reach ${boot.baseUrl}`);
  } else {
    const ver = server.version ? `, ${server.version}` : "";
    L.push(`  ${pad("reachable", 28)} yes (${server.latencyMs}ms${ver})`);
    const d = server.deps || {};
    if (d.store) L.push(`  ${pad("store", 28)} ${d.store.ok ? "ok" : `FAILING — ${d.store.last_error || "?"}`}`);
    if (d.embedder) {
      L.push(`  ${pad("embedder", 28)} ${d.embedder.ok ? "ok" : `FAILING — ${d.embedder.last_error || "?"}`}`);
    }
    if (server.healthExposed === false) {
      L.push(`  ${pad("dependency detail", 28)} unavailable (/healthz not routed — normal behind an ingress)`);
    }
  }
  L.push("");

  if (server.readSet?.entries?.length) {
    L.push(`READ SET for "${effective}" — where a plain recall looks`);
    L.push(`  ${pad("NAMESPACE", 34)} ${pad("ORIGIN", 12)} TIERS`);
    for (const e of server.readSet.entries) {
      const tiers = Array.isArray(e.tiers) && e.tiers.length ? e.tiers.join(",") : "all";
      L.push(`  ${pad(e.namespace, 34)} ${pad(e.origin, 12)} ${tiers}`);
    }
    L.push("");
  }

  const warnings = buildWarnings(boot, live, hs);
  if (warnings.length) {
    L.push(`WARNINGS`);
    for (const w of warnings) {
      L.push(`  [${w.level === "warn" ? "!" : "i"}] ${w.code}: ${w.message}`);
      if (w.fix) L.push(`      fix: ${w.fix}`);
    }
  } else {
    L.push(`No problems detected.`);
  }

  return L.join("\n");
}

/**
 * registerMeminiCommands wires memini:status and memini:namespace.
 *
 * The namespace command no longer writes a local override file: `<namespace>`/
 * `--clear` now PUT/DELETE a server-side pin (POST/DELETE /v1/pins) keyed by
 * pinKeyFacts (best-effort git facts from the gateway's own cwd) and drop the
 * in-memory handshake memo afterward, so the very next hook/tool call
 * re-resolves. Note that MEMINI_NAMESPACE, when set, wins over any pin for
 * THIS plugin's own resolution regardless (see effectiveConfig) — the command
 * still lets an operator record a pin for other tooling to see.
 *
 * Exported for testing.
 */
export function registerMeminiCommands(api: any, ctx: SessionContext) {
  const describe = async () => {
    const hs = await ctx.memo.get();
    return { hs, live: effectiveConfig(ctx.cfg, hs, process.env as Record<string, string | undefined>) };
  };

  api.registerCommand({
    name: "memini:status",
    description: "Show memini's effective settings: namespace + provenance, connection, server read set",
    acceptsArgs: false,
    async handler(cmdCtx: any) {
      try {
        const { hs, live } = await describe();
        // What THIS surface would send: the base, plus prefix and the per-agent
        // template. PluginCommandContext carries the sessionKey, so an
        // agent-keyed conversation resolves its own namespace here.
        const effective = effectiveNamespace(live, cmdCtx) ?? live.namespace;
        const server = await fetchServer(ctx.boot, effective);
        return { text: renderStatus(ctx.boot, live, hs, effective, server) };
      } catch (error) {
        // A command must never throw into the host.
        return { text: `memini: status failed: ${String(error)}` };
      }
    },
  });

  api.registerCommand({
    name: "memini:namespace",
    description: "Show, set, or --clear the memini namespace pin for this gateway's checkout (server-side)",
    acceptsArgs: true,
    async handler(cmdCtx: any) {
      try {
        const arg = String(cmdCtx?.args || "").trim();

        if (!arg) {
          const { hs, live } = await describe();
          const out: string[] = [`namespace: ${live.namespace}  (source: ${live.namespace_source})`];
          if (live.degraded) {
            out.push("");
            out.push(`Could not reach ${ctx.boot.baseUrl}, so this is a local guess (config/env), not the`);
            out.push(`server's authority. A pin (if any) can only be read from the server.`);
          } else if (live.namespace_source === "server:pin" && hs?.pin) {
            out.push(`pin:       key ${hs.pin.key}${hs.pin.created_by ? `, set by ${hs.pin.created_by}` : ""}`);
          } else if (live.namespace_source === "env") {
            out.push("");
            out.push(`MEMINI_NAMESPACE overrides this gateway's resolution — a server-side pin cannot`);
            out.push(`take effect while it is set.`);
          }
          out.push("");
          out.push(`Set a pin with:    /memini:namespace <namespace>`);
          out.push(`Clear it with:     /memini:namespace --clear`);
          return { text: out.join("\n") };
        }

        const cwd = ctx.cfg.cwd;
        if (!cwd) {
          return {
            text: "memini: no working directory is available, so a pin cannot be keyed. Set the `namespace` config value or MEMINI_NAMESPACE instead.",
          };
        }

        if (arg === "--clear" || arg === "clear") {
          const keyFacts = pinKeyFacts(cwd);
          if (!keyFacts.remote_url && !keyFacts.toplevel_path) {
            return {
              text: "memini: this gateway's working directory has no git remote or toplevel, so it cannot have a pin to clear.",
            };
          }
          let res;
          try {
            res = await pinsRequest(ctx.boot, "DELETE", keyFacts);
          } catch (error) {
            return { text: `memini: ${offlineMessage(ctx.boot, error)}` };
          }
          if (res.status === 404) {
            return { text: `No pin was set for this checkout — nothing to clear.` };
          }
          if (!res.ok) {
            return { text: `memini: could not clear the pin: ${pinErrorMessage(res)}` };
          }
          ctx.memo.invalidate();
          return {
            text: [
              `namespace pin cleared for this checkout.`,
              ``,
              `Recall and capture use the new resolution from the next turn.`,
            ].join("\n"),
          };
        }

        const ns = normalizeNamespace(arg);
        const bad = validateNamespace(ns);
        if (bad) {
          // Fail loudly rather than silently normalize into something the caller
          // did not ask for — and CR/LF would split the X-Memini-Namespace header
          // outright.
          return { text: `memini: invalid namespace ${JSON.stringify(arg)}: ${bad}` };
        }
        const keyFacts = pinKeyFacts(cwd);
        if (!keyFacts.remote_url && !keyFacts.toplevel_path) {
          return {
            text:
              `memini: this gateway's working directory has no git remote or toplevel to pin a namespace ` +
              `to. Set the \`namespace\` config value instead for a static, machine-local choice.`,
          };
        }
        let res;
        try {
          res = await pinsRequest(ctx.boot, "PUT", { namespace: ns, ...keyFacts });
        } catch (error) {
          return { text: `memini: ${offlineMessage(ctx.boot, error)}` };
        }
        if (!res.ok) {
          return { text: `memini: could not set the pin: ${pinErrorMessage(res)}` };
        }
        ctx.memo.invalidate();
        const entry = res.body || {};
        return {
          text: [
            `namespace pinned: ${entry.namespace || ns}`,
            `project key:      ${entry.key || keyFacts.remote_url || keyFacts.toplevel_path}`,
            ``,
            `This pin is not picked up by this gateway's own next handshake (it sends no git facts —`,
            `see gatewayFacts); it is recorded for other tooling sharing this checkout. To change`,
            `THIS gateway's own resolution, set the \`namespace\` config value instead.`,
          ].join("\n"),
        };
      } catch (error) {
        return { text: `memini: namespace failed: ${String(error)}` };
      }
    },
  });
}

const TOOL_NAMES = ["memory_recall", "memory_briefing", "memory_list", "memory_remember", "memory_forget"];

// registerMeminiTools registers memory_recall / memory_list / memory_remember.
//
// Registered as a tool factory, not plain tool objects: the calling agent's
// identity is on the factory's OpenClawPluginToolContext (agentId/sessionKey),
// not the execute callback (signature toolCallId, params, signal, onUpdate). The
// host wraps a plain-object tool as `(_ctx) => tool`, discarding ctx, so it would
// always hit the base namespace — empty under per-agent namespaces. The factory
// resolves the namespace from its ctx and binds it into each execute closure.
//
// The factory itself still returns synchronously (OpenClaw's contract): the
// live handshake is awaited INSIDE each tool's already-async execute(), not
// before the factory returns the tool array.
export function registerMeminiTools(api: any, client: MeminiClient, ctx: SessionContext) {
  const text = (obj: any) => ({ content: [{ type: "text", text: JSON.stringify(obj) }] });
  const Tags = Type.Optional(
    Type.Array(Type.String(), { description: "Match only memories carrying every listed tag (AND)." }),
  );
  const Metadata = Type.Optional(
    Type.Record(Type.String(), Type.String(), {
      description: 'Match memories whose top-level metadata contains each key=value pair, e.g. {"category":"bug_fixes"}.',
    }),
  );
  // scope / visibility are the two semantic levers the model gets over
  // namespaces — it never constructs a raw namespace path. Wording tracks the
  // MCP server's tool schemas (internal/api/mcp) so the story is the same on
  // every harness.
  const Scope = Type.Optional(
    Type.String({
      enum: VALID_SCOPES,
      description:
        "How wide to read: 'project' = just this project's own memories; 'full' (default) = project plus " +
        "inherited context (ancestors, your personal namespace, links); 'everywhere' = full plus nested " +
        "sub-projects.",
    }),
  );

  // provenance renders a hit's read-set origin: which namespace it lives in and,
  // for a hit off an ancestor/home/link leg, which leg it came from. Both are
  // omitted when empty, so a project-only recall carries no "from" noise at all
  // and the model reads an absent "from" as "this project's own memory".
  const provenance = (mem: any, from: any) => {
    const out: any = {};
    if (mem?.namespace) out.namespace = mem.namespace;
    if (from) out.from = from;
    return out;
  };

  // effectiveNamespace yields null only under skip_without_agent; tools have no
  // skip, so fall back to base. Landing on the base under per-agent mode means no
  // agent resolved — warn once so an empty result reads as "wrong namespace",
  // not "no memories".
  let warnedMissingAgent = false;
  const nsForCtx = (live: ResolvedConfig, toolCtx: any) => {
    const ns = effectiveNamespace(live, toolCtx) ?? live.namespace;
    if (live.namespace_per_agent && ns === live.namespace && !warnedMissingAgent) {
      warnedMissingAgent = true;
      api.logger?.warn?.(
        `memini: tool call could not resolve an agent; querying base namespace "${live.namespace}". ` +
          `Per-agent memory lives under "${live.namespace_template}" and will not appear here.`,
      );
    }
    return ns;
  };

  const buildTools = (toolCtx: any) => [
    {
      name: "memory_recall",
      description:
        "Search prior context in long-term memory (memini) via hybrid (semantic + keyword) retrieval, " +
        "ranked by relevance, recency, and corroboration. Call BEFORE starting work that may have history: " +
        "editing an unfamiliar file, debugging a recurring issue, making a non-obvious decision, or when " +
        "asked what's known about something. scope picks how wide to read: 'project' (just this project), " +
        "'full' (default: project plus inherited ancestor/personal/link context), or 'everywhere' (full " +
        "plus nested sub-projects). Each result's namespace/from fields are provenance, not a choice — an " +
        "absent 'from' means this project's own memory, otherwise it names the ancestor or personal " +
        "namespace the memory came from; read them to learn where knowledge lives, never construct a " +
        "namespace path. Empty results mean nothing is known — proceed from first principles, never invent " +
        "a remembered fact. A degraded:\"keyword_only\" field in the result means semantic search was " +
        "unavailable and results came from keyword matching alone — treat as incomplete, not exhaustive.",
      parameters: Type.Object({
        query: Type.String({ description: "What to search for" }),
        limit: Type.Optional(Type.Number({ description: "Max results (default 3)" })),
        tags: Tags,
        metadata: Metadata,
        scope: Scope,
      }),
      async execute(_id: any, params: any) {
        const live = await sessionLive(ctx);
        const ns = nsForCtx(live, toolCtx);
        const body: any = { query: params.query, limit: params.limit || live.recall_limit || 3 };
        if (params.tags?.length) body.tags = params.tags;
        if (params.metadata && Object.keys(params.metadata).length) body.metadata = params.metadata;
        // An unrecognized scope is dropped rather than forwarded: /v1/search
        // 400s on one, and a hallucinated value must not turn a recall into an
        // error.
        if (VALID_SCOPES.includes(params.scope)) body.scope = params.scope;
        const res = await client.postJson("/v1/search", body, ns);
        const results = (res?.results || []).map((r: any) => ({
          id: r?.memory?.id || "",
          content: r?.memory?.content || "",
          summary: r?.memory?.summary || "",
          tier: r?.memory?.tier || "",
          score: typeof r?.score === "number" ? r.score : 0,
          ...provenance(r?.memory, r?.from),
        }));
        // /v1/search already carries degraded/note on `res`; pass them through
        // rather than dropping them silently.
        return text(res?.degraded ? { results, degraded: res.degraded, note: res.note } : { results });
      },
    },
    {
      name: "memory_briefing",
      description:
        "Layered session-start briefing for this project from long-term memory (memini) — pinned context, " +
        "durable facts, how-to procedures, and recent activity — in one query-less call. Call it when a " +
        "session opens to orient yourself; prefer it over broad recall queries at session start. The " +
        "scope_header line ('Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2)') spells " +
        "out the ancestor chain you inherit from — read it instead of guessing namespace paths, and name " +
        "one of those ancestors as memory_remember's visibility to share a fact up that chain. " +
        "scope='everywhere' also briefs nested sub-projects.",
      parameters: Type.Object({ scope: Scope }),
      async execute(_id: any, params: any) {
        const live = await sessionLive(ctx);
        const ns = nsForCtx(live, toolCtx);
        const res = await client.getJson(meminiBriefingPath(params), ns);
        if (!res) return text({ briefing: null, error: "memini unavailable" });
        const section = (items: any[]) =>
          (items || []).map((b: any) => ({
            id: b?.memory?.id || "",
            content: b?.memory?.content || "",
            tier: b?.memory?.tier || "",
            ...provenance(b?.memory, b?.from),
          }));
        return text({
          namespace: res.namespace || "",
          scope_header: res.scope_header || "",
          pinned: section(res.pinned),
          facts: section(res.facts),
          procedures: section(res.procedures),
          recent: section(res.recent),
        });
      },
    },
    {
      name: "memory_list",
      description:
        "Browse long-term memory (memini) without a query — filter by tier, tags, or metadata " +
        "category (e.g. all procedural memories or everything categorized bug_fixes). Newest first.",
      parameters: Type.Object({
        tiers: Type.Optional(
          Type.Array(Type.String(), { description: "Restrict to these tiers; empty means all." }),
        ),
        tags: Tags,
        metadata: Metadata,
        limit: Type.Optional(Type.Number({ description: "Max results (0 = all, default 20)" })),
      }),
      async execute(_id: any, params: any) {
        const live = await sessionLive(ctx);
        const ns = nsForCtx(live, toolCtx);
        const args = { ...params, limit: params.limit ?? 20 };
        const res = await client.getJson(meminiListPath(args), ns);
        const memories = (res?.memories || []).map((m: any) => ({
          id: m.id || "",
          content: m.content || "",
          summary: m.summary || "",
          tier: m.tier || "",
          tags: m.tags || [],
          metadata: m.metadata || {},
        }));
        return text({ memories });
      },
    },
    {
      name: "memory_remember",
      description:
        "Store a durable fact, decision, or preference in long-term memory (memini). Call proactively when " +
        "the user says 'remember this', after an architectural decision (capture the why), or after " +
        "discovering a non-obvious bug or convention. Keep memories atomic — one self-contained fact per " +
        "call. Don't store what's already in project docs or trivially recoverable from code. To correct " +
        "an existing memory, pass its id — the write updates it in place. visibility decides who should " +
        "know: 'project' (default) keeps it here; 'personal' follows the user everywhere; or name an " +
        "ancestor from the memory_briefing Scope line to share it up that chain. reinforced=true in the " +
        "result means the fact was ALREADY KNOWN: no new memory was created, the existing one was " +
        "strengthened, and `id` names that pre-existing memory rather than anything you just wrote — do " +
        "not report it to the user as a new save.",
      parameters: Type.Object({
        content: Type.String({ description: "The fact to remember — atomic and self-contained." }),
        id: Type.Optional(
          Type.String({
            description: "Existing memory id (from memory_recall / memory_list) to correct in place instead of writing a new memory.",
          }),
        ),
        tier: Type.Optional(
          Type.String({
            description:
              "semantic=durable knowledge, procedural=how-to, episodic=what happened, working=transient " +
              "(omit to let the server classify from the content)",
          }),
        ),
        tags: Type.Optional(
          Type.Array(Type.String(), {
            description: "Topic keywords for later search/filtering; tag a critical always-relevant fact 'pinned'.",
          }),
        ),
        category: Type.Optional(
          Type.String({
            description:
              "Optional topic bucket stored as metadata.category (e.g. bug_fixes, architecture_decisions) for browsing by subject later.",
          }),
        ),
        visibility: Type.Optional(
          Type.String({
            description:
              "Who should remember this: 'project' (default, this project only), 'personal' (about the user, " +
              "follows them everywhere), or an ancestor namespace name read off the memory_briefing Scope " +
              "line (e.g. the team or org level) to share it up that chain. On a durable write an " +
              "unrecognized name errors listing the valid options. Episodic/working writes always stay in " +
              "the project regardless.",
          }),
        ),
      }),
      async execute(_id: any, params: any) {
        const live = await sessionLive(ctx);
        const ns = nsForCtx(live, toolCtx);
        // No client-side tier default: an omitted (or invalid) tier lets the
        // server classify the content and apply its own default.
        const body: any = { content: params.content };
        if (params.id) body.id = params.id; // POST /v1/memories upserts by id
        if (params.tier && VALID_TIERS.includes(params.tier)) body.tier = params.tier;
        if (params.tags?.length) body.tags = params.tags;
        if (params.category) body.metadata = { category: params.category };
        // visibility is NOT validated client-side beyond trimming: 'project' and
        // 'personal' are fixed, but any other value names an ancestor of THIS
        // namespace, which only the server can resolve — and its error enumerates
        // the valid chain, which is how the model learns the topology. Swallowing
        // an unknown name here would silently write to the wrong place instead.
        const visibility = String(params.visibility || "").trim();
        if (visibility) body.visibility = visibility;
        const res = await client.postJsonResult("/v1/memories", body, ns);
        if (!res.ok) return text({ id: null, success: false, error: res.error });
        const out: any = { id: res.data?.id || null, success: true };
        // reinforced: the fact was already known, nothing new was written, and id
        // names the pre-existing memory. Dropping the flag here would let the
        // model report a no-op as a fresh save.
        if (res.data?.reinforced) out.reinforced = true;
        return text(out);
      },
    },
    {
      name: "memory_forget",
      description:
        "Permanently delete a memory from long-term memory (memini) by its id — use when a recalled " +
        "memory is wrong, outdated, or poisoned. Get the id from memory_recall or memory_list. To " +
        "correct a fact instead, call memory_remember with the existing id (it updates in place, " +
        "preserving history); forget only memories that should not exist at all.",
      parameters: Type.Object({
        id: Type.String({ description: "The id of the memory to forget (from memory_recall / memory_list)." }),
      }),
      async execute(_id: any, params: any) {
        const live = await sessionLive(ctx);
        const ns = nsForCtx(live, toolCtx);
        if (!params.id) return text({ forgotten: false, error: "id is required" });
        const res = await client.deleteJson(`/v1/memories/${encodeURIComponent(params.id)}`, ns);
        return text({ forgotten: res != null });
      },
    },
  ];

  // names is required for a factory: the host matches the factory's tools by the
  // declared names (candidates with names.length > 0), and re-invokes the factory
  // with the live per-agent ctx at execution time.
  api.registerTool((toolCtx: any) => buildTools(toolCtx), { optional: true, names: TOOL_NAMES });
}

// Explicit return-type annotation on the default export — the SDK's chunked
// re-exports make the inferred type reference an internal module path that
// breaks when downstream `import` resolves it.
const plugin: {
  id: string;
  name: string;
  description: string;
  kind?: string | string[];
  configSchema: any;
  register: (api: any) => void;
} = definePluginEntry({
  id: "memini",
  name: "memini",
  description: "Shared cross-session memory via a memini service.",
  kind: "memory",
  configSchema,
  register(api: any) {
    const sessionCtx = createSessionContext(api.pluginConfig, process.env, harnessCwd());
    const client = createClient(sessionCtx.cfg, api);

    // Per-agent isolation is the default as of memini 0.0.11 (it was off before).
    // Warn installs that never set it: their pre-0.0.11 memory lives in the shared
    // base namespace and will not be recalled under the new per-agent namespaces
    // until migrated (`memini namespace split --from <base>`), or isolation is
    // turned off again with namespace_per_agent:false.
    if (api.pluginConfig?.namespace_per_agent === undefined && sessionCtx.cfg.namespace_per_agent) {
      console.error(
        `[memini] per-agent namespaces are now on by default (template "${sessionCtx.cfg.namespace_template}"); ` +
          `existing memory under "${sessionCtx.cfg.namespace}" needs \`memini namespace split --from ${sessionCtx.cfg.namespace}\` ` +
          `to migrate, or set namespace_per_agent:false to keep the shared pool.`,
      );
    }

    if (typeof api.registerMemoryCapability === "function") {
      api.registerMemoryCapability({
        promptBuilder: () => [
          sessionCtx.cfg.namespace_per_agent
            ? `Long-term memory: memini at ${client.baseUrl}, per-agent namespace from "${sessionCtx.cfg.namespace_template}".`
            : `Long-term memory: memini at ${client.baseUrl}, namespace "${sessionCtx.cfg.namespace}".`,
          "Relevant memories are recalled before each turn and turns are captured after. Treat recalled context as background; prefer current workspace state and user instructions.",
        ],
      });
    }

    // OpenClaw fires before_prompt_build on every step of a turn (each tool
    // call), and an unchanged query recalls the same top memories — so the same
    // "long-term memory" block is re-injected on every step, drowning the prompt
    // (eleboucher/memini#21). Track what each session has already been shown and
    // drop repeats on later steps; genuinely new matches still surface. Bounded
    // so the map can't grow without limit across long-lived gateways.
    const injectedBySession = new Map<string, Set<string>>();
    const MAX_TRACKED_SESSIONS = 200;
    const rememberInjected = (session: string, ids: string[]) => {
      let seen = injectedBySession.get(session);
      if (!seen) {
        seen = new Set<string>();
        injectedBySession.set(session, seen);
        while (injectedBySession.size > MAX_TRACKED_SESSIONS) {
          const oldest = injectedBySession.keys().next().value;
          if (oldest === undefined) break;
          injectedBySession.delete(oldest);
        }
      }
      for (const id of ids) if (id) seen.add(id);
    };

    // Echo guard: track IDs this namespace just captured so the next recall can
    // drop them. Keyed by namespace (agent identity) not session, because the
    // session id can be absent/rolled at recall while the namespace is stable.
    // Bounded: oldest captures age out and become recallable again.
    const recentlyCaptured = new Map<string, Set<string>>();
    const MAX_CAPTURED_PER_NS = 5;
    const MAX_CAPTURED_NAMESPACES = 200;
    const rememberCaptured = (ns: string, id: string) => {
      let seen = recentlyCaptured.get(ns);
      if (!seen) {
        seen = new Set<string>();
        recentlyCaptured.set(ns, seen);
        while (recentlyCaptured.size > MAX_CAPTURED_NAMESPACES) {
          const oldest = recentlyCaptured.keys().next().value;
          if (oldest === undefined) break;
          recentlyCaptured.delete(oldest);
        }
      }
      seen.add(id);
      while (seen.size > MAX_CAPTURED_PER_NS) {
        const oldest = seen.values().next().value;
        if (oldest === undefined) break;
        seen.delete(oldest);
      }
    };

    const recallHandler = async (event: any, hookCtx: any) => {
      const live = await sessionLive(sessionCtx);
      if (!live.enabled) return;
      const prompt = typeof event?.prompt === "string" ? event.prompt.trim() : "";
      if (!prompt) return;
      if (shouldSkipSystemTurn(live, hookCtx)) return;
      const ns = effectiveNamespace(live, hookCtx);
      if (ns == null) return;
      const body: any = { query: prompt, limit: live.recall_limit };
      // Exclude this session's own just-captured turns: they're still in the
      // live transcript, so recalling them echoes the conversation back as
      // "long-term memory". agent_end tags each capture with session_id.
      const session = sessionIdentity(hookCtx);
      if (session) body.exclude_metadata = { session_id: session };
      // The server-side temporal echo guard is on by default (5 min window),
      // backstopping the client-side message-ID guard (lost on gateway restart)
      // and the session-id exclusion (misses when session id is absent/rolled).
      const result = await client.postJson("/v1/search", body, ns);
      let results = Array.isArray(result?.results) ? result.results : [];
      // Drop just-captured turns: they're live context, not long-term memory.
      // Survives session-id asymmetry (keyed by stable namespace).
      const captured = recentlyCaptured.get(ns);
      if (captured?.size) results = results.filter((r: any) => !captured.has(r?.memory?.id));
      // Suppress memories this session has already been shown so a multi-step
      // turn doesn't re-inject the same block on every tool call (#21).
      if (session) {
        const seen = injectedBySession.get(session);
        if (seen?.size) results = results.filter((r: any) => !seen.has(r?.memory?.id));
      }
      const bullets = formatResults(results, recallLabels());
      if (bullets.length === 0) return;
      // Apply the token ceiling to the rendered bullets; recall_max_tokens <= 0
      // (the default) leaves the list unchanged, so existing installs are
      // unaffected. The tail (lowest-relevance) is dropped first.
      const fit = fitByTokens(bullets, live.recall_max_tokens);
      if (fit.items.length === 0) return;
      if (session) {
        rememberInjected(session, results.map((r: any) => r?.memory?.id).filter(Boolean));
      }
      const lines = [`Relevant long-term memory from memini:`, ...fit.items];
      // /v1/search sets `degraded: "keyword_only"` (plus a `note`) when the
      // query embed was unavailable and it fell back to keyword-only matching;
      // both are already on `result`, so surfacing them is a one-line addition.
      if (result?.degraded) {
        lines.push(
          `[memini: ${result.note || "semantic search unavailable — results are keyword-only and may be incomplete"}]`,
        );
      }
      if (fit.dropped > 0) lines.push(`[... ${fit.dropped} item(s) truncated by token budget]`);
      return { prependContext: lines.join("\n") };
    };
    // api.on is OpenClaw's typed hook surface and the only one that can register
    // before_prompt_build/agent_end (the coarse api.registerHook is for internal
    // events like message:sent). Warn rather than silently drop memory if a build
    // somehow lacks it (eleboucher/memini#26).
    const addHook = (name: string, handler: any) => {
      if (typeof api.on === "function") api.on(name, handler);
      else api.logger.warn?.(`memini: api.on unavailable; ${name} hook not registered`);
    };
    addHook("before_prompt_build", recallHandler);

    const captureHandler = async (event: any, hookCtx: any) => {
      const live = await sessionLive(sessionCtx);
      if (!live.enabled || !Array.isArray(event.messages)) return;
      const userText = lastTextByRole(event.messages, "user");
      const assistantText = lastTextByRole(event.messages, "assistant");
      if (!userText || !assistantText) return;
      if (shouldSkipSystemTurn(live, hookCtx)) return;
      // Drop OpenClaw runtime plumbing from the captured turn: untrusted-metadata
      // preambles, and subagent task delegations (framing, not conversation).
      const captureUser = stripRuntimePreambles(userText);
      if (!captureUser || startsWithNoisePrefix(captureUser)) return;
      if (captureUser.length < live.min_capture_chars) return;
      const ns = effectiveNamespace(live, hookCtx);
      if (ns == null) return;
      const session = sessionIdentity(hookCtx);
      // A capture without a session_id can never be excluded by the pre-turn
      // recall guard (the server's exclude_metadata is exact key=value), so it
      // would echo this session's own turns back as "long-term memory" forever.
      // No identity → no capture.
      if (!session) return;
      const metadata: any = { source: "openclaw", format: "turn", session_id: session };
      if (!event?.success) metadata.failed = true;
      const writeResult = await client.postJson("/v1/memories", {
        content: `${captureUser.slice(0, 1000)}\n\n${assistantText.slice(0, 3000)}`,
        tags: ["openclaw"],
        metadata,
      }, ns);
      // Record the captured ID so the next recall can drop it.
      if (writeResult?.id) rememberCaptured(ns, String(writeResult.id));
    };
    addHook("agent_end", captureHandler);

    // Opt-in explicit tools, registered after the memory slot above. Best-effort:
    // a failure (e.g. typebox unavailable) is logged and leaves the slot working.
    if (sessionCtx.cfg.expose_tools && typeof api.registerTool === "function") {
      try {
        registerMeminiTools(api, client, sessionCtx);
      } catch (e) {
        api.logger?.warn?.(`memini: tool registration skipped: ${String(e)}`);
      }
    }

    // /memini:status and /memini:namespace. Best-effort for the same reason: a
    // host build without registerCommand must not cost the plugin its memory slot.
    if (typeof api.registerCommand === "function") {
      try {
        registerMeminiCommands(api, sessionCtx);
      } catch (e) {
        api.logger?.warn?.(`memini: command registration skipped: ${String(e)}`);
      }
    }
  },
});

export default plugin;
