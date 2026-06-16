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

import { createRequire } from "node:module";

// OpenClaw rejects an async `register`, so typebox can't be pulled in with an
// `await import(...)`. createRequire gives a synchronous, lazy load (typebox
// ships a CJS build) — only invoked when expose_tools is on, and the try/catch
// in register still lets the memory slot work if it can't be loaded.
const require = createRequire(import.meta.url);

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 5000;
const DEFAULT_NAMESPACE = "openclaw";
const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

const configSchema = {
  type: "object",
  additionalProperties: false,
  properties: {
    enabled: { type: "boolean" },
    base_url: { type: "string" },
    namespace: { type: "string" },
    namespace_per_agent: { type: "boolean" },
    namespace_template: { type: "string" },
    skip_without_agent: { type: "boolean" },
    skip_system_turns: { type: "boolean" },
    system_kinds: { type: "array", items: { type: "string" } },
    fallback_on_error: { type: "boolean" },
    timeout_ms: { type: "number" },
    expose_tools: { type: "boolean" },
  },
};

// Per-agent isolation is the default: each named agent gets its own namespace
// so subagents sharing one OpenClaw install do not poison each other's memory.
// The default template prefixes the configured base ("openclaw" -> "openclaw-miso")
// so per-agent namespaces are distinct from the shared fallback used for
// sessions that carry no agent identity.
const DEFAULT_NAMESPACE_TEMPLATE = "{namespace}-{agent}";

// Turn "kinds" that mark a system-initiated turn (scheduled/heartbeat/cron
// polls) rather than a user-driven one. These resolve an agent identity like
// any other turn, so skip_without_agent doesn't catch them — skip_system_turns
// does. Override the set via the system_kinds config.
const DEFAULT_SYSTEM_KINDS = ["cron", "heartbeat", "scheduled", "schedule"];

// resolveConfig normalizes raw plugin config into the defaults the plugin runs
// with. Exported so the defaults (notably per-agent isolation) are testable.
export function resolveConfig(pluginConfig) {
  const c = pluginConfig || {};
  return {
    enabled: c.enabled !== false,
    base_url: c.base_url || DEFAULT_BASE_URL,
    namespace: c.namespace || DEFAULT_NAMESPACE,
    namespace_per_agent: c.namespace_per_agent !== false,
    namespace_template: c.namespace_template || DEFAULT_NAMESPACE_TEMPLATE,
    skip_without_agent: c.skip_without_agent === true,
    // Off by default (backward-compat): when on, system-initiated turns
    // (cron/heartbeat/scheduled polls) are skipped for both recall and capture
    // even when they carry an agent identity, so scheduled-task chatter doesn't
    // accumulate as episodic noise. system_kinds overrides the matched kinds.
    skip_system_turns: c.skip_system_turns === true,
    system_kinds:
      Array.isArray(c.system_kinds) && c.system_kinds.length
        ? c.system_kinds.map((k) => String(k).toLowerCase())
        : DEFAULT_SYSTEM_KINDS,
    fallback_on_error: c.fallback_on_error !== false,
    timeout_ms: c.timeout_ms || DEFAULT_TIMEOUT_MS,
    // Off by default: the slot already recalls/captures automatically; tools are
    // opt-in for agents that want to read/browse/write on demand.
    expose_tools: c.expose_tools === true,
  };
}

// sanitizeNsSegment keeps a namespace segment header-safe (the server sanitizes
// too, but the X-Memini-Namespace value should be clean): alnum, dot, dash,
// underscore; collapse the rest to dashes and trim.
function sanitizeNsSegment(s) {
  return String(s).trim().replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
}

// Session keys look like "agent:<id>:..." (e.g. agent:carol:cron:...);
// extract the agent segment. Raw session UUIDs are NOT identities — treating
// them as such fragments memory into per-session namespaces.
const SESSION_KEY_AGENT = /(?:^|[:/])agent[:/]([^:/]+)/;

function parseAgentFromSessionKey(value) {
  if (typeof value !== "string") return "";
  const match = value.match(SESSION_KEY_AGENT);
  return match ? match[1] : "";
}

