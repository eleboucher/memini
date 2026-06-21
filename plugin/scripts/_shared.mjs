// Shared utilities for the memini plugin's hook scripts.
//
// Mirrors the layout of agentmemory's plugin/scripts/_shared.mjs:
//   - resolveProject:   env > git remote origin > git toplevel basename > cwd basename
//   - readStdin:        drain stdin to a UTF-8 string
//   - jsonRequest:      POST JSON with bearer-token + namespace headers
//   - postSearch:       POST /v1/search and return result.memory[] of {content,score}
//   - postRemember:     POST /v1/memories
//   - postSupersede:    POST /v1/memories/{id}/supersede (tombstone)
//   - debug:            gated by MEMINI_DEBUG=1
//
// Hooks are .mjs (not .ts) so they run in plain `node` without a build step.

import { execSync } from "node:child_process";
import { basename, join } from "node:path";
import { homedir, tmpdir } from "node:os";
import fs from "node:fs";

export const DEBUG = process.env["MEMINI_DEBUG"] === "1";

/**
 * Split a git remote URL into its path segments (owner, repo, ...).
 * Handles ssh://, https://, and scp-style URLs; strips a trailing .git.
 * Returns [] on any parse error.
 */
function remotePathSegments(url) {
  if (typeof url !== "string" || !url) return [];
  const cleaned = url.trim().replace(/\/+$/, "").replace(/\.git$/i, "");
  if (!cleaned) return [];
  const scpMatch = cleaned.match(/^[^/:]+:[^/]/);
  const path = scpMatch ? cleaned.slice(scpMatch[0].indexOf(":") + 1) : cleaned;
  return path.split("/").filter(Boolean);
}

/**
 * Extract the repo name (last path segment) from a git remote URL.
 * Returns the basename, or null on any parse error.
 */
export function repoNameFromRemote(url) {
  const segs = remotePathSegments(url);
  return segs.length ? segs[segs.length - 1] : null;
}

/**
 * Extract an "owner-repo" slug (last two path segments joined with "-") from a
 * git remote URL, so same-named repos under different owners don't collide
 * (alice/app vs bob/app -> "alice-app" vs "bob-app"). Falls back to the bare
 * repo name when only one segment is present. Returns null on parse error.
 * Used only when MEMINI_NAMESPACE_SCOPE=owner-repo (see resolveProjectBase).
 */
export function repoSlugFromRemote(url) {
  const segs = remotePathSegments(url);
  if (!segs.length) return null;
  if (segs.length === 1) return segs[0];
  const owner = segs[segs.length - 2].replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  const repo = segs[segs.length - 1];
  return owner ? `${owner}-${repo}` : repo;
}

/**
 * Resolve the project namespace for a hook invocation.
 * Order: MEMINI_NAMESPACE env > git remote origin > git toplevel basename > cwd basename.
 * The remote wins over the toplevel so worktrees and /tmp clones get a
 * stable, canonical name.
 */
export function resolveProject(cwd) {
  return withAgent(resolveProjectBase(cwd));
}

// withAgent nests the project namespace under a per-agent segment when
// MEMINI_AGENT is set ("myproject" -> "myproject/reviewer"), so several agents
// sharing a repo keep private memory. Recall with scope=subtree on the project
// reads across all of them. Unset (the default) leaves the namespace untouched.
function withAgent(ns) {
  const agent = (process.env["MEMINI_AGENT"] || "").trim();
  if (!agent) return ns;
  const seg = agent.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  return seg ? `${ns}/${seg}` : ns;
}

// gitOut runs a git command in dir and returns its trimmed stdout, or "" on
// any error (not a repo, git missing, timeout). Best-effort — never throws.
function gitOut(args, dir) {
  try {
    return execSync(`git ${args}`, {
      cwd: dir,
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 500,
    })
      .toString()
      .trim();
  } catch {
    return "";
  }
}

