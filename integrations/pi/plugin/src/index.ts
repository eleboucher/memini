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

import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { resolveNamespace } from "@memini/namespace-resolver";
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
const DEFAULT_TIMEOUT_MS = 30000;
const DEFAULT_RECALL_LIMIT = 3;
const DEFAULT_NAMESPACE = "pi";
// The status probes are diagnostics, not the hot path: fail fast rather than
// hang a slash command behind the 30s request timeout.
const STATUS_TIMEOUT_MS = 4000;
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

export interface NamespaceResolution {
  namespace: string;
  source: NamespaceSource;
}

// The shared resolver reports "git"; the client's provenance vocabulary
// distinguishes the remote from the toplevel, and the resolver prefers the
// remote. Every other value is already common to both.
function resolverSource(source: string): NamespaceSource {
  return source === "git" ? "git-remote" : (source as NamespaceSource);
}

/**
 * resolveProjectNamespace resolves the namespace the way *this* harness does,
 * with provenance: override > MEMINI_NAMESPACE > config/git/cwd (the shared
 * resolver) > the "pi" default.
 *
 * The override sits ABOVE the env var on purpose. A globally exported
 * MEMINI_NAMESPACE (a shell rc, or a fish universal variable) pins every repo on
 * the machine to one namespace; if the env beat the override, /memini:namespace
 * would silently do nothing on exactly the machines that most need it.
 *
 * `opts.ignoreOverride` is what lets describeSettings ask the counterfactuals
 * ("what would this be without the override?"). The override lives in a file, so
 * no amount of env-doctoring would strip it — hence the explicit flag. Exported
 * for testing.
 */
export function resolveProjectNamespace(
  env: NodeJS.ProcessEnv,
  cwd?: string,
  opts: ResolveOpts = {},
): NamespaceResolution {
  const e = (env || {}) as Record<string, string | undefined>;

  if (!opts.ignoreOverride && cwd) {
    const override = readOverride(cwd, { env: e });
    if (override) return { namespace: override.namespace, source: "override" };
  }

  const nsEnv = (e.MEMINI_NAMESPACE || "").trim();
  if (nsEnv) {
    // MEMINI_NAMESPACE is used raw-trimmed (the server validates the header).
    // Routing it through sanitizeNamespacePath would alter an explicit value —
    // the canonical resolver returns it untouched.
    return { namespace: nsEnv, source: "env" };
  }

  if (cwd) {
    const { namespace: resolvedNs, source } = resolveNamespace({
      cwd,
      env: e as Record<string, string>,
      integration: "pi",
    });
    // Per-segment sanitize: resolver output may be a tenant path (work/memini).
    const sanitized = sanitizeNamespacePath(resolvedNs);
    if (sanitized) return { namespace: sanitized, source: resolverSource(source) };
  }

  // No override, no explicit namespace, and no cwd to resolve against.
  return { namespace: DEFAULT_NAMESPACE, source: "default" };
}