// agentIdentity pulls a stable per-agent id from an OpenClaw hook event and
// the hook context (the gateway invokes handlers as handler(event, ctx); some
// events carry no identity fields, but ctx.sessionKey is keyed by agent).
// Returns "" when neither identifies an agent.
function agentIdentity(event, ctx) {
  const direct = [
    ctx?.agentId,
    ctx?.agent_id,
    event?.agentId,
    event?.agent_id,
    event?.agentName,
    event?.agent?.id,
    event?.agent?.name,
    event?.agent?.slug,
    event?.session?.agentId,
    event?.session?.agent_id,
  ];
  for (const c of direct) {
    if (typeof c === "string" && c.trim()) return c.trim();
  }
  const keys = [
    ctx?.sessionKey,
    ctx?.sessionId,
    ctx?.runId,
    event?.sessionKey,
    event?.sessionId,
    event?.runId,
  ];
  for (const k of keys) {
    const id = parseAgentFromSessionKey(k);
    if (id) return id;
  }
  return "";
}

// effectiveNamespace returns the configured namespace, or a per-agent namespace
// when namespace_per_agent is enabled and the event identifies an agent. The
// per-agent name comes from namespace_template (default "{agent}"), with
// {agent} and {namespace} substituted — e.g. "{agent}" -> "alice",
// "openclaw-{agent}" -> "openclaw-alice". Falls back to the base namespace when
// no agent id is present, preserving the shared-memory behavior — unless
// skip_without_agent is set, in which case it returns null so the caller skips
// the operation entirely (no recall, no write, no fallback namespace). Useful
// for gateways where unattributable sessions (cron, heartbeat) should not
// pollute memory.
export function effectiveNamespace(cfg, event, ctx) {
  if (!cfg.namespace_per_agent) return cfg.namespace;
  const id = sanitizeNsSegment(agentIdentity(event, ctx));
  if (!id) return cfg.skip_without_agent ? null : cfg.namespace;
  const tmpl = cfg.namespace_template || DEFAULT_NAMESPACE_TEMPLATE;
  return tmpl.replaceAll("{agent}", id).replaceAll("{namespace}", cfg.namespace);
}

// sessionIdentity pulls a stable per-session id from the hook event/ctx, used
// to tag captured turns and then exclude the current session's own captures
// from its pre-turn auto-recall — otherwise a turn still in the live transcript
// gets echoed back as "long-term memory" the very next turn. Unlike
// agentIdentity (which is per-agent and shared across that agent's sessions),
// this is per-session, so two sessions of the same agent don't suppress each
// other. Returns "" when nothing identifies a session (recall/capture then
// behave as before).
export function sessionIdentity(event, ctx) {
  const candidates = [
    ctx?.sessionId,
    ctx?.sessionKey,
    ctx?.runId,
    event?.sessionId,
    event?.sessionKey,
    event?.runId,
    event?.session?.id,
  ];
  for (const c of candidates) {
    if (typeof c === "string" && c.trim()) return sanitizeNsSegment(c);
  }
  return "";
}

// Leading marker some gateways prepend to a system turn's text, e.g.
// "[OpenClaw heartbeat poll]" or "[cron:daily (...)]". Only the first bracketed
// segment is inspected, so a user quoting "[cron ...]" mid-message is ignored.
const LEADING_MARKER = /^\s*\[([^\]]{1,80})\]/;

// detectSystemKind returns the system-turn kind for an event when one is
// identifiable, else "". It checks, in order: explicit kind/trigger fields on
// ctx/event, the session key segments (agent:<id>:cron:..., heartbeat:gateway),
// and a leading bracket marker on the turn text. Field values match on
// substring (kind:"scheduled" -> "scheduled"); session-key segments match
// whole-segment only, so an agent id like "concord" never reads as "cron".
export function detectSystemKind(event, ctx, text, kinds = DEFAULT_SYSTEM_KINDS) {
  const includesKind = (value) => {
    if (typeof value !== "string" || !value) return "";
    const lower = value.toLowerCase();
    return kinds.find((k) => lower.includes(k)) || "";
  };

  const fields = [
    ctx?.kind, ctx?.trigger, ctx?.sessionKind,
    event?.kind, event?.trigger, event?.sessionKind,
    event?.session?.kind, event?.session?.trigger,
  ];
  for (const f of fields) {
    const k = includesKind(f);
    if (k) return k;
  }

  const keys = [ctx?.sessionKey, event?.sessionKey, ctx?.sessionId, event?.sessionId, ctx?.runId, event?.runId];
  for (const key of keys) {
    if (typeof key !== "string") continue;
    for (const seg of key.split(/[:/]/)) {
      const k = kinds.find((kind) => kind === seg.toLowerCase());
      if (k) return k;
    }
  }

  const m = typeof text === "string" ? text.match(LEADING_MARKER) : null;
  return m ? includesKind(m[1]) : "";
}

