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
 * NOTE: agent_end is a raw-conversation hook. Non-bundled plugins only receive
 * event.messages on it when the operator sets
 * `plugins.entries.memini.hooks.allowConversationAccess: true` in openclaw.json
 * — without it, capture silently no-ops. See README "Install".
 */

import { Type } from "typebox";
import { buildJsonPluginConfigSchema, definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { applyTemplate, DEFAULT_TEMPLATE as RESOLVER_DEFAULT_TEMPLATE } from "@memini/namespace-resolver";
import {
  clearOverride,
  defaultOverridesPath,
  describeSettings,
  normalizeNamespace,
  overrideKey,
  readOverride,
  redactValue,
  validateNamespace,
  writeOverride,
  type NamespaceSource,
  type ResolveOpts,
} from "@memini/client";

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 5000;
const DEFAULT_NAMESPACE = "openclaw";
const VALID_TIERS = ["working", "episodic", "semantic", "procedural"];
// The LLM-facing semantic scope vocabulary, identical to the MCP server's
// (internal/api/mcp: scopeEnum). The deprecated REST aliases "exact"/"subtree"
// are deliberately NOT offered: the model makes a semantic choice, it does not
// speak the back-compat dialect.
const VALID_SCOPES = ["project", "full", "everywhere"];
// The status probes are diagnostics: fail fast rather than hang a slash command.
const STATUS_TIMEOUT_MS = 4000;
const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

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

// harnessCwd is the directory a namespace override is keyed to. OpenClaw is a
// gateway: its cwd is the daemon's, not a project's, so this is best-effort. When
// the gateway happens to run inside a repo, `memini:namespace` can pin that repo;
// when it does not, no override key ever matches and the chain simply falls
// through to the env/config/default legs below.
function harnessCwd(): string | undefined {
  try {
    return process.cwd();
  } catch {
    return undefined;
  }
}

/**
 * resolveBaseNamespace resolves the plugin's BASE namespace, with provenance:
 *
 *   1. per-project override (when a cwd is available)
 *   2. MEMINI_NAMESPACE
 *   3. the explicit `namespace` config value
 *   4. the "openclaw" default
 *
 * The override beats the env var deliberately: a globally exported
 * MEMINI_NAMESPACE (a shell rc, or a fish universal variable) pins every project
 * on the machine to one namespace, and if the env won, `memini:namespace` would
 * silently do nothing on exactly the machines that most need it.
 *
 * There is deliberately still NO git/cwd derivation here, and the default stays
 * the literal "openclaw": a gateway's cwd is usually meaningless, and deriving a
 * namespace from it would silently relocate every existing install's memory.
 * namespace_prefix / namespace_template / per-agent nesting all apply on top of
 * whatever this returns, exactly as before.
 *
 * `opts.ignoreOverride` lets describeSettings ask the counterfactuals ("what
 * would this be without the override?") — the override lives in a file, so no
 * amount of env-doctoring would strip it. Exported for testing.
 */
export function resolveBaseNamespace(
  pluginConfig: any,
  env: Record<string, string | undefined> = process.env,
  cwd: string | undefined = harnessCwd(),
  opts: ResolveOpts = {},
): { namespace: string; source: NamespaceSource } {
  const c = pluginConfig || {};

  if (!opts.ignoreOverride && cwd) {
    const override = readOverride(cwd, { env });
    if (override) return { namespace: override.namespace, source: "override" };
  }

  const nsEnv = (env["MEMINI_NAMESPACE"] || "").trim();
  // Raw-trimmed: the server validates the header, and an explicit hierarchical
  // value (team/eu) must pass through untouched.
  if (nsEnv) return { namespace: nsEnv, source: "env" };

  const configured = typeof c.namespace === "string" ? c.namespace.trim() : "";
  if (configured) return { namespace: configured, source: "config" };

  return { namespace: DEFAULT_NAMESPACE, source: "default" };
}

// resolveConfig normalizes raw plugin config into the defaults the plugin runs
// with. `env`/`cwd` are injectable for tests; everything but the namespace chain
// reads process.env directly, as it always has.
export function resolveConfig(
  pluginConfig: any,
  env: Record<string, string | undefined> = process.env,
  cwd: string | undefined = harnessCwd(),
) {
  const c = pluginConfig || {};
  return {
    enabled: c.enabled !== false,
    // Config wins; otherwise the canonical MEMINI_BASE_URL (alias MEMINI_URL),
    // matching the other integrations so one env setup works everywhere.
    base_url: c.base_url || strEnv("MEMINI_BASE_URL") || strEnv("MEMINI_URL") || DEFAULT_BASE_URL,
    // override > MEMINI_NAMESPACE > config > "openclaw" (see resolveBaseNamespace).
    namespace: resolveBaseNamespace(c, env, cwd).namespace,
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
    // Per-call recall knobs. 0 / unset falls back to the defaults below.
    //
    // recall_limit defaults to 3 (was 5): the count cap is the lever that bounds
    // per-turn injection. There is deliberately NO relevance-score floor knob —
    // benchmarking (bench -vec-gate) showed neither a fused-score nor a raw
    // vector-score threshold can decide "inject nothing when nothing is relevant"
    // with the default MiniLM embedder: the fused score is min-max-normalised
    // within the pool (its best always ~1.0), and MiniLM's absolute scores for
    // relevant vs irrelevant queries overlap, so any floor that suppresses noise
    // also guts real recall. Bound volume with the count cap (and the optional
    // token cap below) instead.
    recall_limit: Number.isFinite(c.recall_limit) && c.recall_limit > 0 ? c.recall_limit : 3,
    // Hard ceiling on the rendered recall-block tokens. Defaults to 0 (uncapped)
    // to match the other integrations and the Claude Code plugin: with
    // recall_limit=3 the count is the bound, so a token cap is an optional extra.
    // Config wins; otherwise MEMINI_INJECT_RECALL_MAX_TOK (same knob name the
    // opencode and Claude Code plugins use). Set it > 0 to cap a raised limit.
    recall_max_tokens:
      Number.isFinite(c.recall_max_tokens) && c.recall_max_tokens > 0
        ? c.recall_max_tokens
        : intEnv("MEMINI_INJECT_RECALL_MAX_TOK", 0),
    // Minimum length (chars) of the stripped user turn required to capture it.
    // Off (0) by default — capture is additive and dropping turns is lossy, so
    // it stays opt-in (see the defaults policy). Set it (e.g. 30) when a gateway
    // still emits short residual-noise turns after preamble stripping. Config
    // wins; otherwise MEMINI_MIN_CAPTURE_CHARS.
    min_capture_chars:
      Number.isFinite(c.min_capture_chars) && c.min_capture_chars > 0
        ? c.min_capture_chars
        : intEnv("MEMINI_MIN_CAPTURE_CHARS", 0),
    namespace_prefix: c.namespace_prefix || "",
    // home: the caller's personal namespace, sent as X-Memini-Home. Config
    // wins over MEMINI_HOME env (same precedence style as base_url); no
    // git/cwd derivation — unset means "no home leg", not a guess.
    home: c.home || strEnv("MEMINI_HOME") || undefined,
  };
}

// strEnv returns a trimmed string env var, or "" when unset/blank.
function strEnv(name: string) {
  const raw = process.env[name];
  return raw == null ? "" : raw.trim();
}

// intEnv parses a non-negative integer env var, falling back to `def` when
// unset or malformed (env values are operator input and must never throw).
function intEnv(name: string, def: number) {
  const raw = process.env[name];
  if (raw == null || raw === "") return def;
  const n = Number.parseInt(raw, 10);
  return Number.isFinite(n) && n >= 0 ? n : def;
}

export type ResolvedConfig = ReturnType<typeof resolveConfig>;

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

function normalizedHostname(hostname: any) {
  return hostname.replace(/^\[|\]$/g, "").toLowerCase();
}

function usesPlaintextBearerAuth(baseUrl: any, secret: any) {
  if (!secret) return false;
  try {
    const parsed = new URL(baseUrl);
    return parsed.protocol === "http:" && !LOOPBACK_HOSTS.has(normalizedHostname(parsed.hostname));
  } catch {
    return false;
  }
}

function plaintextBearerAuthMessage(baseUrl: any) {
  return `memini: MEMINI_API_KEY is configured for plaintext HTTP to ${baseUrl}. Bearer tokens and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
}

export function createPlaintextBearerAuthGuard(warn: any, env?: any) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl: any, secret: any) {
    if (!usesPlaintextBearerAuth(baseUrl, secret)) return;
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
  namespace: string;
}

function createClient(cfg: ResolvedConfig, api: any): MeminiClient {
  const baseUrl = String(cfg.base_url || DEFAULT_BASE_URL).replace(/\/+$/, "");
  const namespace = String(cfg.namespace || DEFAULT_NAMESPACE);
  const timeoutMs = Number(cfg.timeout_ms || DEFAULT_TIMEOUT_MS);
  const fallbackOnError = cfg.fallback_on_error !== false;
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
  const home = cfg.home;
  const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m: any) => api.logger.warn?.(m));

  async function postJson(path: string, payload: any, ns?: string) {
    if (process.env.MEMINI_REQUIRE_HTTPS === "1") guardPlaintextBearerAuth(baseUrl, secret);
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers: Record<string, string> = { "Content-Type": "application/json", "X-Memini-Namespace": ns || namespace };
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
    const headers: Record<string, string> = { "X-Memini-Namespace": ns || namespace };
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
    const headers: Record<string, string> = { "X-Memini-Namespace": ns || namespace };
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
        "X-Memini-Namespace": ns || namespace,
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

  return { postJson, getJson, deleteJson, postJsonResult, baseUrl, namespace };
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

// --- /memini:status + /memini:namespace --------------------------------------

// statusGet is the diagnostics-only GET. It bypasses the client's
// warn-and-return-null degrade path: a failed probe here is data for the report,
// not a fault to log. Silent on purpose — the caller decides what a miss means,
// and for /healthz a miss means "not exposed", not "server down".
async function statusGet(cfg: ResolvedConfig, namespace: string, path: string): Promise<any> {
  const baseUrl = String(cfg.base_url || DEFAULT_BASE_URL).replace(/\/+$/, "");
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
  const headers: Record<string, string> = { "X-Memini-Namespace": namespace };
  if (secret) headers.Authorization = `Bearer ${secret}`;
  if (cfg.home) headers["X-Memini-Home"] = cfg.home;
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
// Reachability is decided by /v1/namespaces/read-set, not /healthz: a remote
// memini typically sits behind an ingress that routes only /v1 and /mcp, so
// /healthz 404s while the server is perfectly healthy, and calling that "server
// down" would be a false alarm on the most common deployment. The read set
// doubles as the probe — it is the server's own introspection of where a plain
// recall looks, so it cannot drift from what recall really does.
export async function fetchServer(cfg: ResolvedConfig, namespace: string): Promise<ServerReport> {
  const started = Date.now();
  const readSet = await statusGet(cfg, namespace, "/v1/namespaces/read-set");
  const out: ServerReport = { reachable: readSet != null, latencyMs: Date.now() - started, readSet };
  // Dependency detail, when the deployment exposes it. A miss is "not routed",
  // not "broken" — so it never touches `reachable`.
  const health = await statusGet(cfg, namespace, "/healthz?verbose=1");
  if (health) {
    out.version = health.version;
    out.deps = health.deps;
  } else {
    out.healthExposed = false;
  }
  return out;
}

const pad = (s: any, n: number) => String(s).padEnd(n);

// renderStatus formats the report. `effective` is the namespace this surface
// would actually send (base + prefix + per-agent template); the NAMESPACE block
// reports the BASE chain, because that is the only leg an override or the env
// participates in. Exported for testing — the assertion that matters (a bearer
// token is never printed in full) is on this string.
export function renderStatus(settings: any, cfg: ResolvedConfig, effective: string, server: ServerReport): string {
  const ns = settings.namespace;
  const L: string[] = [];

  L.push(`memini — effective settings (openclaw)`);
  L.push("");

  L.push(`NAMESPACE`);
  L.push(`  ${pad("this surface sends", 28)} ${effective}`);
  L.push(`  ${pad("base", 28)} ${pad(ns.effective, 34)} <- ${ns.source}`);
  if (ns.override) {
    L.push(
      `  ${pad("without the override", 28)} ${pad(ns.withoutOverride.namespace, 34)} <- ${ns.withoutOverride.source}`,
    );
  }
  if (ns.derived.namespace !== ns.effective) {
    L.push(`  ${pad("without the env pin", 28)} ${pad(ns.derived.namespace, 34)} <- ${ns.derived.source}`);
  }
  L.push(`  ${pad("per-agent", 28)} ${cfg.namespace_per_agent ? cfg.namespace_template : "off"}`);
  if (cfg.namespace_prefix) L.push(`  ${pad("prefix", 28)} ${cfg.namespace_prefix}`);
  L.push(`  ${pad("home (personal)", 28)} ${ns.home || "(unset)"}`);
  L.push("");

  // Connection + namespace inputs from the shared knob table (already redacted).
  // The capture/injection knobs it also carries belong to the Claude Code hooks;
  // listing them here would imply this plugin honors them, and it does not.
  const groups: [string, string[]][] = [
    ["CONNECTION", ["MEMINI_BASE_URL", "MEMINI_API_KEY", "MEMINI_REQUIRE_HTTPS"]],
    ["NAMESPACE INPUTS", ["MEMINI_NAMESPACE", "MEMINI_HOME"]],
  ];
  for (const [group, names] of groups) {
    const rows = (settings.settings || []).filter((s: any) => names.includes(s.name));
    if (!rows.length) continue;
    L.push(group);
    for (const r of rows) {
      L.push(
        `  ${pad(r.name.replace(/^MEMINI_/, "").toLowerCase(), 28)} ${pad(r.value, 34)} ${r.source === "env" ? "<- env" : "(default)"}`,
      );
    }
    L.push("");
  }

  // The plugin's own config, which the shared table knows nothing about. The
  // bearer is the one the requests actually carry — redacted, since a settings
  // dump is the likeliest place a token gets pasted into an issue.
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN || "";
  L.push(`PLUGIN`);
  L.push(`  ${pad("base_url", 28)} ${cfg.base_url}`);
  L.push(`  ${pad("bearer", 28)} ${secret ? redactValue(secret) : "(none)"}`);
  L.push(`  ${pad("recall_limit", 28)} ${cfg.recall_limit}`);
  L.push(`  ${pad("expose_tools", 28)} ${cfg.expose_tools ? "on" : "off"}`);
  L.push(`  ${pad("skip_system_turns", 28)} ${cfg.skip_system_turns ? "on" : "off"}`);
  L.push("");

  L.push(`SERVER`);
  if (!server.reachable) {
    L.push(`  ${pad("reachable", 28)} NO — could not reach ${cfg.base_url}`);
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

  L.push(`PATHS`);
  L.push(`  ${pad("overrides", 28)} ${settings.paths.overrides}`);
  L.push("");

  if (settings.warnings.length) {
    L.push(`WARNINGS`);
    for (const w of settings.warnings) {
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
 * `cfg` is mutated in place on a namespace change rather than re-created: every
 * hook and tool resolves its namespace from cfg on each call, so an override
 * applies to the very next turn instead of waiting for a gateway restart. A
 * command that appears to succeed and then does nothing until you restart is
 * worse than no command at all.
 *
 * Exported for testing.
 */
export function registerMeminiCommands(api: any, cfg: ResolvedConfig, pluginConfig: any) {
  const cwd = harnessCwd();

  // The base chain, as this plugin resolves it. describeSettings calls it with
  // doctored environments (and ignoreOverride) to produce the counterfactual
  // lines, which is what turns a dump into a diagnosis.
  const resolve = (env: Record<string, string | undefined>, opts?: ResolveOpts) =>
    resolveBaseNamespace(pluginConfig, env, cwd, opts || {});

  const describe = () =>
    describeSettings({
      cwd: cwd || "",
      env: process.env as Record<string, string | undefined>,
      resolve,
    });

  api.registerCommand({
    name: "memini:status",
    description: "Show memini's effective settings: namespace + provenance, connection, server read set",
    acceptsArgs: false,
    async handler(ctx: any) {
      try {
        const settings = describe();
        // What THIS surface would send: the base, plus prefix and the per-agent
        // template. PluginCommandContext carries the sessionKey, so an
        // agent-keyed conversation resolves its own namespace here.
        const effective = effectiveNamespace(cfg, ctx) ?? cfg.namespace;
        const server = await fetchServer(cfg, effective);
        return { text: renderStatus(settings, cfg, effective, server) };
      } catch (error) {
        // A command must never throw into the host.
        return { text: `memini: status failed: ${String(error)}` };
      }
    },
  });

  api.registerCommand({
    name: "memini:namespace",
    description: "Show, set, or --clear the memini namespace override for this project",
    acceptsArgs: true,
    async handler(ctx: any) {
      try {
        if (!cwd) {
          return {
            text: "memini: no working directory is available, so a per-project override cannot be keyed. Set the `namespace` config value or MEMINI_NAMESPACE instead.",
          };
        }
        const arg = String(ctx?.args || "").trim();
        const before = resolveBaseNamespace(pluginConfig, process.env, cwd);

        if (!arg) {
          const current = readOverride(cwd, { env: process.env });
          const out = [
            `namespace: ${before.namespace}  (source: ${before.source})`,
            `project:   ${overrideKey(cwd)}`,
            ``,
          ];
          if (current) {
            out.push(`An override is active (set ${current.setAt}).`);
            out.push(`Clear it with:  /memini:namespace --clear`);
          } else {
            out.push(`No override — using ${before.source === "default" ? "the default" : `the ${before.source} value`}.`);
            out.push(`Set one with:  /memini:namespace <namespace>`);
          }
          out.push(`Overrides file: ${defaultOverridesPath(process.env)}`);
          return { text: out.join("\n") };
        }

        if (arg === "--clear" || arg === "clear") {
          if (!clearOverride(cwd, { env: process.env })) {
            return { text: `No override was set for ${overrideKey(cwd)} — nothing to clear.` };
          }
          const after = resolveBaseNamespace(pluginConfig, process.env, cwd);
          cfg.namespace = after.namespace;
          return {
            text: [
              `namespace override cleared: ${before.namespace} -> ${after.namespace}  (source: ${after.source})`,
              ``,
              `Recall and capture use the new namespace from the next turn.`,
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
        writeOverride(cwd, ns, { env: process.env });
        const after = resolveBaseNamespace(pluginConfig, process.env, cwd);
        cfg.namespace = after.namespace;
        return {
          text: [
            `namespace override set: ${before.namespace} -> ${after.namespace}`,
            `project: ${overrideKey(cwd)}`,
            ``,
            `The override wins over MEMINI_NAMESPACE. Recall and capture use it from the next turn` +
              (cfg.namespace_per_agent ? `, with the per-agent template "${cfg.namespace_template}" on top.` : `.`),
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
export function registerMeminiTools(api: any, client: MeminiClient, cfg: ResolvedConfig) {
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
  const nsForCtx = (ctx: any) => {
    const ns = effectiveNamespace(cfg, ctx) ?? cfg.namespace;
    if (cfg.namespace_per_agent && ns === cfg.namespace && !warnedMissingAgent) {
      warnedMissingAgent = true;
      api.logger?.warn?.(
        `memini: tool call could not resolve an agent; querying base namespace "${cfg.namespace}". ` +
          `Per-agent memory lives under "${cfg.namespace_template}" and will not appear here.`,
      );
    }
    return ns;
  };

  const buildTools = (ns: string) => [
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
        const body: any = { query: params.query, limit: params.limit || 3 };
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
        if (!params.id) return text({ forgotten: false, error: "id is required" });
        const res = await client.deleteJson(`/v1/memories/${encodeURIComponent(params.id)}`, ns);
        return text({ forgotten: res != null });
      },
    },
  ];

  // names is required for a factory: the host matches the factory's tools by the
  // declared names (candidates with names.length > 0), and re-invokes the factory
  // with the live per-agent ctx at execution time.
  api.registerTool((ctx: any) => buildTools(nsForCtx(ctx)), { optional: true, names: TOOL_NAMES });
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
    const cfg = resolveConfig(api.pluginConfig);
    const client = createClient(cfg, api);

    // Per-agent isolation is the default as of memini 0.0.11 (it was off before).
    // Warn installs that never set it: their pre-0.0.11 memory lives in the shared
    // base namespace and will not be recalled under the new per-agent namespaces
    // until migrated (`memini namespace split --from <base>`), or isolation is
    // turned off again with namespace_per_agent:false.
    if (api.pluginConfig?.namespace_per_agent === undefined && cfg.namespace_per_agent) {
      console.error(
        `[memini] per-agent namespaces are now on by default (template "${cfg.namespace_template}"); ` +
          `existing memory under "${cfg.namespace}" needs \`memini namespace split --from ${cfg.namespace}\` ` +
          `to migrate, or set namespace_per_agent:false to keep the shared pool.`,
      );
    }

    if (typeof api.registerMemoryCapability === "function") {
      api.registerMemoryCapability({
        promptBuilder: () => [
          cfg.namespace_per_agent
            ? `Long-term memory: memini at ${client.baseUrl}, per-agent namespace from "${cfg.namespace_template}".`
            : `Long-term memory: memini at ${client.baseUrl}, namespace "${client.namespace}".`,
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

    const recallHandler = async (event: any, ctx: any) => {
      if (!cfg.enabled) return;
      const prompt = typeof event?.prompt === "string" ? event.prompt.trim() : "";
      if (!prompt) return;
      if (shouldSkipSystemTurn(cfg, ctx)) return;
      const ns = effectiveNamespace(cfg, ctx);
      if (ns == null) return;
      const body: any = { query: prompt, limit: cfg.recall_limit };
      // Exclude this session's own just-captured turns: they're still in the
      // live transcript, so recalling them echoes the conversation back as
      // "long-term memory". agent_end tags each capture with session_id.
      const session = sessionIdentity(ctx);
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
      const fit = fitByTokens(bullets, cfg.recall_max_tokens);
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

    const captureHandler = async (event: any, ctx: any) => {
      if (!cfg.enabled || !Array.isArray(event.messages)) return;
      const userText = lastTextByRole(event.messages, "user");
      const assistantText = lastTextByRole(event.messages, "assistant");
      if (!userText || !assistantText) return;
      if (shouldSkipSystemTurn(cfg, ctx)) return;
      // Drop OpenClaw runtime plumbing from the captured turn: untrusted-metadata
      // preambles, and subagent task delegations (framing, not conversation).
      const captureUser = stripRuntimePreambles(userText);
      if (!captureUser || startsWithNoisePrefix(captureUser)) return;
      if (captureUser.length < cfg.min_capture_chars) return;
      const ns = effectiveNamespace(cfg, ctx);
      if (ns == null) return;
      const session = sessionIdentity(ctx);
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
    if (cfg.expose_tools && typeof api.registerTool === "function") {
      try {
        registerMeminiTools(api, client, cfg);
      } catch (e) {
        api.logger?.warn?.(`memini: tool registration skipped: ${String(e)}`);
      }
    }

    // /memini:status and /memini:namespace. Best-effort for the same reason: a
    // host build without registerCommand must not cost the plugin its memory slot.
    if (typeof api.registerCommand === "function") {
      try {
        registerMeminiCommands(api, cfg, api.pluginConfig);
      } catch (e) {
        api.logger?.warn?.(`memini: command registration skipped: ${String(e)}`);
      }
    }
  },
});

export default plugin;