function resolveProjectBase(cwd) {
  const nsEnv = process.env["MEMINI_NAMESPACE"];
  if (nsEnv && nsEnv.trim()) return nsEnv.trim();
  const dir = cwd && cwd.trim() ? cwd : process.cwd();

  const remote = gitOut("remote get-url origin", dir);
  const toplevel = gitOut("rev-parse --show-toplevel", dir);

  // Self-heal: a repo's identity is its remote URL (stable across folder moves
  // and clones) and its toplevel path (stable when the remote is later removed
  // or renamed). If either key has a remembered namespace, reuse it so a move
  // or a dropped remote never silently orphans a project's memory. The path key
  // is intentionally sticky: deleting a repo and cloning a *different* one into
  // the exact same directory inherits the old namespace until the map is cleared
  // (a rare case; set MEMINI_NAMESPACE to override).
  const ownerRepo = (process.env["MEMINI_NAMESPACE_SCOPE"] || "").trim() === "owner-repo";
  const remoteKey = remote ? "remote:" + normalizeRemote(remote) : null;
  const pathKey = toplevel ? "path:" + toplevel : null;
  const map = readProjectMap();
  const cached = (remoteKey && map[remoteKey]) || (pathKey && map[pathKey]);
  if (cached) return cached;

  // Derive a fresh namespace. owner-repo disambiguates same-named repos across
  // owners; the default keeps the bare repo name for backward compatibility.
  let ns = "";
  if (remote) ns = (ownerRepo ? repoSlugFromRemote(remote) : repoNameFromRemote(remote)) || "";
  if (!ns && toplevel) ns = basename(toplevel);
  if (!ns) ns = basename(dir);

  // Remember the derivation under every stable key we have, so a later move or
  // remote change resolves back to this same namespace.
  const entries = {};
  if (remoteKey) entries[remoteKey] = ns;
  if (pathKey) entries[pathKey] = ns;
  if (remoteKey || pathKey) writeProjectMap({ ...map, ...entries });
  return ns;
}

/** Normalize a remote URL into a stable map key: trim, drop trailing slashes
 * and a .git suffix, lowercase. Same checkout uses one consistent remote, so
 * light normalization is enough to survive trivial formatting differences. */
function normalizeRemote(url) {
  return String(url).trim().replace(/\/+$/, "").replace(/\.git$/i, "").toLowerCase();
}

/** Path of the self-healing project→namespace map. */
function projectMapPath() {
  return join(meminiCacheDir(), "project-map.json");
}

/** Read the project map ({} on any error — the map is a best-effort cache). */
export function readProjectMap() {
  try {
    const obj = parseJSON(fs.readFileSync(projectMapPath(), "utf8"));
    return obj && typeof obj === "object" ? obj : {};
  } catch {
    return {};
  }
}

/** Persist the project map (best-effort; failures are swallowed). */
function writeProjectMap(map) {
  try {
    fs.mkdirSync(meminiCacheDir(), { recursive: true });
    fs.writeFileSync(projectMapPath(), JSON.stringify(map));
  } catch (e) {
    if (DEBUG) console.error("[memini] writeProjectMap failed:", e?.message || e);
  }
}

/** Drain stdin to a UTF-8 string. */
export async function readStdin() {
  let s = "";
  for await (const chunk of process.stdin) s += chunk;
  return s;
}

/**
 * Safe JSON.parse that returns null on failure.
 * Hooks must not crash the agent because of malformed input.
 */