// shouldSkipSystemTurn reports whether this turn should be skipped (no recall,
// no capture) because skip_system_turns is on and the turn is system-initiated.
export function shouldSkipSystemTurn(cfg, event, ctx, text) {
  if (!cfg.skip_system_turns) return false;
  return detectSystemKind(event, ctx, text, cfg.system_kinds) !== "";
}

function extractText(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .flatMap((block) => {
      if (!block || typeof block !== "object") return [];
      if (block.type === "text" && typeof block.text === "string") return [block.text];
      return [];
    })
    .join("\n")
    .trim();
}

function lastTextByRole(messages, role) {
  for (const message of [...messages].reverse()) {
    if (!message || typeof message !== "object" || message.role !== role) continue;
    const text = extractText(message.content);
    if (text) return text;
  }
  return "";
}

function formatResults(results) {
  if (!Array.isArray(results) || results.length === 0) return "";
  return results
    .slice(0, 5)
    .map((result, index) => {
      const mem = result?.memory ?? {};
      const text = (mem.summary || mem.content || `Memory ${index + 1}`).trim();
      const tier = (mem.tier || "memory").trim();
      return `- (${tier}) ${text.slice(0, 300)}`;
    })
    .filter(Boolean)
    .join("\n");
}

function normalizedHostname(hostname) {
  return hostname.replace(/^\[|\]$/g, "").toLowerCase();
}

function usesPlaintextBearerAuth(baseUrl, secret) {
  if (!secret) return false;
  try {
    const parsed = new URL(baseUrl);
    return parsed.protocol === "http:" && !LOOPBACK_HOSTS.has(normalizedHostname(parsed.hostname));
  } catch {
    return false;
  }
}

