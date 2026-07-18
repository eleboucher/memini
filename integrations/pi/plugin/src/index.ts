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
 * X-Memini-Namespace header. Namespace and behavioral settings come from the
 * config-handshake redesign (POST /v1/handshake, api/openapi.yaml): pi imports
 * @memini/client directly (unlike the standalone opencode/hermes/openwebui
 * plugins, which ship a wire-shape copy) and composes gatherFacts +
 * performHandshake + resolveNamespace + effectiveSetting the same way the
 * Claude Code plugin's hooks do (plugin/scripts/_shared.mjs), adapted to pi's
 * long-lived, one-process-per-session model: the handshake is memoized
 * in-memory for HANDSHAKE_TTL_MS rather than cached to a file, since there is
 * no second process that could ever read that file. See ../README.md for the
 * env var table.
 */

import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { readFileSync } from "node:fs";
import {
  readBootstrap,
  gatherFacts,
  performHandshake,
  resolveNamespace,
  deriveLocalNamespace,
  effectiveSetting,
  buildTurnCapture,
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
const DEFAULT_TIMEOUT_MS = 30000;
const DEFAULT_RECALL_LIMIT = 3;
// The status probes are diagnostics, not the hot path: fail fast rather than
// hang a slash command behind the recall/capture request timeout.
const STATUS_TIMEOUT_MS = 4000;
// performHandshake's own default (2500ms) is used when this isn't overridden;
// named here only so callers reading this file see the actual value in force.
const HANDSHAKE_TIMEOUT_MS = 2500;

// The client identifies itself to /v1/handshake for logging/diagnostics only
// (api/openapi.yaml's HandshakeRequest.client). Version is read from this
// extension's own package.json so it never has to be kept in sync by hand;
// "0.0.0" degrades gracefully when running from a checkout that lacks one.
const CLIENT_NAME = "pi-memini";
function readPluginVersion(): string {
  try {
    const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
    return typeof pkg.version === "string" && pkg.version ? pkg.version : "0.0.0";
  } catch {
    return "0.0.0";
  }
}
const CLIENT_VERSION = readPluginVersion();

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

// --- memoization (in-memory only; pi is one process per session) ------------

interface Memo<T> {
  /** Return the cached value, refreshing via `fn` if empty or past `ttlMs`. */
  get(): Promise<T>;
  /** Drop the cached value — the next `get()` call refreshes unconditionally.
   *  Used after a pin write, so the very next hook/tool call re-handshakes
   *  against the server's now-current project map instead of waiting out the
   *  rest of the TTL window. */
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

// --- session context: facts + memoized handshake -----------------------------

export interface SessionContext {
  boot: Bootstrap;
  facts: ProjectFacts;
  memo: Memo<HandshakeResult | undefined>;
}

/**
 * Attempt a live handshake. performHandshake is already fail-soft for network
 * errors, non-2xx, malformed JSON, and timeouts (returns undefined) — the one
 * exception is the plaintext-bearer guard, which throws on purpose. Whether
 * THAT throw is swallowed here is what `fallbackOnError` (MEMINI_FALLBACK)
 * decides, matching the extension's existing degrade-vs-surface knob for every
 * other memini call.
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

/**
 * Build a session's fixed inputs (bootstrap + project facts) and its memoized
 * handshake. Facts are gathered once — pi is one process per project/session,
 * so unlike a hook script invoked fresh per tool call, there is no cwd drift to
 * track. `now` is injectable for TTL tests. Exported for testing.
 */
export function createSessionContext(
  cwd: string,
  env: NodeJS.ProcessEnv = process.env,
  now: () => number = Date.now,
): SessionContext {
  const e = env as Record<string, string | undefined>;
  const boot = readBootstrap(e);
  const facts = gatherFacts(cwd, e);
  const fallbackOnError = envBool(e.MEMINI_FALLBACK, true);
  const memo = memoizeAsync(() => attemptHandshake(boot, facts, fallbackOnError), HANDSHAKE_TTL_MS, now);
  return { boot, facts, memo };
}

// --- static config (fixed for the process lifetime; never handshake-derived) -

export interface StaticConfig {
  home?: string;
  timeout_ms: number;
  fallback_on_error: boolean;
}

/**
 * base_url/home/timeout/fallback never come from the server — they gate
 * TRANSPORT, not behavior, so effectiveSetting (env > server > default) does
 * not apply to them; they resolve once, locally, from env alone. Exported for
 * testing.
 */
export function resolveStaticConfig(env: NodeJS.ProcessEnv = process.env): StaticConfig {
  const e = env || {};
  const homeEnv = (e.MEMINI_HOME || "").trim();
  return {
    home: homeEnv || undefined,
    timeout_ms: Number(e.MEMINI_TIMEOUT_MS || DEFAULT_TIMEOUT_MS),
    fallback_on_error: envBool(e.MEMINI_FALLBACK, true),
  };
}

// --- live config (namespace + behavior knobs; varies with the handshake) ----

export interface LiveConfig {
  namespace: string;
  namespace_source: string;
  degraded: boolean;
  recall: boolean;
  capture: boolean;
  recall_limit: number;
  recall_max_tokens: number;
  recall_min_score: number;
  // Windowed injection-cooldown knobs (both dimensions — pi's before_agent_start
  // is per user prompt, so it can drive the prompt counter as well as the clock).
  // 0 disables that dimension; BOTH zero == legacy "suppress forever" (#134).
  inject_cooldown_ms: number;
  inject_cooldown_prompts: number;
  capture_user_max_chars: number;
  capture_assistant_max_chars: number;
}

function knob(wireKey: string) {
  const k = BEHAVIOR_KNOBS.find((b) => b.wireKey === wireKey);
  if (!k) throw new Error(`pi-memini: unknown behavior knob "${wireKey}"`);
  return k;
}

/**
 * Resolve everything that CAN change on a live handshake: the namespace (a
 * successful handshake wins outright; otherwise MEMINI_NAMESPACE, otherwise
 * local git/cwd derivation — resolveNamespace's own precedence, degraded:true
 * on every non-handshake path) and the behavior knobs (env-override > server >
 * built-in default, via effectiveSetting). Pure and synchronous so it's cheap
 * to call on every hook/tool invocation once `hs` is in hand. Exported for
 * testing — this is where namespace + fail-soft precedence live.
 */
export function resolveLiveConfig(
  boot: Bootstrap,
  facts: ProjectFacts,
  hs: HandshakeResult | undefined,
  env: Record<string, string | undefined> = process.env,
): LiveConfig {
  const resolved = resolveNamespace(boot, facts, hs);
  const server = hs?.settings;
  return {
    namespace: resolved.namespace,
    namespace_source: resolved.source,
    degraded: resolved.degraded,
    recall: effectiveSetting<boolean>(knob("recall"), server, env).value,
    capture: effectiveSetting<boolean>(knob("capture"), server, env).value,
    recall_limit: effectiveSetting<number>(knob("recall_limit"), server, env).value,
    recall_max_tokens: effectiveSetting<number>(knob("inject_recall_max_tok"), server, env).value,
    recall_min_score: effectiveSetting<number>(knob("inject_recall_min_score"), server, env).value,
    inject_cooldown_ms: effectiveSetting<number>(knob("inject_cooldown_ms"), server, env).value,
    inject_cooldown_prompts: effectiveSetting<number>(knob("inject_cooldown_prompts"), server, env).value,
    capture_user_max_chars: effectiveSetting<number>(knob("capture_user_max_chars"), server, env).value,
    capture_assistant_max_chars: effectiveSetting<number>(knob("capture_assistant_max_chars"), server, env).value,
  };
}

/** Resolve `ctx`'s current live config, triggering (or reusing) the memoized handshake. */
export async function sessionLive(
  ctx: SessionContext,
  env: NodeJS.ProcessEnv = process.env,
): Promise<LiveConfig> {
  const hs = await ctx.memo.get();
  return resolveLiveConfig(ctx.boot, ctx.facts, hs, env as Record<string, string | undefined>);
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

function plaintextBearerAuthMessage(baseUrl: string): string {
  return `memini: MEMINI_API_KEY is configured for plaintext HTTP to ${baseUrl}. Bearer tokens and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
}

export function createPlaintextBearerAuthGuard(warn: (m: string) => void, env?: NodeJS.ProcessEnv) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl: string, secret?: string): void {
    if (!isPlaintextBearerUnsafe(baseUrl, secret || "")) return;
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
  postJson: (path: string, payload: any, namespace: string) => Promise<any>;
  getJson: (path: string, namespace: string) => Promise<any>;
  deleteJson: (path: string, namespace: string) => Promise<any>;
  // postJsonResult is postJson without the degrade-to-null: it hands back the
  // server's own error text. The explicit write tool uses it, because a rejected
  // write is information the model can act on — a `visibility` naming an unknown
  // ancestor errors listing the valid chain, which is how the model learns the
  // topology. Swallowing that into `success: false` leaves it nothing to correct
  // against. It still never throws.
  postJsonResult: (path: string, payload: any, namespace: string) => Promise<{ ok: boolean; data?: any; error?: string }>;
}

function createClient(staticCfg: StaticConfig, boot: Bootstrap, warn: (m: string) => void): MeminiClient {
  const baseUrl = String(boot.baseUrl).replace(/\/+$/, "");
  const secret = boot.apiKey;
  const guard = createPlaintextBearerAuthGuard(warn);
  if (boot.requireHttps) guard(baseUrl, secret);

  function headers(namespace: string, extra?: Record<string, string>): Record<string, string> {
    const h: Record<string, string> = { "X-Memini-Namespace": namespace, ...(extra || {}) };
    if (secret) h.Authorization = `Bearer ${secret}`;
    if (staticCfg.home) h["X-Memini-Home"] = staticCfg.home;
    return h;
  }

  async function request(method: string, path: string, namespace: string, body?: any): Promise<any> {
    guard(baseUrl, secret);
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method,
        headers: headers(namespace, body ? { "Content-Type": "application/json" } : undefined),
        body: body ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(staticCfg.timeout_ms),
      });
      if (!res.ok) {
        if (staticCfg.fallback_on_error) {
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
      if (!staticCfg.fallback_on_error) throw error;
      warn(`memini: ${String(error)}`);
      return null;
    }
  }

  // See MeminiClient.postJsonResult. Never throws — a failure is reported as
  // {ok:false, error} regardless of fallback_on_error, so a tool call degrades
  // into an answer rather than an exception in the host.
  async function requestResult(
    method: string,
    path: string,
    namespace: string,
    body?: any,
  ): Promise<{ ok: boolean; data?: any; error?: string }> {
    try {
      guard(baseUrl, secret);
      const res = await fetch(`${baseUrl}${path}`, {
        method,
        headers: headers(namespace, body ? { "Content-Type": "application/json" } : undefined),
        body: body ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(staticCfg.timeout_ms),
      });
      if (!res.ok) {
        const detail = (await res.text().catch(() => "")).trim();
        warn(`memini ${method} ${path} failed: ${res.status} ${detail}`);
        return { ok: false, error: detail || `HTTP ${res.status}` };
      }
      return { ok: true, data: await res.json().catch(() => ({})) };
    } catch (error) {
      warn(`memini: ${String(error)}`);
      return { ok: false, error: String(error) };
    }
  }

  return {
    postJson: (path, payload, namespace) => request("POST", path, namespace, payload),
    getJson: (path, namespace) => request("GET", path, namespace),
    deleteJson: (path, namespace) => request("DELETE", path, namespace),
    postJsonResult: (path, payload, namespace) => requestResult("POST", path, namespace, payload),
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
// assistant reply, bounding each side by the server-resolved capture settings.
// Delegates to @memini/client so every integration cuts identically: marked, and
// never through the middle of a character. Exported for testing.
export function buildTurnContent(
  userText: string,
  assistantText: string,
  userMax: number,
  assistantMax: number,
): string {
  return buildTurnCapture(String(userText), String(assistantText), userMax, assistantMax);
}

// --- pins (/memini:namespace) -------------------------------------------------

/** The subset of ProjectFacts that can key a server-side pin. Exported for testing. */
export function pinKeyFacts(facts: ProjectFacts): { remote_url?: string; toplevel_path?: string } {
  const out: { remote_url?: string; toplevel_path?: string } = {};
  if (facts.remote_url) out.remote_url = facts.remote_url;
  if (facts.toplevel_path) out.toplevel_path = facts.toplevel_path;
  return out;
}

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

function offlineMessage(boot: Bootstrap, error: unknown): string {
  const detail = String((error as any)?.message || error);
  return (
    `${detail}\n\nCould not reach the memini server at ${boot.baseUrl}. Pins live on the server, so ` +
    `setting one needs it reachable. For an offline, machine-local override instead, export ` +
    `MEMINI_NAMESPACE=<namespace>.`
  );
}

// --- /memini:status + /memini:namespace --------------------------------------

// statusGet is the diagnostics-only GET: it never degrades into the client's
// warn-and-null path, because status must distinguish "the server said no" from
// "the request never happened". `quiet` is for probes whose failure is a
// legitimate answer rather than a fault — /healthz behind an ingress that routes
// only /v1 and /mcp 404s, which means "not exposed", not "server down".
async function statusGet(
  boot: Bootstrap,
  namespace: string,
  path: string,
  warn: (m: string) => void,
  quiet = false,
): Promise<any> {
  const baseUrl = String(boot.baseUrl).replace(/\/+$/, "");
  const headers: Record<string, string> = { "X-Memini-Namespace": namespace };
  if (boot.apiKey) headers.Authorization = `Bearer ${boot.apiKey}`;
  if (boot.homeEnv) headers["X-Memini-Home"] = boot.homeEnv;
  try {
    const res = await fetch(`${baseUrl}${path}`, {
      method: "GET",
      headers,
      signal: AbortSignal.timeout(STATUS_TIMEOUT_MS),
    });
    if (!res.ok) {
      if (!quiet) warn(`GET ${path} -> ${res.status}`);
      return null;
    }
    return await res.json();
  } catch (error) {
    if (!quiet) warn(`GET ${path} failed: ${String(error)}`);
    return null;
  }
}

interface ServerReport {
  reachable: boolean;
  latencyMs: number;
  readSet?: any;
  version?: string;
  status?: string;
  deps?: any;
  healthExposed?: boolean;
}

// fetchServer probes the server the way the plugin actually uses it.
//
// Reachability is decided by /v1/namespaces/readset, not /healthz: a remote
// memini typically sits behind an ingress that routes only /v1 and /mcp, so
// /healthz 404s while the server is perfectly healthy. The read set doubles as
// the probe — it is the server's own introspection of which namespaces a plain
// recall draws from, so it cannot drift from what recall really does, and status
// needs it anyway.
async function fetchServer(boot: Bootstrap, namespace: string, warn: (m: string) => void): Promise<ServerReport> {
  const started = Date.now();
  const readSet = await statusGet(boot, namespace, "/v1/namespaces/readset", warn);
  const out: ServerReport = {
    reachable: readSet != null,
    latencyMs: Date.now() - started,
    readSet,
  };
  // Dependency detail, when the deployment exposes it. Quiet: a 404 here means
  // "not routed", not "broken".
  const health = await statusGet(boot, namespace, "/healthz?verbose=1", warn, true);
  if (health) {
    out.version = health.version;
    out.status = health.status;
    out.deps = health.deps;
  } else {
    out.healthExposed = false;
  }
  return out;
}

const pad = (s: unknown, n: number) => String(s).padEnd(n);

interface Warning {
  level: "warn" | "note";
  code: string;
  message: string;
  fix?: string;
}

/** Build the warnings section from bootstrap + facts + handshake + live config. Exported for testing. */
export function buildWarnings(ctx: SessionContext, live: LiveConfig, hs: HandshakeResult | undefined): Warning[] {
  const warnings: Warning[] = [];

  if (live.degraded) {
    warnings.push({
      level: "warn",
      code: "degraded-mode",
      message: `could not reach the memini server at ${ctx.boot.baseUrl}: the namespace is local-derived and every setting is a built-in default, not what the server would return.`,
      fix: "Check MEMINI_BASE_URL and that the server is running; recall and capture are both failing until it is reachable.",
    });
  }

  if (ctx.boot.namespaceEnv && hs?.namespace_source !== "pin") {
    warnings.push({
      level: "warn",
      code: "global-namespace-pin",
      message: `MEMINI_NAMESPACE is set to "${ctx.boot.namespaceEnv}", which pins EVERY project on this machine to one namespace (unless this repo has a stronger server-side pin). If it is exported from a shell rc (or a fish universal variable), every repo you work in is sharing one memory pool.`,
      fix: "Set a pin instead: /memini:namespace <ns> (a pin beats the environment).",
    });
  }

  if (!ctx.boot.homeEnv) {
    warnings.push({
      level: "warn",
      code: "home-unset",
      message: 'MEMINI_HOME is unset: there is no personal namespace, so visibility:"personal" writes will error and no personal leg merges into recall.',
      fix: "Export MEMINI_HOME=personal/<you>.",
    });
  }

  if (isPlaintextBearerUnsafe(ctx.boot.baseUrl, ctx.boot.apiKey)) {
    warnings.push({
      level: "warn",
      code: "plaintext-bearer",
      message: `a bearer token is configured for plaintext HTTP to ${ctx.boot.baseUrl}; the token and your memory payloads can be observed on the network.`,
      fix: "Use HTTPS, or tunnel over SSH. Set MEMINI_REQUIRE_HTTPS=1 to make this an error.",
    });
  }

  return warnings;
}

// renderStatus formats the report. Exported for testing — the assertion that
// matters (a token is never printed in full) is on this string.
export function renderStatus(
  ctx: SessionContext,
  staticCfg: StaticConfig,
  live: LiveConfig,
  hs: HandshakeResult | undefined,
  server: ServerReport,
): string {
  const L: string[] = [];

  L.push(`memini — effective settings (pi)`);
  L.push(`cwd: ${ctx.facts.toplevel_path || process.cwd()}`);
  L.push("");

  // Namespace first: it is what people actually come here to find out.
  L.push(`NAMESPACE`);
  L.push(`  ${pad("effective", 28)} ${pad(live.namespace, 34)} <- ${live.namespace_source}`);
  if (live.degraded) {
    const local = deriveLocalNamespace(ctx.facts);
    if (local.namespace !== live.namespace) {
      L.push(`  ${pad("git/cwd would give", 28)} ${pad(local.namespace, 34)} <- local-${local.source}`);
    }
  }
  L.push(`  ${pad("home (personal)", 28)} ${ctx.boot.homeEnv || "(unset)"}`);
  L.push("");

  // Behavior knobs relevant to this extension, with provenance.
  L.push(`SETTINGS`);
  const wireKeys = ["recall", "capture", "recall_limit", "inject_recall_max_tok", "inject_recall_min_score"];
  for (const wireKey of wireKeys) {
    const k = knob(wireKey);
    const { value, source } = effectiveSetting(k, hs?.settings, process.env as Record<string, string | undefined>);
    const origin = source === "env-override" ? "<- env" : source === "server" ? "<- server" : "(default)";
    L.push(`  ${pad(k.envName.replace(/^MEMINI_/, "").toLowerCase(), 28)} ${pad(String(value), 22)} ${origin}`);
  }
  L.push("");

  // This extension's own connection knobs — not server-resolved.
  const secret = ctx.boot.apiKey;
  L.push(`EXTENSION`);
  L.push(`  ${pad("base_url", 28)} ${ctx.boot.baseUrl}`);
  L.push(`  ${pad("timeout_ms", 28)} ${staticCfg.timeout_ms}`);
  L.push(`  ${pad("bearer", 28)} ${secret ? redactValue(secret) : "(none)"}`);
  L.push("");

  L.push(`SERVER`);
  if (!server.reachable) {
    L.push(`  ${pad("reachable", 28)} NO — could not reach ${ctx.boot.baseUrl}`);
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
    L.push(`READ SET for "${live.namespace}" — where a plain recall looks`);
    L.push(`  ${pad("NAMESPACE", 34)} ${pad("ORIGIN", 12)} TIERS`);
    for (const e of server.readSet.entries) {
      const tiers = Array.isArray(e.tiers) && e.tiers.length ? e.tiers.join(",") : "all";
      L.push(`  ${pad(e.namespace, 34)} ${pad(e.origin, 12)} ${tiers}`);
    }
    L.push("");
  }

  const warnings = buildWarnings(ctx, live, hs);
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
 * registerMeminiCommands wires /memini:status and /memini:namespace.
 *
 * The namespace command no longer writes a local, per-machine override file:
 * `<namespace>`/`--clear` now PUT/DELETE a server-side pin (POST /v1/pins per
 * api/openapi.yaml), and drop the in-memory handshake memo afterward so the
 * very next hook/tool call re-resolves against the new pin instead of waiting
 * out the rest of HANDSHAKE_TTL_MS. Showing the current namespace (no args)
 * reuses whatever the memo already holds (live only if it was empty/expired) —
 * a manual pin write is what forces freshness, not every `/memini:status` or
 * bare `/memini:namespace` call.
 *
 * Exported for testing.
 */
export function registerMeminiCommands(
  pi: ExtensionAPI,
  ctx: SessionContext,
  staticCfg: StaticConfig,
  warn: (m: string) => void,
): void {
  const show = (content: string) => {
    // Same channel the recall injection uses: a custom message, displayed, no
    // turn triggered.
    pi.sendMessage({ customType: "memini-status", content, display: true });
  };

  pi.registerCommand("memini:status", {
    description: "Show memini's effective settings: namespace + provenance, connection, server read set",
    handler: async (_args: string, cmdCtx: ExtensionCommandContext) => {
      try {
        const hs = await ctx.memo.get();
        const live = resolveLiveConfig(ctx.boot, ctx.facts, hs, process.env as Record<string, string | undefined>);
        const server = await fetchServer(ctx.boot, live.namespace, warn);
        show(renderStatus(ctx, staticCfg, live, hs, server));
      } catch (error) {
        // A command must never throw into the host.
        cmdCtx.ui.notify(`memini: status failed: ${String(error)}`, "error");
      }
    },
  });

  pi.registerCommand("memini:namespace", {
    description: "Show, set, or --clear the memini namespace pin for this project (server-side)",
    handler: async (args: string, cmdCtx: ExtensionCommandContext) => {
      try {
        const arg = String(args || "").trim();
        const { boot, facts } = ctx;

        if (!arg) {
          const hs = await ctx.memo.get();
          const live = resolveLiveConfig(boot, facts, hs, process.env as Record<string, string | undefined>);
          const out: string[] = [];
          if (!hs) {
            out.push(`namespace: ${live.namespace}  (${live.namespace_source} — server unreachable)`);
            out.push("");
            out.push(`Could not reach ${boot.baseUrl}, so this is a local guess, not the server's authority.`);
            out.push(`A pin (if any) can only be read from the server.`);
            show(out.join("\n"));
            return;
          }
          out.push(`namespace: ${hs.namespace}  (source: ${hs.namespace_source})`);
          if (hs.namespace_source === "pin" && hs.pin) {
            out.push(`pin:       key ${hs.pin.key}`);
            if (hs.pin.created_by) out.push(`           set by ${hs.pin.created_by}`);
            if (hs.pin.updated_at) out.push(`           updated ${hs.pin.updated_at}`);
            if (hs.pin.note) out.push(`           note: ${hs.pin.note}`);
            if (boot.namespaceEnv) {
              out.push("");
              out.push(`MEMINI_NAMESPACE is set to "${boot.namespaceEnv}", but the pin wins — a pin`);
              out.push(`beats the environment on purpose.`);
            }
          } else if (hs.namespace_source === "env") {
            out.push("");
            out.push(`This comes from MEMINI_NAMESPACE, which pins EVERY project on this machine to`);
            out.push(`one namespace. To scope just this project, set a pin: /memini:namespace <ns>`);
            out.push(`(a pin beats the environment).`);
          }
          out.push("");
          out.push(`Set a pin with:    /memini:namespace <namespace>`);
          out.push(`Clear it with:     /memini:namespace --clear`);
          show(out.join("\n"));
          return;
        }

        if (arg === "--clear" || arg === "clear") {
          const keyFacts = pinKeyFacts(facts);
          if (!keyFacts.remote_url && !keyFacts.toplevel_path) {
            cmdCtx.ui.notify(
              `memini: this project has no git remote or toplevel, so it cannot have a pin to clear.`,
              "error",
            );
            return;
          }
          let res;
          try {
            res = await pinsRequest(boot, "DELETE", keyFacts);
          } catch (error) {
            cmdCtx.ui.notify(`memini: ${offlineMessage(boot, error)}`, "error");
            return;
          }
          if (res.status === 404) {
            show(`No pin was set for this project — nothing to clear.`);
            return;
          }
          if (!res.ok) {
            cmdCtx.ui.notify(`memini: could not clear the pin: ${pinErrorMessage(res)}`, "error");
            return;
          }
          ctx.memo.invalidate();
          show(
            [
              `namespace pin cleared — this project resolves automatically again.`,
              ``,
              `Recall and capture use the new resolution from the next turn.`,
            ].join("\n"),
          );
          return;
        }

        const ns = normalizeNamespace(arg);
        const bad = validateNamespace(ns);
        if (bad) {
          // Fail loudly rather than silently normalize into something the user
          // did not ask for — and CR/LF would split the X-Memini-Namespace
          // header outright.
          cmdCtx.ui.notify(`memini: invalid namespace ${JSON.stringify(arg)}: ${bad}`, "error");
          return;
        }
        const keyFacts = pinKeyFacts(facts);
        if (!keyFacts.remote_url && !keyFacts.toplevel_path) {
          cmdCtx.ui.notify(
            `memini: this project has no git remote or toplevel to pin a namespace to. A pin is keyed ` +
              `by the project's git identity; run inside a git repository, or export ` +
              `MEMINI_NAMESPACE=${ns} for a machine-local override.`,
            "error",
          );
          return;
        }
        let res;
        try {
          res = await pinsRequest(boot, "PUT", { namespace: ns, ...keyFacts });
        } catch (error) {
          cmdCtx.ui.notify(`memini: ${offlineMessage(boot, error)}`, "error");
          return;
        }
        if (!res.ok) {
          cmdCtx.ui.notify(`memini: could not set the pin: ${pinErrorMessage(res)}`, "error");
          return;
        }
        ctx.memo.invalidate();
        const entry = res.body || {};
        show(
          [
            `namespace pinned: ${entry.namespace || ns}`,
            `project key:      ${entry.key || keyFacts.remote_url || keyFacts.toplevel_path}`,
            ``,
            `Recall and capture use it from the next turn. The pin lives on the memini server, so it`,
            `follows you across machines and every client resolves the same namespace. It beats`,
            `MEMINI_NAMESPACE.`,
          ].join("\n"),
        );
      } catch (error) {
        cmdCtx.ui.notify(`memini: namespace failed: ${String(error)}`, "error");
      }
    },
  });
}

