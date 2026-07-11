/**
 * memini memory extension for Pi (https://pi.dev).
 *
 * Pi has no built-in MCP, but it has a first-class extension API. This extension
 * wires memory two ways at once:
 *
 *   - Automatic (no tool call needed):
 *       - before_agent_start: recall memories relevant to the user's prompt and
 *         inject them as a persistent context message before the agent runs.
 *       - agent_end: capture the completed user/assistant turn into memini as
 *         episodic memory.
 *   - Explicit tools (the model calls them on demand), modeled on the tool set
 *     Claude Code gets from memini's MCP server: memory_recall, memory_list,
 *     memory_remember, memory_forget.
 *
 * Talks to memini over REST (/v1/search, /v1/memories), scoped by the
 * X-Memini-Namespace header. Config comes from MEMINI_* env vars; secrets like
 * MEMINI_API_KEY stay in the environment. See ../README.md for the table.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { resolveNamespace } from "@memini/namespace-resolver";

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 30000;
const DEFAULT_RECALL_LIMIT = 3;
const DEFAULT_NAMESPACE = "pi";
const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

function envBool(value: string | undefined, fallback: boolean): boolean {
  if (value === undefined || value === null || value === "") return fallback;
  return !/^(0|false|no|off)$/i.test(String(value).trim());
}

/**
 * intEnv parses a non-negative integer env var and returns `def` when unset or
 * malformed — env values are user input and shouldn't crash a hook.
 */
export function intEnv(name: string, def: number): number {
  const raw = process.env[name];
  if (raw == null || raw === "") return def;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n < 0) return def;
  return n;
}

/** floatEnv parses a non-negative float env var; falls back to `def`. */
export function floatEnv(name: string, def: number): number {
  const raw = process.env[name];
  if (raw == null || raw === "") return def;
  const n = Number.parseFloat(raw);
  if (!Number.isFinite(n) || n < 0) return def;
  return n;
}

/**
 * labelsEnv parses MEMINI_INJECT_LABELS into a Set of enabled labels.
 * Recognized: "tier", "confidence", "age". Empty/unset returns an empty Set.
 */
export function labelsEnv(name = "MEMINI_INJECT_LABELS"): Set<string> {
  const raw = process.env[name];
  if (!raw) return new Set();
  return new Set(
    raw
      .split(/[|,]/)
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean),
  );
}

