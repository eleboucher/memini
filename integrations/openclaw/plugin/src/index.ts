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

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 5000;
const DEFAULT_NAMESPACE = "openclaw";
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

// resolveConfig normalizes raw plugin config into the defaults the plugin runs with.
export function resolveConfig(pluginConfig: any) {
  const c = pluginConfig || {};
  return {
    enabled: c.enabled !== false,
    // Config wins; otherwise the canonical MEMINI_BASE_URL (alias MEMINI_URL),
    // matching the other integrations so one env setup works everywhere.
    base_url: c.base_url || strEnv("MEMINI_BASE_URL") || strEnv("MEMINI_URL") || DEFAULT_BASE_URL,
    namespace: c.namespace || DEFAULT_NAMESPACE,
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
    // Off by default: the slot already recalls/captures automatically; tools are
    // opt-in for agents that want to read/browse/write on demand.
    expose_tools: c.expose_tools === true,
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
// name comes from namespace_template (default "{agent}"), with {agent} and
// {namespace} substituted — e.g. "{agent}" -> "alice", "openclaw-{agent}" ->
// "openclaw-alice". Falls back to the base namespace when no agent id is present,
// preserving the shared-memory behavior — unless skip_without_agent is set, in
// which case it returns null so the caller skips the operation entirely (no
// recall, no write, no fallback namespace). Useful for gateways where
// unattributable sessions (cron, heartbeat) should not pollute memory.
export function effectiveNamespace(cfg: ResolvedConfig, ctx: any) {
  if (!cfg.namespace_per_agent) return cfg.namespace;
  const id = sanitizeNsSegment(agentIdentity(ctx));
  if (!id) return cfg.skip_without_agent ? null : cfg.namespace;
  const tmpl = cfg.namespace_template || DEFAULT_NAMESPACE_TEMPLATE;
  return tmpl.replaceAll("{agent}", id).replaceAll("{namespace}", cfg.namespace);
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
// no session (recall/capture then behave as before).
export function sessionIdentity(ctx: any) {
  for (const c of [ctx?.sessionId, ctx?.sessionKey, ctx?.runId]) {
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
// one or more "<Label> (untrusted metadata):" blocks (chat/sender info), each
// followed by a fenced JSON object. That metadata is explicitly untrusted and
// is not memory — captured verbatim it dominates a namespace and recalls at
// high similarity (it's templated), crowding out real memories. Strip the
// leading metadata blocks and keep the actual message that follows.
const UNTRUSTED_METADATA_BLOCK =
  /^\s*[^\n]*\(untrusted metadata\):\s*```(?:json)?\s*[\s\S]*?```\s*/;

export function stripRuntimePreambles(text: any) {
  if (typeof text !== "string") return text;
  let out = text;
  while (UNTRUSTED_METADATA_BLOCK.test(out)) {
    out = out.replace(UNTRUSTED_METADATA_BLOCK, "");
  }
  return out.trim();
}

// Mirrors the opencode plugin's labels toggle. Default `["tier"]` keeps the
// legacy "(tier) text" line so existing callers see identical output.
const DEFAULT_RECALL_LABELS = ["tier"];

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
  baseUrl: string;
  namespace: string;
}

function createClient(cfg: ResolvedConfig, api: any): MeminiClient {
  const baseUrl = String(cfg.base_url || DEFAULT_BASE_URL).replace(/\/+$/, "");
  const namespace = String(cfg.namespace || DEFAULT_NAMESPACE);
  const timeoutMs = Number(cfg.timeout_ms || DEFAULT_TIMEOUT_MS);
  const fallbackOnError = cfg.fallback_on_error !== false;
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
  const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m: any) => api.logger.warn?.(m));

  async function postJson(path: string, payload: any, ns?: string) {
    if (process.env.MEMINI_REQUIRE_HTTPS === "1") guardPlaintextBearerAuth(baseUrl, secret);
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers: Record<string, string> = { "Content-Type": "application/json", "X-Memini-Namespace": ns || namespace };
    if (secret) headers.Authorization = `Bearer ${secret}`;
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (!res.ok) {
        if (fallbackOnError) return null;
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
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "GET",
        headers,
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (!res.ok) {
        if (fallbackOnError) return null;
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

  return { postJson, getJson, baseUrl, namespace };
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

const TOOL_NAMES = ["memory_recall", "memory_list", "memory_remember"];

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
      description: "Search long-term memory (memini) for relevant past facts and context.",
      parameters: Type.Object({
        query: Type.String({ description: "What to search for" }),
        limit: Type.Optional(Type.Number({ description: "Max results (default 5)" })),
        tags: Tags,
        metadata: Metadata,
      }),
      async execute(_id: any, params: any) {
        const body: any = { query: params.query, limit: params.limit || 5 };
        if (params.tags?.length) body.tags = params.tags;
        if (params.metadata && Object.keys(params.metadata).length) body.metadata = params.metadata;
        const res = await client.postJson("/v1/search", body, ns);
        const results = (res?.results || []).map((r: any) => ({
          content: r?.memory?.content || "",
          summary: r?.memory?.summary || "",
          tier: r?.memory?.tier || "",
          score: typeof r?.score === "number" ? r.score : 0,
        }));
        return text({ results });
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
      description: "Store a durable fact, decision, or preference in long-term memory (memini).",
      parameters: Type.Object({
        content: Type.String({ description: "The fact to remember" }),
        tier: Type.Optional(
          Type.String({
            description:
              "semantic=durable knowledge, procedural=how-to, episodic=what happened, working=transient (default semantic)",
          }),
        ),
        tags: Type.Optional(Type.Array(Type.String(), { description: "Optional keywords for later search/filtering." })),
        category: Type.Optional(
          Type.String({
            description:
              "Optional topic bucket stored as metadata.category (e.g. bug_fixes, architecture_decisions) for browsing by subject later.",
          }),
        ),
      }),
      async execute(_id: any, params: any) {
        const body: any = { content: params.content, tier: params.tier || "semantic" };
        const VALID_TIERS = ["working", "episodic", "semantic", "procedural"];
        if (!VALID_TIERS.includes(body.tier)) body.tier = "semantic";
        if (params.tags?.length) body.tags = params.tags;
        if (params.category) body.metadata = { category: params.category };
        const res = await client.postJson("/v1/memories", body, ns);
        return text({ id: res?.id || null, success: res != null });
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
      const result = await client.postJson("/v1/search", body, ns);
      let results = Array.isArray(result?.results) ? result.results : [];
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
      if (!captureUser || captureUser.startsWith("[Subagent Context]")) return;
      const ns = effectiveNamespace(cfg, ctx);
      if (ns == null) return;
      const metadata: any = { source: "openclaw", format: "turn" };
      const session = sessionIdentity(ctx);
      if (session) metadata.session_id = session;
      if (!event?.success) metadata.failed = true;
      await client.postJson("/v1/memories", {
        content: `${captureUser.slice(0, 1000)}\n\n${assistantText.slice(0, 3000)}`,
        tier: "episodic",
        metadata,
      }, ns);
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
  },
});

export default plugin;