const TOOL_NAMES = ["memory_recall", "memory_briefing", "memory_list", "memory_remember", "memory_forget"];
const VALID_TIERS = ["working", "episodic", "semantic", "procedural"];
// The LLM-facing semantic scope vocabulary, identical to the MCP server's
// (internal/api/mcp: scopeEnum). The deprecated REST aliases "exact"/"subtree"
// are deliberately NOT offered: the model makes a semantic choice, it does not
// speak the back-compat dialect.
const VALID_SCOPES = ["project", "full", "everywhere"];

// briefingPath builds the GET /v1/namespaces/briefing query string. The endpoint
// is header-scoped (X-Memini-Namespace), so there is no namespace in the path —
// the model never names one. Exported for testing.
export function briefingPath(args: any): string {
  const scope = String(args?.scope || "").trim();
  return VALID_SCOPES.includes(scope)
    ? `/v1/namespaces/briefing?scope=${encodeURIComponent(scope)}`
    : "/v1/namespaces/briefing";
}

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

  const sessionCtx = createSessionContext(process.cwd(), process.env);
  const staticCfg = resolveStaticConfig(process.env);
  const client = createClient(staticCfg, sessionCtx.boot, warn);

  // Best-effort: a host without registerCommand (or a throw inside it) must not
  // cost the extension its recall and capture hooks.
  try {
    if (typeof pi.registerCommand === "function") registerMeminiCommands(pi, sessionCtx, staticCfg, warn);
  } catch (error) {
    warn(`command registration skipped: ${String(error)}`);
  }

  const MAX_TRACKED_SESSIONS = 200;
  // Latest user prompt per session, set in before_agent_start and consumed in
  // agent_end to assemble the full turn. Bounded: a session whose capture
  // never lands would otherwise pin its entry forever.
  const pendingUser = new Map<string, string>();
  const rememberPendingUser = (session: string, prompt: string) => {
    pendingUser.set(session, prompt);
    while (pendingUser.size > MAX_TRACKED_SESSIONS) {
      const oldest = pendingUser.keys().next().value;
      if (oldest === undefined) break;
      pendingUser.delete(oldest);
    }
  };
  // Assistant message ids already captured, so a re-fired agent_end never writes
  // a duplicate turn. Re-fires only concern recent turns, so cap the window
  // instead of growing one entry per turn forever.
  const captured = new Set<string>();
  const MAX_CAPTURED = 200;
  const rememberCaptured = (id: string) => {
    captured.add(id);
    while (captured.size > MAX_CAPTURED) {
      const oldest = captured.values().next().value;
      if (oldest === undefined) break;
      captured.delete(oldest);
    }
  };
  // Memory ids each session has already been shown (mirrors the openclaw
  // plugin): the recall injection is a persistent context message, so
  // re-injecting an unchanged match every turn stacks identical blocks in the
  // prompt. Each entry is stamped with the wall-clock time AND the per-session
  // prompt counter at injection ({at, n}); the windowed cooldown predicate
  // (suppressed) uses them to keep re-serving a still-fresh match out while
  // re-admitting one both windows have moved past. The inner cap keeps a stable
  // session — which never ages out of the outer map — from growing for the
  // process lifetime.
  const injectedBySession = new Map<string, Map<string, { at: number; n: number }>>();
  const MAX_INJECTED_PER_SESSION = 200;
  // Per-session user-prompt counter, bumped once per before_agent_start (pi's
  // per-user-message hook) before any gate — it drives the cooldown's prompt
  // dimension and advances even on turns that inject nothing. Bounded like the
  // maps above so a churn of one-shot sessions can't grow it unbounded.
  const promptCountBySession = new Map<string, number>();
  const bumpPromptCount = (session: string): number => {
    const n = (promptCountBySession.get(session) ?? 0) + 1;
    promptCountBySession.set(session, n);
    while (promptCountBySession.size > MAX_TRACKED_SESSIONS) {
      const oldest = promptCountBySession.keys().next().value;
      if (oldest === undefined) break;
      promptCountBySession.delete(oldest);
    }
    return n;
  };
  const rememberInjected = (session: string, ids: string[]) => {
    let seen = injectedBySession.get(session);
    if (!seen) {
      seen = new Map<string, { at: number; n: number }>();
      injectedBySession.set(session, seen);
      while (injectedBySession.size > MAX_TRACKED_SESSIONS) {
        const oldest = injectedBySession.keys().next().value;
        if (oldest === undefined) break;
        injectedBySession.delete(oldest);
      }
    }
    const now = Date.now();
    const n = promptCountBySession.get(session) ?? 0;
    for (const id of ids) {
      if (!id) continue;
      // delete+set refreshes the {at, n} stamp (a re-served id restarts both
      // windows) and the insertion order (newest last, so the cap evicts the
      // least-recently-shown id first).
      seen.delete(id);
      seen.set(id, { at: now, n });
    }
    while (seen.size > MAX_INJECTED_PER_SESSION) {
      const oldest = seen.keys().next().value;
      if (oldest === undefined) break;
      seen.delete(oldest);
    }
  };
  // The shared windowed-cooldown predicate (design-context.md): an id is
  // suppressed (excluded from recall AND dropped from results) while inside
  // EITHER window, and re-admits only once BOTH have lapsed. Both knobs at 0
  // reproduces the legacy #134 "suppress forever" behavior. counter==0 makes the
  // prompt dimension inert (a host that never advances a counter degrades to
  // time-only rather than "forever"); negative deltas (clock skew / stale
  // counter) compare as inside-window and clamp to suppressed.
  const suppressed = (
    entry: { at: number; n: number },
    now: number,
    counter: number,
    cooldownMs: number,
    cooldownPrompts: number,
  ): boolean => {
    if (cooldownMs === 0 && cooldownPrompts === 0) return true;
    const promptDim = cooldownPrompts > 0 && counter > 0 && counter - entry.n < cooldownPrompts;
    const timeDim = cooldownMs > 0 && now - entry.at < cooldownMs;
    return promptDim || timeDim;
  };
  // The set of a session's already-shown ids still inside the cooldown — the view
  // both exclude_ids and the client-side drop-filter use. Prunes lapsed entries
  // as it reads so a lapsed id is neither excluded nor dropped: it re-serves and
  // its stamp refreshes on the next show.
  const injectedInWindow = (
    session: string,
    counter: number,
    cooldownMs: number,
    cooldownPrompts: number,
  ): Set<string> => {
    const inWindow = new Set<string>();
    const seen = injectedBySession.get(session);
    if (!seen) return inWindow;
    const now = Date.now();
    for (const [id, entry] of seen) {
      if (suppressed(entry, now, counter, cooldownMs, cooldownPrompts)) inWindow.add(id);
      else seen.delete(id);
    }
    return inWindow;
  };
  // /v1/search drops exclude_ids before ranking and the limit, so an
  // already-shown hit frees its slot for the next-best match. Older servers
  // 400 on the unknown field: when a request carrying it fails and the retry
  // without it succeeds, stop sending it. The client-side filter stays.
  let serverExcludeIds = true;
  const searchExcluding = async (body: any, excludeIds: string[]) => {
    if (!serverExcludeIds || excludeIds.length === 0) {
      return client.postJson("/v1/search", body);
    }
    try {
      const result = await client.postJson("/v1/search", { ...body, exclude_ids: excludeIds });
      if (result !== null) return result;
    } catch {
      // With fallback_on_error=false the 400 arrives as a throw, not null.
    }
    const retry = await client.postJson("/v1/search", body);
    if (retry !== null) {
      serverExcludeIds = false;
      warn("memini: server does not accept exclude_ids; using client-side dedupe only");
    }
    return retry;
  };

  // Recall before the turn: search for the user's prompt and inject the matches
  // as a persistent context message. Buffer the prompt for capture at agent_end.
  pi.on("before_agent_start", async (event, ctx) => {
    const sid = sessionIdOf(ctx);
    const query = String(event?.prompt || "").trim();
    if (query && sid) rememberPendingUser(sid, query);
    // Advance the per-session prompt counter once per user turn, BEFORE any gate
    // (the recall-setting gate and the shape gates below), so the cooldown's
    // prompt dimension measures turns-since-injection even on turns that inject
    // nothing. before_agent_start is per user prompt on pi, so this is the
    // literal "X messages" unit.
    const counter = sid ? bumpPromptCount(sid) : 0;

    const live = await sessionLive(sessionCtx);
    if (!live.recall || !query) return;

    const body: any = { query, limit: live.recall_limit };
    // Exclude this session's own captured turns: they're still in live context,
    // so recalling them just echoes the conversation back a turn behind.
    if (sid) body.exclude_metadata = { session_id: sid };
    if (live.recall_min_score > 0) body.min_score = live.recall_min_score;

    // Only ids still inside the injection cooldown go along as exclude_ids, so a
    // suppressed hit doesn't waste a recall_limit slot; a LAPSED id is absent so
    // it re-serves. Computed once and reused for the client-side drop below.
    const inWindow = sid
      ? injectedInWindow(sid, counter, live.inject_cooldown_ms, live.inject_cooldown_prompts)
      : new Set<string>();
    const excludeIds = [...inWindow];
    const result = await searchExcluding(body, excludeIds);
    const floor = live.recall_min_score > 0 ? live.recall_min_score : 0;
    let rawHits = Array.isArray(result?.results) ? result.results : [];
    // Suppress memories still inside this session's cooldown — the injected
    // message persists in context, so a repeat adds nothing but noise. A lapsed
    // id is not in inWindow, so it passes through, re-serves, and re-stamps.
    if (inWindow.size) rawHits = rawHits.filter((r: any) => !inWindow.has(r?.memory?.id));
    const filtered =
      floor > 0
        ? rawHits.filter((r: any) => (typeof r?.score === "number" ? r.score : 0) >= floor)
        : rawHits;
    const hits = formatResults(filtered, live.recall_limit, labelsEnv());
    if (hits.length === 0) return;

    const fit = fitByTokens(hits, live.recall_max_tokens);
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
    const live = await sessionLive(sessionCtx);
    if (!live.capture) return;
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

    const stored = await client.postJson(
      "/v1/memories",
      {
        content: buildTurnContent(userText, assistantText, live.capture_user_max_chars, live.capture_assistant_max_chars),
        tags: ["pi"],
        metadata,
      },
      live.namespace,
    );
    if (stored !== null) {
      if (dedupKey) rememberCaptured(dedupKey);
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

  pi.registerTool({
    name: "memory_recall",
    label: "Recall memory",
    description:
      "Search prior context in long-term memory (memini) via hybrid (semantic + keyword) retrieval, ranked " +
      "by relevance, recency, and corroboration. Call BEFORE starting work that may have history: editing " +
      "an unfamiliar file, debugging a recurring issue, making a non-obvious decision, or when asked what's " +
      "known about something. Prefer a short descriptive query ('JWT auth setup'). scope picks how wide to " +
      "read: 'project' (just this project), 'full' (default: project plus inherited ancestor/personal/link " +
      "context), or 'everywhere' (full plus nested sub-projects). Each result's namespace/from fields are " +
      "provenance, not a choice — an absent 'from' means this project's own memory, otherwise it names the " +
      "ancestor or personal namespace the memory came from; read them to learn where knowledge lives, never " +
      "construct a namespace path. Empty results mean nothing is known — proceed from first principles, " +
      "never invent a remembered fact. A degraded:\"keyword_only\" field in the result means semantic " +
      "search was unavailable and results came from keyword matching alone — treat as incomplete, not " +
      "exhaustive.",
    parameters: Type.Object({
      query: Type.String({ description: "What to search for" }),
      limit: Type.Optional(Type.Number({ description: "Max results (default 3)" })),
      tags: Tags,
      metadata: Metadata,
      scope: Scope,
    }),
    async execute(_toolCallId: string, params: any) {
      const live = await sessionLive(sessionCtx);
      const body: any = { query: params.query, limit: params.limit || DEFAULT_RECALL_LIMIT };
      if (params.tags?.length) body.tags = params.tags;
      if (params.metadata && Object.keys(params.metadata).length) body.metadata = params.metadata;
      // An unrecognized scope is dropped rather than forwarded: /v1/search 400s
      // on one, and a hallucinated value must not turn a recall into an error.
      if (VALID_SCOPES.includes(params.scope)) body.scope = params.scope;
      const res = await client.postJson("/v1/search", body, live.namespace);
      const results = (res?.results || []).map((r: any) => {
        const mem = r?.memory || {};
        const out: any = {
          id: mem.id || "",
          content: mem.content || "",
          summary: mem.summary || "",
          tier: mem.tier || "",
          score: typeof r?.score === "number" ? r.score : 0,
        };
        // Read provenance: which namespace the hit lives in, and (for a hit off
        // an ancestor/home/link leg) which leg it came from. Omitted when empty
        // so a project-only recall carries no "from" noise at all.
        if (mem.namespace) out.namespace = mem.namespace;
        if (r?.from) out.from = r.from;
        return out;
      });
      // /v1/search already carries `degraded`/`note` on `res`; pass them through
      // rather than dropping them silently.
      return text(res?.degraded ? { results, degraded: res.degraded, note: res.note } : { results });
    },
  });

  pi.registerTool({
    name: "memory_briefing",
    label: "Session briefing",
    description:
      "Layered session-start briefing for this project from long-term memory (memini) — pinned context, " +
      "durable facts, how-to procedures, and recent activity — in one query-less call. Call it when a " +
      "session opens to orient yourself; prefer it over broad recall queries at session start. The " +
      "scope_header line ('Scope: acme/phoenix/api ← acme/phoenix(3) ← acme(4) ← personal(2)') spells out " +
      "the ancestor chain you inherit from — read it instead of guessing namespace paths, and name one of " +
      "those ancestors as memory_remember's visibility to share a fact up that chain. scope='everywhere' " +
      "also briefs nested sub-projects.",
    parameters: Type.Object({ scope: Scope }),
    async execute(_toolCallId: string, params: any) {
      const live = await sessionLive(sessionCtx);
      const res = await client.getJson(briefingPath(params), live.namespace);
      if (!res) return text({ briefing: null, error: "memini unavailable" });
      const section = (items: any[]) =>
        (items || []).map((b: any) => {
          const mem = b?.memory || {};
          const out: any = { id: mem.id || "", content: mem.content || "", tier: mem.tier || "" };
          if (mem.namespace) out.namespace = mem.namespace;
          if (b?.from) out.from = b.from;
          return out;
        });
      return text({
        namespace: res.namespace || "",
        scope_header: res.scope_header || "",
        pinned: section(res.pinned),
        facts: section(res.facts),
        procedures: section(res.procedures),
        recent: section(res.recent),
      });
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
      const live = await sessionLive(sessionCtx);
      const args = { ...params, limit: params.limit ?? 20 };
      const res = await client.getJson(meminiListPath(args), live.namespace);
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
    // Save-policy invariants are canonical in internal/api/mcp/mcp.go serverInstructions.
    description:
      "Store a fact, decision, preference, or event for later recall. Do not wait to be asked — call " +
      "this the moment you learn: a decision and why it was made, a bug's root cause, a project " +
      "convention, a stated user preference, a correction from the user (a correction IS a durable " +
      "preference), an environment or tool quirk, or a non-obvious command/workflow. When the user " +
      "says 'remember this', 'note that', 'don't forget', 'going forward...', or corrects you, call " +
      "this tool FIRST, then acknowledge — and on an explicit request save unconditionally, even if it " +
      "seems trivial or already stored; secrets and credentials are the one exception. Keep " +
      "memories atomic — one self-contained fact per call; " +
      "search works better on small records. Do NOT store secrets or credentials, transient session " +
      "state, task progress, or facts already in project docs/CLAUDE.md or trivially recoverable from " +
      "code. To correct an existing memory, pass its id — the write updates it in place. If a stored " +
      "memory proves wrong or outdated, fix it immediately: re-save the corrected fact with the " +
      "existing id, or delete it with memory_forget if it should not exist — never leave a " +
      "known-incorrect memory in place. visibility decides who should know: 'project' (default) keeps " +
      "it here; 'personal' follows the user everywhere; or name an ancestor from the memory_briefing " +
      "Scope line to share it up that chain. reinforced=true in the result means the fact was ALREADY " +
      "KNOWN: no new memory was created, the existing one was strengthened, and `id` names that " +
      "pre-existing memory rather than anything you just wrote — do not report it to the user as a new save.",
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
            "follows them everywhere), or an ancestor namespace name read off the memory_briefing Scope line " +
            "(e.g. the team or org level) to share it up that chain. On a durable write an unrecognized name " +
            "errors listing the valid options. Episodic/working writes always stay in the project regardless.",
        }),
      ),
    }),
    async execute(_toolCallId: string, params: any) {
      const live = await sessionLive(sessionCtx);
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
      const res = await client.postJsonResult("/v1/memories", body, live.namespace);
      if (!res.ok) return text({ id: null, success: false, error: res.error });
      const out: any = { id: res.data?.id || null, success: true };
      // reinforced: the fact was already known, nothing new was written, and id
      // names the pre-existing memory. Dropping the flag here would let the model
      // report a no-op as a fresh save.
      if (res.data?.reinforced) out.reinforced = true;
      return text(out);
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
      const live = await sessionLive(sessionCtx);
      if (!params.id) return text({ forgotten: false, error: "id is required" });
      const res = await client.deleteJson(`/v1/memories/${encodeURIComponent(params.id)}`, live.namespace);
      return text({ forgotten: res != null });
    },
  });

  void TOOL_NAMES;
}