// sanitizeNamespace keeps the X-Memini-Namespace value header-safe: alnum, dot,
// dash, underscore; collapse the rest to dashes and trim.
export function sanitizeNamespace(s: string): string {
  return String(s)
    .trim()
    .replace(/[^A-Za-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// sanitizeNamespacePath sanitizes a hierarchical namespace per segment,
// preserving the "/" separators the resolver's tenant paths carry
// (work/memini must not flatten to work-memini — the other integrations keep
// the separator, and flattening would split memory across integrations).
export function sanitizeNamespacePath(s: string): string {
  return String(s)
    .split("/")
    .map(sanitizeNamespace)
    .filter(Boolean)
    .join("/");
}

// deriveNamespace scopes memory to the project: the basename of the working
// directory, the same scheme memini auto-resolves from a git repo. Returns ""
// when no path is given.
export function deriveNamespace(cwd: string | undefined): string {
  if (typeof cwd !== "string" || !cwd.trim()) return "";
  const base = cwd.replace(/[\\/]+$/, "").split(/[\\/]/).pop() || "";
  return sanitizeNamespace(base);
}

export interface ResolvedConfig {
  base_url: string;
  namespace: string;
  home?: string;
  recall: boolean;
  capture: boolean;
  recall_limit: number;
  recall_max_tokens: number;
  recall_min_score: number;
  timeout_ms: number;
  fallback_on_error: boolean;
}

// resolveConfig builds the config from env vars (Claude Code plugin style),
// deriving the namespace from cwd when MEMINI_NAMESPACE is unset. Exported for
// testing.
export function resolveConfig(env: NodeJS.ProcessEnv, cwd?: string): ResolvedConfig {
  const e = env || {};
  const nsEnv = (e.MEMINI_NAMESPACE || "").trim();
  let namespace: string;
  if (nsEnv) {
    // MEMINI_NAMESPACE wins and is used raw-trimmed (the server validates the
    // header). Routing it through sanitizeNamespacePath would alter an explicit
    // value — the canonical resolver returns it untouched.
    namespace = nsEnv;
  } else if (cwd) {
    const { namespace: resolvedNs } = resolveNamespace({
      cwd,
      env: e as Record<string, string>,
      integration: "pi",
    });
    // Per-segment sanitize: resolver output may be a tenant path (work/memini).
    namespace = sanitizeNamespacePath(resolvedNs) || DEFAULT_NAMESPACE;
  } else {
    // No explicit namespace and no cwd to resolve against.
    namespace = DEFAULT_NAMESPACE;
  }
  const recall_limit = (() => {
    const n = Number(e.MEMINI_RECALL_LIMIT);
    return Number.isFinite(n) && n >= 0 ? n : DEFAULT_RECALL_LIMIT;
  })();
  // home: the caller's personal namespace, sent as X-Memini-Home. Env-only,
  // mirroring MEMINI_NAMESPACE's precedence style — no cwd/derivation
  // fallback; unset means "no home leg".
  const homeEnv = (e.MEMINI_HOME || "").trim();
  return {
    base_url: e.MEMINI_BASE_URL || e.MEMINI_URL || DEFAULT_BASE_URL,
    // namespace is already resolved above (raw-trimmed on the env path,
    // per-segment sanitized on the resolver path); re-sanitizing here would
    // flatten tenant separators.
    namespace: namespace || DEFAULT_NAMESPACE,
    home: homeEnv || undefined,
    recall: envBool(e.MEMINI_RECALL, true),
    capture: envBool(e.MEMINI_CAPTURE, true),
    recall_limit,
    recall_max_tokens: intEnv("MEMINI_INJECT_RECALL_MAX_TOK", 0),
    recall_min_score: floatEnv("MEMINI_INJECT_RECALL_MIN_SCORE", 0),
    timeout_ms: Number(e.MEMINI_TIMEOUT_MS || DEFAULT_TIMEOUT_MS),
    fallback_on_error: envBool(e.MEMINI_FALLBACK, true),
  };
}

// --- token budget (copied from the opencode plugin; both ship standalone) ----

/** approxTokens: ~0.75 tokens/word, floor of 1 for any non-empty line. */
export function approxTokens(text: string): number {
  if (!text) return 0;
  const words = String(text).trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.ceil((words * 4) / 3));
}

/**
 * fitByTokens trims a list of pre-formatted strings under `maxTokens`, keeping
 * the head (most relevant first). maxTokens<=0 means unbounded.
 */
export function fitByTokens(
  items: string[],
  maxTokens: number,
): { items: string[]; tokens: number; dropped: number } {
  if (!Array.isArray(items) || items.length === 0) return { items: [], tokens: 0, dropped: 0 };
  if (!Number.isFinite(maxTokens) || maxTokens <= 0) {
    const tokens = items.reduce((sum, s) => sum + approxTokens(s), 0);
    return { items: items.slice(), tokens, dropped: 0 };
  }
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
  return { items: out, tokens: used, dropped };
}

/** truncate to `max` chars with a marker. */
export function truncate(value: string, max: number): string {
  return value.length > max ? value.slice(0, max) + "\n[...truncated]" : value;
}

// formatResults renders search hits to bullet lines. Empty labels -> "- (tier)
// text"; non-empty -> "[tier · conf · age] text". Matches the opencode plugin.
export function formatResults(results: any[], limit: number, labels?: Set<string>): string[] {
  if (!Array.isArray(results) || results.length === 0) return [];
  const useLabels = labels && labels.size > 0 ? labels : null;
  return results
    .slice(0, limit || DEFAULT_RECALL_LIMIT)
    .map((result, index) => {
      const mem = (result && result.memory) || {};
      const text = truncate(String(mem.summary || mem.content || `Memory ${index + 1}`).trim(), 300);
      if (!text) return null;
      const tier = String(mem.tier || "memory").trim();
      if (!useLabels) return `- (${tier}) ${text}`;
      const tagParts: string[] = [];
      if (useLabels.has("tier") && tier) tagParts.push(tier);
      if (useLabels.has("confidence") && typeof mem.confidence === "number") {
        tagParts.push(`conf=${mem.confidence.toFixed(2)}`);
      }
      if (useLabels.has("age") && mem.created_at) {
        const ageMs = Date.now() - new Date(mem.created_at).getTime();
        if (Number.isFinite(ageMs) && ageMs >= 0) {
          const days = Math.floor(ageMs / 86400000);
          tagParts.push(days === 0 ? "today" : `${days}d`);
        }
      }
      if (tagParts.length === 0) return `- (${tier}) ${text}`;
      return `[${tagParts.join(" · ")}] ${text}`;
    })
    .filter((x): x is string => Boolean(x));
}

// --- plaintext-bearer guard (ported from the opencode/openclaw plugins) ------

function normalizedHostname(hostname: string): string {
  return hostname.replace(/^\[|\]$/g, "").toLowerCase();
}

function usesPlaintextBearerAuth(baseUrl: string, secret?: string): boolean {
  if (!secret) return false;
  try {
    const parsed = new URL(baseUrl);
    return parsed.protocol === "http:" && !LOOPBACK_HOSTS.has(normalizedHostname(parsed.hostname));
  } catch {
    return false;
  }
}

function plaintextBearerAuthMessage(baseUrl: string): string {
  return `memini: MEMINI_API_KEY is configured for plaintext HTTP to ${baseUrl}. Bearer tokens and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
}

export function createPlaintextBearerAuthGuard(warn: (m: string) => void, env?: NodeJS.ProcessEnv) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl: string, secret?: string): void {
    if (!usesPlaintextBearerAuth(baseUrl, secret)) return;
    const message = plaintextBearerAuthMessage(baseUrl);
    if ((env || process.env).MEMINI_REQUIRE_HTTPS === "1") throw new Error(message);
    if (!warned) {
      warned = true;
      warn(message);
    }
  };
}

// --- REST client -------------------------------------------------------------

interface MeminiClient {
  postJson: (path: string, payload: any) => Promise<any>;
  getJson: (path: string) => Promise<any>;
  deleteJson: (path: string) => Promise<any>;
}

function createClient(cfg: ResolvedConfig, warn: (m: string) => void): MeminiClient {
  const baseUrl = String(cfg.base_url).replace(/\/+$/, "");
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
  const guard = createPlaintextBearerAuthGuard(warn);
  if (process.env.MEMINI_REQUIRE_HTTPS === "1") guard(baseUrl, secret);

  function headers(extra?: Record<string, string>): Record<string, string> {
    const h: Record<string, string> = { "X-Memini-Namespace": cfg.namespace, ...(extra || {}) };
    if (secret) h.Authorization = `Bearer ${secret}`;
    if (cfg.home) h["X-Memini-Home"] = cfg.home;
    return h;
  }

  async function request(method: string, path: string, body?: any): Promise<any> {
    guard(baseUrl, secret);
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method,
        headers: headers(body ? { "Content-Type": "application/json" } : undefined),
        body: body ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(cfg.timeout_ms),
      });
      if (!res.ok) {
        if (cfg.fallback_on_error) {
          // Degrade but never silently: a swallowed 401/500 on a capture or
          // recall looks like "memory isn't working" with nothing to debug.
          warn(`memini ${method} ${path} failed: ${res.status}`);
          return null;
        }
        const text = await res.text().catch(() => "");
        throw new Error(`memini ${method} ${path} failed: ${res.status} ${text}`);
      }
      // 204 (DELETE) has an empty body; treat a 2xx as ok.
      return await res.json().catch(() => ({ ok: true }));
    } catch (error) {
      if (!cfg.fallback_on_error) throw error;
      warn(`memini: ${String(error)}`);
      return null;
    }
  }

  return {
    postJson: (path, payload) => request("POST", path, payload),
    getJson: (path) => request("GET", path),
    deleteJson: (path) => request("DELETE", path),
  };
}

// meminiListPath builds the GET /v1/memories query string for memory_list:
// repeatable tier/tag params plus meta=key=value pairs. Exported for testing.
export function meminiListPath(args: any): string {
  const parts: string[] = [];
  for (const t of args?.tiers || []) parts.push(`tier=${encodeURIComponent(String(t))}`);
  for (const tag of args?.tags || []) parts.push(`tag=${encodeURIComponent(String(tag))}`);
  for (const [k, v] of Object.entries(args?.metadata || {})) {
    parts.push(`meta=${encodeURIComponent(`${k}=${v}`)}`);
  }
  if (Number.isInteger(args?.limit) && args.limit > 0) parts.push(`limit=${args.limit}`);
  return parts.length ? `/v1/memories?${parts.join("&")}` : "/v1/memories";
}

// --- turn capture helpers ----------------------------------------------------

// extractMessageText pulls plain text out of a Pi AgentMessage, whose content
// may be a string or an array of typed parts. Exported for testing.
export function extractMessageText(message: any): string {
  if (!message) return "";
  const c = message.content;
  if (typeof c === "string") return c.trim();
  if (Array.isArray(c)) {
    return c
      .filter((p: any) => p && p.type === "text" && typeof p.text === "string")
      .map((p: any) => p.text)
      .join("\n")
      .trim();
  }
  if (typeof message.text === "string") return message.text.trim();
  return "";
}

// extractLastAssistantText returns the text of the most recent assistant message.
// agent_end carries the full conversation (AgentMessage[]), not just this run's
// messages, so iterate in reverse and take the latest assistant turn only — never
// a join of every reply in the session. Exported for testing.
export function extractLastAssistantText(messages: any[]): string {
  if (!Array.isArray(messages)) return "";
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m && m.role === "assistant") {
      const t = extractMessageText(m);
      if (t) return t;
    }
  }
  return "";
}

// buildTurnContent assembles the episodic payload from the user prompt and the
// assistant reply, bounding each side. Exported for testing.
export function buildTurnContent(userText: string, assistantText: string): string {
  return `${String(userText).slice(0, 1000)}\n\n${String(assistantText).slice(0, 3000)}`;
}

const TOOL_NAMES = ["memory_recall", "memory_list", "memory_remember", "memory_forget"];
const VALID_TIERS = ["working", "episodic", "semantic", "procedural"];

// sessionIdOf resolves Pi's session id from the read-only session manager on the
// extension context, so echo-exclusion and capture-dedup are keyed consistently.
// "" when unavailable.
function sessionIdOf(ctx: any): string {
  try {
    return String(ctx?.sessionManager?.getSessionId?.() ?? "");
  } catch {
    return "";
  }
}

// leafIdOf returns the current session leaf-entry id — a stable per-turn key for
// capture dedup, since Pi's AgentMessages carry no id of their own.
function leafIdOf(ctx: any): string {
  try {
    return String(ctx?.sessionManager?.getLeafId?.() ?? "");
  } catch {
    return "";
  }
}

export default function meminiExtension(pi: ExtensionAPI): void {
  const warn = (m: string) => {
    try {
      // ctx.ui.notify isn't available at module scope; log to stderr.
      console.error(`[memini] ${m}`);
    } catch {
      /* ignore */
    }
  };

  const cfg = resolveConfig(process.env, process.cwd());
  const client = createClient(cfg, warn);
  // Latest user prompt per session, set in before_agent_start and consumed in
  // agent_end to assemble the full turn.
  const pendingUser = new Map<string, string>();
  // Assistant message ids already captured, so a re-fired agent_end never writes
  // a duplicate turn.
  const captured = new Set<string>();
  // Memory ids each session has already been shown (mirrors the openclaw
  // plugin): the recall injection is a persistent context message, so
  // re-injecting an unchanged match every turn stacks identical blocks in the
  // prompt. Bounded so long-lived hosts can't grow the map without limit.
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

  // Recall before the turn: search for the user's prompt and inject the matches
  // as a persistent context message. Buffer the prompt for capture at agent_end.
  pi.on("before_agent_start", async (event, ctx) => {
    const sid = sessionIdOf(ctx);
    const query = String(event?.prompt || "").trim();
    if (query && sid) pendingUser.set(sid, query);
    if (!cfg.recall || !query) return;

    const body: any = { query, limit: cfg.recall_limit };
    // Exclude this session's own captured turns: they're still in live context,
    // so recalling them just echoes the conversation back a turn behind.
    if (sid) body.exclude_metadata = { session_id: sid };
    if (cfg.recall_min_score > 0) body.min_score = cfg.recall_min_score;

    const result = await client.postJson("/v1/search", body);
    const floor = cfg.recall_min_score > 0 ? cfg.recall_min_score : 0;
    let rawHits = Array.isArray(result?.results) ? result.results : [];
    // Suppress memories this session has already been shown — the injected
    // message persists in context, so a repeat adds nothing but noise.
    if (sid) {
      const seen = injectedBySession.get(sid);
      if (seen?.size) rawHits = rawHits.filter((r: any) => !seen.has(r?.memory?.id));
    }
    const filtered =
      floor > 0
        ? rawHits.filter((r: any) => (typeof r?.score === "number" ? r.score : 0) >= floor)
        : rawHits;
    const hits = formatResults(filtered, cfg.recall_limit, labelsEnv());
    if (hits.length === 0) return;

    const fit = fitByTokens(hits, cfg.recall_max_tokens);
    if (fit.items.length === 0) return;
    if (sid) {
      rememberInjected(sid, filtered.map((r: any) => r?.memory?.id).filter(Boolean));
    }
    const lines = [
      "Relevant long-term memory from memini (background context — prefer " +
        "current workspace state and the user's instructions):",
      ...fit.items,
    ];
    // /v1/search sets `degraded: "keyword_only"` (plus a `note`) when the query
    // embed was unavailable and it fell back to keyword-only matching; both are
    // already on `result`, so surfacing them is a one-line addition.
    if (result?.degraded) {
      lines.push(
        `[memini: ${result.note || "semantic search unavailable — results are keyword-only and may be incomplete"}]`,
      );
    }
    if (fit.dropped > 0) lines.push(`[... ${fit.dropped} item(s) truncated by token budget]`);

    return {
      message: {
        customType: "memini-recall",
        content: lines.join("\n"),
        display: true,
      },
    };
  });

  // Capture after the turn: pair the buffered user prompt with the assistant
  // reply from this run and store it as episodic memory.
  pi.on("agent_end", async (event, ctx) => {
    if (!cfg.capture) return;
    const sid = sessionIdOf(ctx);
    const userText = (sid && pendingUser.get(sid)) || "";
    const assistantText = extractLastAssistantText(event?.messages);
    if (!userText || !assistantText) return;
    // AgentMessages carry no id, so key dedup on the session leaf entry — it
    // advances each turn, so a re-fired agent_end for the same turn is skipped.
    const dedupKey = leafIdOf(ctx);
    if (dedupKey && captured.has(dedupKey)) return;

    const metadata: Record<string, any> = { source: "pi", format: "turn" };
    if (sid) metadata.session_id = sid;

    const stored = await client.postJson("/v1/memories", {
      content: buildTurnContent(userText, assistantText),
      tags: ["pi"],
      metadata,
    });
    if (stored !== null) {
      if (dedupKey) captured.add(dedupKey);
      if (sid) pendingUser.delete(sid);
    }
  });

  // Explicit tools — the same set Claude Code gets from memini's MCP server.
  const text = (obj: any) => ({ content: [{ type: "text" as const, text: JSON.stringify(obj) }], details: {} });
  const Tags = Type.Optional(
    Type.Array(Type.String(), { description: "Match only memories carrying every listed tag (AND)." }),
  );
  const Metadata = Type.Optional(
    Type.Record(Type.String(), Type.String(), {
      description:
        'Match memories whose top-level metadata contains each key=value pair, e.g. {"category":"bug_fixes"}.',
    }),
  );

  pi.registerTool({
    name: "memory_recall",
    label: "Recall memory",
    description:
      "Recall relevant memories from long-term memory (memini) via hybrid (semantic + keyword) search. " +
      "Call before starting work that may have history: editing an unfamiliar file, debugging a recurring " +
      "issue, or when asked what's known about something. Empty results mean nothing is known — proceed " +
      "from first principles, never invent a remembered fact. A degraded:\"keyword_only\" field in the " +
      "result means semantic search was unavailable and results came from keyword matching alone — treat " +
      "as incomplete, not exhaustive.",
    parameters: Type.Object({
      query: Type.String({ description: "What to search for" }),
      limit: Type.Optional(Type.Number({ description: "Max results (default 3)" })),
      tags: Tags,
      metadata: Metadata,
    }),
    async execute(_toolCallId: string, params: any) {
      const body: any = { query: params.query, limit: params.limit || DEFAULT_RECALL_LIMIT };
      if (params.tags?.length) body.tags = params.tags;
      if (params.metadata && Object.keys(params.metadata).length) body.metadata = params.metadata;
      const res = await client.postJson("/v1/search", body);
      const results = (res?.results || []).map((r: any) => ({
        id: r?.memory?.id || "",
        content: r?.memory?.content || "",
        summary: r?.memory?.summary || "",
        tier: r?.memory?.tier || "",
        score: typeof r?.score === "number" ? r.score : 0,
      }));
      // /v1/search already carries `degraded`/`note` on `res`; pass them through
      // rather than dropping them silently.
      return text(res?.degraded ? { results, degraded: res.degraded, note: res.note } : { results });
    },
  });

  pi.registerTool({
    name: "memory_list",
    label: "List memory",
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
    async execute(_toolCallId: string, params: any) {
      const args = { ...params, limit: params.limit ?? 20 };
      const res = await client.getJson(meminiListPath(args));
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
  });

  pi.registerTool({
    name: "memory_remember",
    label: "Remember",
    description:
      "Store a durable fact, decision, or preference in long-term memory (memini). Call proactively when " +
      "the user says 'remember this', after an architectural decision (capture the why), or after " +
      "discovering a non-obvious bug or convention. Keep memories atomic — one self-contained fact per " +
      "call. Don't store what's already in project docs or trivially recoverable from code. To correct " +
      "an existing memory, pass its id — the write updates it in place.",
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
    }),
    async execute(_toolCallId: string, params: any) {
      // No client-side tier default: an omitted (or invalid) tier lets the
      // server classify the content and apply its own default.
      const body: any = { content: params.content };
      if (params.id) body.id = params.id; // POST /v1/memories upserts by id
      if (params.tier && VALID_TIERS.includes(params.tier)) body.tier = params.tier;
      if (params.tags?.length) body.tags = params.tags;
      if (params.category) body.metadata = { category: params.category };
      const res = await client.postJson("/v1/memories", body);
      return text({ id: res?.id || null, success: res != null });
    },
  });

  pi.registerTool({
    name: "memory_forget",
    label: "Forget",
    description:
      "Permanently delete a memory from long-term memory (memini) by its id — use when a recalled memory " +
      "is wrong, outdated, or poisoned. Get the id from memory_recall or memory_list. To correct a fact " +
      "instead, call memory_remember with the existing id (it updates in place, preserving history); " +
      "forget only memories that should not exist at all.",
    parameters: Type.Object({
      id: Type.String({ description: "The id of the memory to forget (from memory_recall / memory_list)." }),
    }),
    async execute(_toolCallId: string, params: any) {
      if (!params.id) return text({ forgotten: false, error: "id is required" });
      const res = await client.deleteJson(`/v1/memories/${encodeURIComponent(params.id)}`);
      return text({ forgotten: res != null });
    },
  });

  void TOOL_NAMES;
}