function plaintextBearerAuthMessage(baseUrl) {
  return `memini: MEMINI_API_KEY is configured for plaintext HTTP to ${baseUrl}. Bearer tokens and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
}

export function createPlaintextBearerAuthGuard(warn, env) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl, secret) {
    if (!usesPlaintextBearerAuth(baseUrl, secret)) return;
    const message = plaintextBearerAuthMessage(baseUrl);
    if ((env || process.env).MEMINI_REQUIRE_HTTPS === "1") throw new Error(message);
    if (!warned) {
      warned = true;
      warn(message);
    }
  };
}

function createClient(cfg, api) {
  const baseUrl = String(cfg.base_url || DEFAULT_BASE_URL).replace(/\/+$/, "");
  const namespace = String(cfg.namespace || DEFAULT_NAMESPACE);
  const timeoutMs = Number(cfg.timeout_ms || DEFAULT_TIMEOUT_MS);
  const fallbackOnError = cfg.fallback_on_error !== false;
  const secret = process.env.MEMINI_API_KEY;
  const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m) => api.logger.warn?.(m));
  if (process.env.MEMINI_REQUIRE_HTTPS === "1") {
    guardPlaintextBearerAuth(baseUrl, secret);
  }

  async function postJson(path, payload, ns) {
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers = { "Content-Type": "application/json", "X-Memini-Namespace": ns || namespace };
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

  async function getJson(path, ns) {
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers = { "X-Memini-Namespace": ns || namespace };
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
export function meminiListPath(args) {
  const parts = [];
  for (const t of args?.tiers || []) parts.push(`tier=${encodeURIComponent(String(t))}`);
  for (const tag of args?.tags || []) parts.push(`tag=${encodeURIComponent(String(tag))}`);
  for (const [k, v] of Object.entries(args?.metadata || {})) {
    parts.push(`meta=${encodeURIComponent(`${k}=${v}`)}`);
  }
  if (Number.isInteger(args?.limit) && args.limit > 0) parts.push(`limit=${args.limit}`);
  return parts.length ? `/v1/memories?${parts.join("&")}` : "/v1/memories";
}

const plugin = {
  id: "memini",
  name: "memini",
  description: "Shared cross-session memory via a memini service.",
  configSchema,
  // Synchronous by contract — see the createRequire note at the top of the file.
  register(api) {
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

    api.on("before_prompt_build", async (event, ctx) => {
      if (!cfg.enabled) return;
      const prompt = typeof event?.prompt === "string" ? event.prompt.trim() : "";
      if (!prompt) return;
      if (shouldSkipSystemTurn(cfg, event, ctx, prompt)) return;
      const ns = effectiveNamespace(cfg, event, ctx);
      if (ns == null) return;
      const body = { query: prompt, limit: 5 };
      // Exclude this session's own captured turns: they're already in the live
      // transcript, so recalling them just echoes the conversation back a turn
      // behind. Captures from other (past) sessions are still recalled.
      const session = sessionIdentity(event, ctx);
      if (session) body.exclude_metadata = { session };
      const result = await client.postJson("/v1/search", body, ns);
      const block = formatResults(result?.results || []);
      if (!block) return;
      return { prependContext: `Relevant long-term memory from memini:\n${block}` };
    });

    api.on("agent_end", async (event, ctx) => {
      if (!cfg.enabled || !event?.success || !Array.isArray(event.messages)) return;
      const userText = lastTextByRole(event.messages, "user");
      const assistantText = lastTextByRole(event.messages, "assistant");
      if (!userText || !assistantText) return;
      if (shouldSkipSystemTurn(cfg, event, ctx, userText)) return;
      const ns = effectiveNamespace(cfg, event, ctx);
      if (ns == null) return;
      // Tag the capture with its session id so before_prompt_build can exclude
      // this session's own turns from its auto-recall (see that hook).
      const metadata = { source: "openclaw" };
      const session = sessionIdentity(event, ctx);
      if (session) metadata.session = session;
      await client.postJson("/v1/memories", {
        content: `User: ${userText.slice(0, 1000)}\nAssistant: ${assistantText.slice(0, 3000)}`,
        tier: "episodic",
        metadata,
      }, ns);
    });

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
};

// registerMeminiTools registers memory_recall / memory_list / memory_remember as
// explicit OpenClaw tools. Synchronous (register must not be async): typebox is
// loaded lazily via require so it's only needed when expose_tools is on; each
// tool resolves the namespace like the hooks do.
export function registerMeminiTools(api, client, cfg) {
  const { Type } = require("@sinclair/typebox");
  const text = (obj) => ({ content: [{ type: "text", text: JSON.stringify(obj) }] });
  const nsFor = (ctx) => effectiveNamespace(cfg, {}, ctx) ?? cfg.namespace;
  const Tags = Type.Optional(
    Type.Array(Type.String(), { description: "Match only memories carrying every listed tag (AND)." }),
  );
  const Metadata = Type.Optional(
    Type.Record(Type.String(), Type.String(), {
      description: 'Match memories whose top-level metadata contains each key=value pair, e.g. {"category":"bug_fixes"}.',
    }),
  );

  api.registerTool(
    {
      name: "memory_recall",
      description: "Search long-term memory (memini) for relevant past facts and context.",
      parameters: Type.Object({
        query: Type.String({ description: "What to search for" }),
        limit: Type.Optional(Type.Number({ description: "Max results (default 5)" })),
        tags: Tags,
        metadata: Metadata,
      }),
      async execute(_id, params, ctx) {
        const body = { query: params.query, limit: params.limit || 5 };
        if (params.tags?.length) body.tags = params.tags;
        if (params.metadata && Object.keys(params.metadata).length) body.metadata = params.metadata;
        const res = await client.postJson("/v1/search", body, nsFor(ctx));
        const results = (res?.results || []).map((r) => ({
          content: r?.memory?.content || "",
          summary: r?.memory?.summary || "",
          tier: r?.memory?.tier || "",
          score: typeof r?.score === "number" ? r.score : 0,
        }));
        return text({ results });
      },
    },
    { optional: true },
  );

  api.registerTool(
    {
      name: "memory_list",
      description:
        "Browse long-term memory (memini) without a query — filter by tier, tags, or metadata " +
        "category (e.g. all procedural memories, or everything categorized bug_fixes). Newest first.",
      parameters: Type.Object({
        tiers: Type.Optional(
          Type.Array(Type.String(), { description: "Restrict to these tiers; empty means all." }),
        ),
        tags: Tags,
        metadata: Metadata,
        limit: Type.Optional(Type.Number({ description: "Max results (0 = all, default 20)" })),
      }),
      async execute(_id, params, ctx) {
        const args = { ...params, limit: params.limit ?? 20 };
        const res = await client.getJson(meminiListPath(args), nsFor(ctx));
        const memories = (res?.memories || []).map((m) => ({
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
    { optional: true },
  );

  api.registerTool(
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
      async execute(_id, params, ctx) {
        const body = { content: params.content, tier: params.tier || "semantic" };
        if (params.tags?.length) body.tags = params.tags;
        if (params.category) body.metadata = { category: params.category };
        const res = await client.postJson("/v1/memories", body, nsFor(ctx));
        return text({ id: res?.id || null, success: res != null });
      },
    },
    { optional: true },
  );
}

export default plugin;