// resolveConfig builds the config from env vars (Claude Code plugin style),
// deriving the namespace from the override / env / cwd chain above. Exported for
// testing.
export function resolveConfig(env: NodeJS.ProcessEnv, cwd?: string): ResolvedConfig {
  const e = env || {};
  const { namespace } = resolveProjectNamespace(e, cwd);
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
    // namespace is already resolved above (verbatim on the override/env paths,
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

// --- /memini:status + /memini:namespace --------------------------------------

// statusGet is the diagnostics-only GET: it never degrades into the client's
// warn-and-null path, because status must distinguish "the server said no" from
// "the request never happened". `quiet` is for probes whose failure is a
// legitimate answer rather than a fault — /healthz behind an ingress that routes
// only /v1 and /mcp 404s, which means "not exposed", not "server down".
async function statusGet(
  cfg: ResolvedConfig,
  namespace: string,
  path: string,
  warn: (m: string) => void,
  quiet = false,
): Promise<any> {
  const baseUrl = String(cfg.base_url).replace(/\/+$/, "");
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
// Reachability is decided by /v1/namespaces/read-set, not /healthz: a remote
// memini typically sits behind an ingress that routes only /v1 and /mcp, so
// /healthz 404s while the server is perfectly healthy. The read set doubles as
// the probe — it is the server's own introspection of which namespaces a plain
// recall draws from, so it cannot drift from what recall really does, and status
// needs it anyway.
async function fetchServer(cfg: ResolvedConfig, namespace: string, warn: (m: string) => void): Promise<ServerReport> {
  const started = Date.now();
  const readSet = await statusGet(cfg, namespace, "/v1/namespaces/read-set", warn);
  const out: ServerReport = {
    reachable: readSet != null,
    latencyMs: Date.now() - started,
    readSet,
  };
  // Dependency detail, when the deployment exposes it. Quiet: a 404 here means
  // "not routed", not "broken".
  const health = await statusGet(cfg, namespace, "/healthz?verbose=1", warn, true);
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

// renderStatus formats the report. Exported for testing — the assertion that
// matters (a token is never printed in full) is on this string.
export function renderStatus(settings: any, cfg: ResolvedConfig, server: ServerReport): string {
  const ns = settings.namespace;
  const L: string[] = [];

  L.push(`memini — effective settings (pi)`);
  L.push(`cwd: ${settings.cwd}`);
  L.push("");

  // Namespace first: it is what people actually come here to find out.
  L.push(`NAMESPACE`);
  L.push(`  ${pad("effective", 28)} ${pad(ns.effective, 34)} <- ${ns.source}`);
  if (ns.override) {
    L.push(
      `  ${pad("without the override", 28)} ${pad(ns.withoutOverride.namespace, 34)} <- ${ns.withoutOverride.source}`,
    );
  }
  if (ns.derived.namespace !== ns.effective) {
    L.push(`  ${pad("git/cwd would give", 28)} ${pad(ns.derived.namespace, 34)} <- ${ns.derived.source}`);
  }
  L.push(`  ${pad("home (personal)", 28)} ${ns.home || "(unset)"}`);
  L.push("");

  // Connection + namespace inputs, from the shared knob table (already redacted).
  // The capture/injection knobs the table also carries are the Claude Code hooks'
  // — listing them here would imply this extension honors them, and it does not.
  const groups: [string, string[]][] = [
    ["CONNECTION", ["MEMINI_BASE_URL", "MEMINI_API_KEY", "MEMINI_REQUIRE_HTTPS"]],
    ["NAMESPACE INPUTS", ["MEMINI_NAMESPACE", "MEMINI_NAMESPACE_SCOPE", "MEMINI_AGENT", "MEMINI_HOME"]],
  ];
  for (const [group, names] of groups) {
    const rows = (settings.settings || []).filter((s: any) => names.includes(s.name));
    if (!rows.length) continue;
    L.push(group);
    for (const r of rows) {
      const origin = r.source === "env" ? `<- env` : `(default)`;
      L.push(`  ${pad(r.name.replace(/^MEMINI_/, "").toLowerCase(), 28)} ${pad(r.value, 34)} ${origin}`);
    }
    L.push("");
  }

  // This extension's own knobs: they are not in the shared table (the hooks do
  // not have them), and "recall is off" is exactly the kind of finding a status
  // command exists to surface. The bearer is the one the requests actually
  // carry — redacted, since a settings dump is the likeliest place a token gets
  // pasted into an issue.
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN || "";
  L.push(`EXTENSION`);
  L.push(`  ${pad("recall", 28)} ${cfg.recall ? "on" : "off"}`);
  L.push(`  ${pad("capture", 28)} ${cfg.capture ? "on" : "off"}`);
  L.push(`  ${pad("recall_limit", 28)} ${cfg.recall_limit}`);
  L.push(`  ${pad("timeout_ms", 28)} ${cfg.timeout_ms}`);
  L.push(`  ${pad("bearer", 28)} ${secret ? redactValue(secret) : "(none)"}`);
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
    L.push(`READ SET for "${ns.effective}" — where a plain recall looks`);
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
 * registerMeminiCommands wires /memini:status and /memini:namespace.
 *
 * `cfg` is mutated in place on a namespace change rather than re-created: the
 * REST client reads cfg.namespace on every request, so an override applies to
 * the very next recall/capture instead of waiting for a pi restart. A command
 * that appears to succeed and then does nothing until you restart is worse than
 * no command at all.
 *
 * Exported for testing.
 */
export function registerMeminiCommands(pi: ExtensionAPI, cfg: ResolvedConfig, warn: (m: string) => void): void {
  const show = (content: string) => {
    // Same channel the recall injection uses: a custom message, displayed, no
    // turn triggered.
    pi.sendMessage({ customType: "memini-status", content, display: true });
  };

  pi.registerCommand("memini:status", {
    description: "Show memini's effective settings: namespace + provenance, connection, server read set",
    handler: async (_args: string, ctx: ExtensionCommandContext) => {
      try {
        const cwd = ctx.cwd || process.cwd();
        const settings = describeSettings({
          cwd,
          env: process.env as Record<string, string | undefined>,
          // Hand describeSettings THIS harness's resolver, so what it reports is
          // what the extension actually does. The opts pass-through carries
          // ignoreOverride, which is how the counterfactual lines see past an
          // override (it lives in a file, so no env-doctoring would remove it).
          resolve: (env, o) => resolveProjectNamespace(env as NodeJS.ProcessEnv, cwd, o),
        });
        const server = await fetchServer(cfg, settings.namespace.effective, warn);
        show(renderStatus(settings, cfg, server));
      } catch (error) {
        // A command must never throw into the host.
        ctx.ui.notify(`memini: status failed: ${String(error)}`, "error");
      }
    },
  });

  pi.registerCommand("memini:namespace", {
    description: "Show, set, or --clear the memini namespace override for this project",
    handler: async (args: string, ctx: ExtensionCommandContext) => {
      try {
        const cwd = ctx.cwd || process.cwd();
        const arg = String(args || "").trim();
        const before = resolveProjectNamespace(process.env, cwd);

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
            out.push(`No override — resolving automatically.`);
            out.push(`Set one with:  /memini:namespace <namespace>`);
          }
          out.push(`Overrides file: ${defaultOverridesPath(process.env)}`);
          show(out.join("\n"));
          return;
        }

        if (arg === "--clear" || arg === "clear") {
          const removed = clearOverride(cwd, { env: process.env });
          if (!removed) {
            show(`No override was set for ${overrideKey(cwd)} — nothing to clear.`);
            return;
          }
          const after = resolveProjectNamespace(process.env, cwd);
          cfg.namespace = after.namespace;
          show(
            [
              `namespace override cleared: ${before.namespace} -> ${after.namespace}  (source: ${after.source})`,
              ``,
              `Recall and capture use the new namespace from the next turn.`,
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
          ctx.ui.notify(`memini: invalid namespace ${JSON.stringify(arg)}: ${bad}`, "error");
          return;
        }
        writeOverride(cwd, ns, { env: process.env });
        const after = resolveProjectNamespace(process.env, cwd);
        cfg.namespace = after.namespace;
        show(
          [
            `namespace override set: ${before.namespace} -> ${after.namespace}`,
            `project: ${overrideKey(cwd)}`,
            ``,
            `The override wins over MEMINI_NAMESPACE. Recall and capture use it from the next turn.`,
          ].join("\n"),
        );
      } catch (error) {
        ctx.ui.notify(`memini: namespace failed: ${String(error)}`, "error");
      }
    },
  });
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

  // Best-effort: a host without registerCommand (or a throw inside it) must not
  // cost the extension its recall and capture hooks.
  try {
    if (typeof pi.registerCommand === "function") registerMeminiCommands(pi, cfg, warn);
  } catch (error) {
    warn(`command registration skipped: ${String(error)}`);
  }

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