export function parseJSON(s) {
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

const REST_URL = process.env["MEMINI_BASE_URL"] || process.env["MEMINI_URL"] || "http://localhost:8080";
const SECRET = process.env["MEMINI_API_KEY"] || process.env["MEMINI_TOKEN"] || "";

const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

function normalizedHostname(hostname) {
  return hostname.replace(/^\[|\]$/g, "").toLowerCase();
}

// usesPlaintextBearerAuth is true when a bearer token would be sent over
// plaintext HTTP to a non-loopback host — i.e. the token (and memory payloads)
// would be observable on the network.
function usesPlaintextBearerAuth(baseUrl, secret) {
  if (!secret) return false;
  try {
    const parsed = new URL(baseUrl);
    return parsed.protocol === "http:" && !LOOPBACK_HOSTS.has(normalizedHostname(parsed.hostname));
  } catch {
    return false;
  }
}

/**
 * createPlaintextBearerAuthGuard mirrors the OpenClaw/OpenCode plugins: it
 * refuses (when MEMINI_REQUIRE_HTTPS=1) or warns once when a bearer token would
 * travel over plaintext HTTP to a non-loopback host. Exported for testing.
 */
export function createPlaintextBearerAuthGuard(warn, env) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl, secret) {
    if (!usesPlaintextBearerAuth(baseUrl, secret)) return;
    const message =
      `memini: a bearer token is configured for plaintext HTTP to ${baseUrl}. ` +
      `The token and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
    if ((env || process.env).MEMINI_REQUIRE_HTTPS === "1") throw new Error(message);
    if (!warned) {
      warned = true;
      warn(message);
    }
  };
}

const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m) => console.error(`[memini] ${m}`));

function authHeaders(extra) {
  const h = { "Content-Type": "application/json", ...(extra || {}) };
  if (SECRET) h["Authorization"] = `Bearer ${SECRET}`;
  return h;
}

/**
 * POST JSON to memini. `namespace` is sent as X-Memini-Namespace. Returns
 * parsed JSON on 2xx, null otherwise. Never throws — hooks must not crash
 * the agent.
 */
export async function postJSON(path, body, namespace, timeoutMs = 5000) {
  try {
    guardPlaintextBearerAuth(REST_URL, SECRET);
    const res = await fetch(`${REST_URL}${path}`, {
      method: "POST",
      headers: authHeaders({ "X-Memini-Namespace": namespace }),
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok) {
      if (DEBUG) {
        const t = await res.text().catch(() => "");
        console.error(`[memini] POST ${path} -> ${res.status}: ${t.slice(0, 200)}`);
      }
      return null;
    }
    return await res.json();
  } catch (e) {
    if (DEBUG) console.error(`[memini] POST ${path} failed:`, e?.message || e);
    return null;
  }
}

/**
 * POST /v1/search and return an array of {content, score, memory} objects.
 * Returns [] on failure.
 *
 * `minScore` (>= 0) sets a per-call relevance floor: candidates whose fused
 * score is below it are dropped server-side. 0 / unset falls back to the
 * server-wide default gate (MEMINI_RECALL_MIN_SCORE). When `minScore` is set,
 * the hook also filters client-side as a belt-and-braces guard against
 * score-fusion edge cases where the server's normalization disagrees with
 * what the caller wants.
 */
export async function postSearch(query, namespace, { limit = 5, tiers, exclude, minScore } = {}) {
  const body = { query, limit };
  if (tiers) body.tiers = tiers;
  if (typeof minScore === "number" && minScore > 0) body.min_score = minScore;
  // exclude drops memories carrying any of these metadata key=value pairs, e.g.
  // {session_id} so a session's own captured digests aren't recalled back at it
  // while still in the live context.
  if (exclude && Object.keys(exclude).length) body.exclude_metadata = exclude;
  const res = await postJSON("/v1/search", body, namespace);
  if (!res || !Array.isArray(res.results)) return [];
  const floor = typeof minScore === "number" && minScore > 0 ? minScore : 0;
  return res.results
    .map((r) => ({
      content: r?.memory?.content || "",
      summary: r?.memory?.summary || "",
      score: typeof r?.score === "number" ? r.score : 0,
      memory: r?.memory || null,
      tier: r?.memory?.tier || "",
    }))
    .filter((r) => r.score >= floor);
}

/**
 * GET JSON from memini. `namespace` is sent as X-Memini-Namespace. Returns
 * parsed JSON on 2xx, null otherwise. Never throws.
 */
export async function getJSON(path, namespace, timeoutMs = 5000) {
  try {
    guardPlaintextBearerAuth(REST_URL, SECRET);
    const res = await fetch(`${REST_URL}${path}`, {
      method: "GET",
      headers: authHeaders({ "X-Memini-Namespace": namespace }),
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok) {
      if (DEBUG) console.error(`[memini] GET ${path} -> ${res.status}`);
      return null;
    }
    return await res.json();
  } catch (e) {
    if (DEBUG) console.error(`[memini] GET ${path} failed:`, e?.message || e);
    return null;
  }
}

/**
 * GET /v1/namespaces/{ns}/briefing — a layered session-start summary
 * {namespace, facts, procedures, recent, pinned}. Returns null on failure.
 *
 * `opts` controls per-section caps. Each field is an int; omit (or 0) to use
 * the server default (5). Pass an explicit 0 to disable that section (REST
 * only — MCP can't distinguish omitted from zero).
 */
export async function getBriefing(namespace, opts = {}) {
  const enc = encodeURIComponent(namespace);
  const params = new URLSearchParams();
  // A "per_section_default" opt acts as the catch-all; per-section fields
  // win when set. This matches the REST contract exactly.
  const fallback = opts.per_section_default ?? opts.perSection ?? 5;
  const pick = (v) => (Number.isInteger(v) && v > 0 ? v : fallback);
  params.set("per_section", String(fallback));
  if (Number.isInteger(opts.per_section_pinned) && opts.per_section_pinned >= 0) {
    params.set("per_section_pinned", String(opts.per_section_pinned));
  } else if (Number.isInteger(opts.pinned) && opts.pinned >= 0) {
    params.set("per_section_pinned", String(opts.pinned));
  } else {
    params.set("per_section_pinned", String(pick(opts.pinned)));
  }
  const setIf = (key, v) => {
    if (Number.isInteger(v) && v >= 0) params.set(key, String(v));
  };
  setIf("per_section_facts", opts.per_section_facts ?? opts.facts);
  setIf("per_section_procedures", opts.per_section_procedures ?? opts.procedures);
  setIf("per_section_recent", opts.per_section_recent ?? opts.recent);
  return getJSON(`/v1/namespaces/${enc}/briefing?${params.toString()}`, namespace);
}

// --- Injection budget ----------------------------------------------------
//
// Per-hook env knobs let an operator shrink auto-injected context without
// changing code. Defaults match today's behavior; the existing tests +
// callers keep working unchanged. New knobs:
//
//   MEMINI_INJECT_BRIEFING_*        per-section caps for SessionStart
//   MEMINI_INJECT_BRIEFING_MAX_TOK  hard ceiling on briefing injection tokens
//   MEMINI_INJECT_PRETOOL_ITEMS     max items per file in PreToolUse
//   MEMINI_INJECT_PRETOOL_MAX_TOK   hard ceiling on per-tool injection tokens
//   MEMINI_INJECT_PRETOOL_MIN_SCORE floor on the fused score (>=)
//   MEMINI_INJECT_PRETOOL_TOOLS     pipe-separated tool allowlist
//   MEMINI_INJECT_LABELS            comma-separated label toggles: tier,
//                                   confidence, age, reason

/**
 * intEnv parses a positive integer env var (>= 0) and returns `default` when
 * unset or malformed. A negative value also falls back — env values are user
 * input and shouldn't crash a hook.
 */
export function intEnv(name, defaultValue) {
  const raw = process.env[name];
  if (raw == null || raw === "") return defaultValue;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n < 0) return defaultValue;
  return n;
}

/**
 * floatEnv parses a non-negative float env var and returns `default` when
 * unset or malformed. Used for min_score.
 */
export function floatEnv(name, defaultValue) {
  const raw = process.env[name];
  if (raw == null || raw === "") return defaultValue;
  const n = Number.parseFloat(raw);
  if (!Number.isFinite(n) || n < 0) return defaultValue;
  return n;
}

/**
 * listEnv parses a pipe- or comma-separated env var into a non-empty string
 * array (trimmed, lowercased). Empty / unset returns [].
 */
export function listEnv(name) {
  const raw = process.env[name];
  if (!raw) return [];
  return raw
    .split(/[|,]/)
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);
}

/**
 * labelsEnv parses MEMINI_INJECT_LABELS into a Set of enabled labels.
 * Recognized: "tier", "confidence", "age", "reason". Empty/unset returns an
 * empty Set — the format helpers then skip every label.
 */
export function labelsEnv(name = "MEMINI_INJECT_LABELS") {
  return new Set(listEnv(name));
}

/**
 * approxTokens is a cheap token estimator. ~0.75 tokens/word for English-ish
 * content, with a floor of 1 so a single non-empty line never reports 0.
 */
export function approxTokens(text) {
  if (!text) return 0;
  const words = String(text).trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.ceil((words * 4) / 3));
}

/**
 * fitByTokens trims a list of pre-formatted strings to fit under `maxTokens`,
 * keeping the head (the most-relevant entries first). Returns the trimmed
 * list and the running token total, so callers can render a "[… truncated]"
 * footer when items were dropped.
 */
export function fitByTokens(items, maxTokens) {
  if (!Array.isArray(items) || items.length === 0) return { items: [], tokens: 0, dropped: 0 };
  if (!Number.isFinite(maxTokens) || maxTokens <= 0) {
    const tokens = items.reduce((sum, s) => sum + approxTokens(s), 0);
    return { items: items.slice(), tokens, dropped: 0 };
  }
  const out = [];
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

/**
 * POST /v1/memories. Returns the saved Memory on success, null otherwise.
 */
export async function postRemember(content, namespace, opts = {}) {
  const body = { content, tier: opts.tier || "episodic" };
  if (opts.tags) body.tags = opts.tags;
  if (opts.summary) body.summary = opts.summary;
  if (typeof opts.importance === "number") body.importance = opts.importance;
  if (opts.id) body.id = opts.id;
  if (opts.metadata) body.metadata = opts.metadata;
  return postJSON("/v1/memories", body, namespace);
}

/**
 * POST /v1/memories/{id}/supersede. Tombstones a memory so default recall
 * hides it; the row stays for the audit chain and is hard-deleted by the
 * sweeper after TombstoneTTL. `id` is percent-encoded because the
 * session-end / stop: prefixes carry `:`.
 */
export async function postSupersede(id, by, namespace) {
  const enc = encodeURIComponent(id);
  return postJSON(`/v1/memories/${enc}/supersede`, { by }, namespace);
}

/**
 * Truncate to `max` bytes, suffix with a marker. Same shape as
 * agentmemory's truncate helper.
 */
export function truncate(value, max) {
  if (typeof value === "string") {
    return value.length > max ? value.slice(0, max) + "\n[...truncated]" : value;
  }
  if (value && typeof value === "object") {
    let str;
    try {
      str = JSON.stringify(value);
    } catch {
      return value;
    }
    return str.length > max ? str.slice(0, max) + "...[truncated]" : str;
  }
  return value;
}

// --- Session event buffer -------------------------------------------------
//
// Tool calls are buffered locally during a session (one JSON line each) and
// distilled into a single digest memory at session end, rather than POSTed
// per-call. This keeps the agent's memory dense — one searchable digest per
// session instead of dozens of thin tool-use fragments — and means zero
// network traffic on the hot PostToolUse path.

/** Base memini cache directory ($XDG_CACHE_HOME or ~/.cache, then /memini). */
function meminiCacheDir() {
  const base = process.env["XDG_CACHE_HOME"] || join(homedir() || tmpdir(), ".cache");
  return join(base, "memini");
}

/** Root directory for per-session event buffers. */
function bufferDir() {
  return join(meminiCacheDir(), "sessions");
}

/**
 * Path of the file recording the plugin's install root. The MCP server's
 * headersHelper is a plain shell command that — unlike hooks — does NOT receive
 * ${CLAUDE_PLUGIN_ROOT}, so it can't locate mcp-headers.mjs on its own. The
 * SessionStart hook (which does get ${CLAUDE_PLUGIN_ROOT}) writes it here, and
 * the headersHelper reads it. Both sides agree on this path.
 */
export function pluginRootFile() {
  return join(meminiCacheDir(), "plugin-root");
}

/** Record ${CLAUDE_PLUGIN_ROOT} so the MCP headersHelper can find bundled scripts. */
export function writePluginRoot() {
  const root = process.env["CLAUDE_PLUGIN_ROOT"];
  if (!root) return;
  try {
    fs.mkdirSync(meminiCacheDir(), { recursive: true });
    fs.writeFileSync(pluginRootFile(), root);
  } catch (e) {
    if (DEBUG) console.error("[memini] writePluginRoot failed:", e?.message || e);
  }
}

/** Sanitize a session id into a safe filename component. */
function safeId(sessionId) {
  return String(sessionId || "unknown").replace(/[^a-zA-Z0-9._-]/g, "_");
}

/** Path of the JSONL buffer file for a session. */
export function sessionBufferPath(sessionId) {
  return join(bufferDir(), safeId(sessionId) + ".jsonl");
}

/**
 * Append one tool event to the session buffer. Best-effort: filesystem errors
 * are swallowed so a hook never fails the agent.
 */
export function appendSessionEvent(sessionId, event) {
  try {
    fs.mkdirSync(bufferDir(), { recursive: true });
    fs.appendFileSync(sessionBufferPath(sessionId), JSON.stringify(event) + "\n");
  } catch (e) {
    if (DEBUG) console.error("[memini] appendSessionEvent failed:", e?.message || e);
  }
}

/** Read and parse a session buffer into an array of events ([] on any error). */
export function readSessionEvents(sessionId) {
  try {
    const raw = fs.readFileSync(sessionBufferPath(sessionId), "utf8");
    const out = [];
    for (const line of raw.split("\n")) {
      if (!line.trim()) continue;
      const ev = parseJSON(line);
      if (ev) out.push(ev);
    }
    return out;
  } catch {
    return [];
  }
}

// --- Auto-save state ------------------------------------------------------
//
// The Stop hook periodically nudges the agent to persist durable memories. It
// tracks how many user messages had been seen at the last nudge in a small
// state file alongside the session buffer, so it nudges once per interval
// rather than on every stop. Lives in bufferDir() so cleanStaleBuffers GCs it.

/** Path of the auto-save state file for a session. */
export function sessionStatePath(sessionId) {
  return join(bufferDir(), safeId(sessionId) + ".savestate");
}

/** Read a session's auto-save state ({lastSavedCount, updatedAt}) or null. */
export function readSaveState(sessionId) {
  try {
    return parseJSON(fs.readFileSync(sessionStatePath(sessionId), "utf8"));
  } catch {
    return null;
  }
}

/** Persist a session's auto-save state (best-effort). */
export function writeSaveState(sessionId, state) {
  try {
    fs.mkdirSync(bufferDir(), { recursive: true });
    fs.writeFileSync(sessionStatePath(sessionId), JSON.stringify(state));
  } catch (e) {
    if (DEBUG) console.error("[memini] writeSaveState failed:", e?.message || e);
  }
}

/** Delete a session's buffer file (best-effort). */
export function deleteSessionBuffer(sessionId) {
  try {
    fs.rmSync(sessionBufferPath(sessionId), { force: true });
  } catch (e) {
    if (DEBUG) console.error("[memini] deleteSessionBuffer failed:", e?.message || e);
  }
}

/** Delete buffer files older than maxAgeMs (best-effort hygiene). */
export function cleanStaleBuffers(maxAgeMs) {
  try {
    const dir = bufferDir();
    const now = Date.now();
    for (const name of fs.readdirSync(dir)) {
      const p = join(dir, name);
      try {
        if (now - fs.statSync(p).mtimeMs > maxAgeMs) fs.rmSync(p, { force: true });
      } catch {}
    }
  } catch {}
}

/**
 * Distill buffered tool events into a single dense, searchable digest. Returns
 * null when there is nothing worth recording. The shape is { content, summary,
 * files, commands, count }.
 */
export function buildSessionDigest(events, project) {
  if (!Array.isArray(events) || events.length === 0) return null;

  const fileCounts = new Map();
  const commands = [];
  const seenCmd = new Set();
  for (const ev of events) {
    const files = Array.isArray(ev.files) && ev.files.length ? ev.files : ev.file ? [ev.file] : [];
    for (const f of files) {
      if (typeof f !== "string" || !f) continue;
      fileCounts.set(f, (fileCounts.get(f) || 0) + 1);
    }
    if (ev.cmd && !seenCmd.has(ev.cmd)) {
      seenCmd.add(ev.cmd);
      commands.push(ev.cmd);
    }
  }

  const files = [...fileCounts.entries()]
    .sort((a, b) => b[1] - a[1])
    .map(([f, n]) => (n > 1 ? `${f} (${n})` : f));
  const topCommands = commands.slice(0, 10);

  const parts = [`Session digest for ${project}: ${events.length} tool calls.`];
  if (files.length) parts.push(`Edited: ${files.slice(0, 15).join(", ")}.`);
  if (topCommands.length) parts.push(`Ran: ${topCommands.map((c) => truncate(c, 80)).join("; ")}.`);

  return {
    content: parts.join(" "),
    summary: `Worked on ${files.length} file(s) in ${project}`,
    files: [...fileCounts.keys()],
    commands: topCommands,
    count: events.length,
  };
}

/** Extract a short hint of the agent's last user prompt for context. */
export function promptHint(prompt) {
  if (typeof prompt !== "string" || !prompt) return "";
  return prompt.length > 240 ? prompt.slice(0, 240) + "..." : prompt;
}

/** Coerce a hook payload's various field names to a single shape. */
export function readToolCall(payload) {
  return {
    toolName: payload?.tool_name ?? payload?.toolName ?? null,
    toolInput: payload?.tool_input ?? payload?.toolArgs ?? null,
    toolOutput: payload?.tool_response ?? payload?.tool_output ?? null,
    sessionId: payload?.session_id || payload?.sessionId || "unknown",
    cwd: payload?.cwd || process.cwd(),
  };
}
