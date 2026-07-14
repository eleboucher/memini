// Shared utilities for the memini plugin's hook scripts.
//
//   - getSessionContext: the single per-invocation entry point — resolves the
//     namespace + behavioral settings via the server handshake (or a cached
//     one, or local derivation when the server is unreachable).
//   - readStdin:        drain stdin to a UTF-8 string
//   - postJSON/getJSON: REST helpers with bearer-token + namespace headers
//   - postSearch:       POST /v1/search and return result.memory[] of {content,score}
//   - postRemember:     POST /v1/memories
//   - postSupersede:    POST /v1/memories/{id}/supersede (tombstone)
//   - debug:            gated by MEMINI_DEBUG=1
//
// Hooks are .mjs (not .ts) so they run in plain `node` without a build step.

import { join } from "node:path";
import { homedir, tmpdir } from "node:os";
import fs from "node:fs";

// The shared client core (packages/memini-client), bundled to a committed,
// dependency-free ESM file so these hooks keep running under a bare `node` with
// no install step. Regenerate with `mise run build-client`.
import {
  readBootstrap,
  assertBearerTransportSafe,
  gatherFacts,
  resolveNamespace,
  performHandshake,
  readCachedHandshake,
  writeCachedHandshake,
  deleteCachedHandshake,
  invalidateAllHandshakes,
  BEHAVIOR_KNOBS,
  effectiveSetting,
  writeSessionCwd,
  deleteSessionCwd,
} from "./_client.gen.mjs";

export {
  writeSessionCwd,
  deleteSessionCwd,
  deleteCachedHandshake,
  invalidateAllHandshakes,
};

export const DEBUG = process.env["MEMINI_DEBUG"] === "1";

// Client identification sent with every handshake (logging/diagnostics only).
const CLIENT_NAME = "memini-claude-plugin";

/**
 * Resolve everything a hook needs — namespace, provenance, and behavioral
 * settings — once per hook invocation, via the server handshake.
 *
 *   allowNetwork:
 *     "always"  — always POST a live handshake; write the cache on success.
 *                 (SessionStart: the one hook that does the network round-trip.)
 *     "on-miss" — use a valid cached handshake; otherwise POST a live one and
 *                 write the cache on success. (Stop / PreCompact / SessionEnd.)
 *     "never"   — use a valid cached handshake ONLY; never touch the network.
 *                 (Pre/PostToolUse — the hot path, kept network-free so a live
 *                 handshake can never race or add latency to a tool call.)
 *
 * Cache policy: on a live handshake SUCCESS we ALWAYS writeCachedHandshake; on
 * ANY failure we write nothing — the ABSENCE of a cache entry is itself the
 * degraded signal a later hook reads (see `never`, above). `noPersist: true`
 * suppresses that write so a read-only diagnostic (/memini:status) can force a
 * fresh live handshake without mutating the session's cached entry underneath
 * the hooks that own it.
 *
 * The returned `setting(wireKey)` resolves a behavioral knob with provenance:
 * a local MEMINI_* env override beats the server-merged value beats the
 * built-in default (exactly effectiveSetting's precedence). Returns
 * { namespace, source, degraded, facts, handshake, boot, setting }.
 */
export async function getSessionContext({ cwd, ppid, allowNetwork = "on-miss", timeoutMs, noPersist = false, env = process.env } = {}) {
  const boot = readBootstrap(env);
  const facts = gatherFacts(cwd, env);

  const live = async () => {
    // performHandshake runs the plaintext-bearer guard OUTSIDE its own
    // try/catch, so a guard throw (MEMINI_REQUIRE_HTTPS + plaintext + a
    // secret) surfaces here rather than as a silent undefined. A hook must
    // never crash, and refusing to send the bearer is exactly the degraded
    // outcome the guard asks for — so treat the throw as a handshake failure
    // and fall through to local derivation.
    try {
      return await performHandshake(boot, facts, {
        timeoutMs: timeoutMs ?? 2500,
        clientName: CLIENT_NAME,
      });
    } catch {
      return undefined;
    }
  };

  let hs;
  if (allowNetwork === "never") {
    hs = readCachedHandshake(ppid, cwd, facts, env);
  } else if (allowNetwork === "always") {
    hs = await live();
    if (hs && !noPersist) writeCachedHandshake(ppid, cwd, facts, hs, env);
  } else {
    // "on-miss"
    hs = readCachedHandshake(ppid, cwd, facts, env);
    if (!hs) {
      hs = await live();
      if (hs && !noPersist) writeCachedHandshake(ppid, cwd, facts, hs, env);
    }
  }

  const resolved = resolveNamespace(boot, facts, hs);
  const serverSettings = hs?.settings;

  return {
    namespace: resolved.namespace,
    source: resolved.source,
    degraded: resolved.degraded,
    facts,
    handshake: hs,
    boot,
    setting(wireKey) {
      const knob = BEHAVIOR_KNOBS.find((k) => k.wireKey === wireKey);
      if (!knob) throw new Error(`getSessionContext: unknown behavior knob "${wireKey}"`);
      return effectiveSetting(knob, serverSettings, env);
    },
  };
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

// The REST client's transport is read once, here, from the same bootstrap the
// handshake uses: MEMINI_BASE_URL (no MEMINI_URL alias), MEMINI_API_KEY (no
// MEMINI_TOKEN alias), MEMINI_HOME. The plaintext-bearer guard is the bundle's
// assertBearerTransportSafe — one shared implementation, with the broadened
// accept-1/true/yes/on parsing of MEMINI_REQUIRE_HTTPS.
const boot = readBootstrap();

function authHeaders(extra) {
  const h = { "Content-Type": "application/json", ...(extra || {}) };
  if (boot.apiKey) h["Authorization"] = `Bearer ${boot.apiKey}`;
  if (boot.homeEnv) h["X-Memini-Home"] = boot.homeEnv;
  return h;
}

/**
 * POST JSON to memini. `namespace` is sent as X-Memini-Namespace. Returns
 * parsed JSON on 2xx, null otherwise. Never throws — hooks must not crash
 * the agent.
 */
export async function postJSON(path, body, namespace, timeoutMs = 5000) {
  try {
    assertBearerTransportSafe(boot.baseUrl, boot.apiKey);
    const res = await fetch(`${boot.baseUrl}${path}`, {
      method: "POST",
      headers: authHeaders({ "X-Memini-Namespace": namespace }),
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok) {
      // Degrade but never silently: a swallowed 401/500 on a capture or recall
      // looks like "memory isn't working" with nothing to debug. The response
      // body stays DEBUG-only.
      const t = DEBUG ? await res.text().catch(() => "") : "";
      console.error(`[memini] POST ${path} -> ${res.status}${t ? `: ${t.slice(0, 200)}` : ""}`);
      return null;
    }
    return await res.json();
  } catch (e) {
    console.error(`[memini] POST ${path} failed:`, e?.message || e);
    return null;
  }
}

/**
 * POST /v1/search and return an array of {content, score, memory} objects.
 * Returns [] on failure.
 *
 * `minScore` (>= 0) sets a per-call relevance floor: candidates whose fused
 * score is below it are dropped server-side. 0 / unset falls back to the
 * server's baked relevance floor (0.1). When `minScore` is set,
 * the hook also filters client-side as a belt-and-braces guard against
 * score-fusion edge cases where the server's normalization disagrees with
 * what the caller wants.
 */
export async function postSearch(query, namespace, { limit = 5, tiers, exclude, minScore, source } = {}) {
  const body = { query, limit };
  if (tiers) body.tiers = tiers;
  if (typeof minScore === "number" && minScore > 0) body.min_score = minScore;
  // source is the recall's "why" — recorded on the activity event so the feed
  // can show which integration asked. Passed through verbatim; the server never
  // validates it, so a future caller's unknown value is logged, not rejected.
  if (source) body.source = source;
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
export async function getJSON(path, namespace, timeoutMs = 5000, opts = {}) {
  try {
    assertBearerTransportSafe(boot.baseUrl, boot.apiKey);
    const res = await fetch(`${boot.baseUrl}${path}`, {
      method: "GET",
      headers: authHeaders({ "X-Memini-Namespace": namespace }),
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok) {
      // `quiet` is for probes whose failure is a legitimate answer rather than a
      // fault — e.g. /healthz behind an ingress that only routes /v1 and /mcp,
      // where a 404 means "not exposed", not "server down". Callers that probe
      // must not print an alarming line for an expected miss.
      if (!opts.quiet) console.error(`[memini] GET ${path} -> ${res.status}`);
      return null;
    }
    return await res.json();
  } catch (e) {
    if (!opts.quiet) console.error(`[memini] GET ${path} failed:`, e?.message || e);
    return null;
  }
}

/**
 * GET /v1/namespaces/briefing — a layered session-start summary
 * {namespace, facts, procedures, recent, pinned}. Returns null on failure.
 *
 * The route is header-scoped: there is NO namespace path segment — the
 * namespace rides in X-Memini-Namespace (getJSON sends it, along with the
 * bearer and X-Memini-Home, exactly like every other REST helper here). This
 * matches api/openapi.yaml and how integrations/pi and integrations/openclaw
 * call it. The old path-param form (/v1/namespaces/<ns>/briefing) does not
 * exist server-side; against a real deployment it fell through to the admin
 * UI's SPA catch-all, which 200s with HTML and made this helper silently
 * return null — SessionStart then injected only the memory directive, never
 * real briefing context.
 *
 * `opts` controls per-section caps. Each field is an int; omit (or 0) to use
 * the server default (5). Pass an explicit 0 to disable that section (REST
 * only — MCP can't distinguish omitted from zero).
 */
export async function getBriefing(namespace, opts = {}) {
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
  return getJSON(`/v1/namespaces/briefing?${params.toString()}`, namespace);
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
 * envEnabled parses a boolean env var with an explicit default. Unset/empty
 * falls back to `defaultOn`; "0", "false", "no", "off" (case-insensitive) are
 * false; anything else is true. Lets a feature ship default-on while staying
 * opt-out via MEMINI_FOO=0.
 */
export function envEnabled(name, defaultOn, env = process.env) {
  const raw = env[name];
  if (raw == null || raw === "") return defaultOn;
  return !/^(0|false|no|off)$/i.test(String(raw).trim());
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
      // If the item fits partially, truncate at the last newline boundary so
      // bullet points stay intact (~4 chars/token).
      const charBudget = (maxTokens - used) * 4;
      if (charBudget > 20) {
        let cut = s.slice(0, charBudget);
        const lastNL = cut.lastIndexOf("\n");
        if (lastNL > 20) cut = cut.slice(0, lastNL);
        if (cut.length > 20) {
          out.push(cut + "\n[...truncated]");
          used += approxTokens(cut);
          continue;
        }
      }
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
  // visibility: "project" (default, omitted) | "personal" | an ancestor
  // namespace name. Episodic/working writes are clamped to project
  // server-side regardless, so hooks that always write episodic (the
  // session/turn capture path) have no reason to set this — it exists for
  // callers (e.g. a future /remember command) that let the user choose.
  if (opts.visibility) body.visibility = opts.visibility;
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

// --- Last-recall fingerprint state ----------------------------------------
//
// PreToolUse fires on every Read/Edit/Write/Glob/Grep and searches memini per
// file. Editing the same file repeatedly re-injects an IDENTICAL memory block
// back into context on every call: the recall call itself always still runs
// (results can change between calls), but when the rendered payload for a
// file is byte-for-byte the same as what was last injected THIS session,
// re-injecting it is pure token waste — the context already carries it. This
// state is a per-session, per-file map of {hash, at}, bounded so a long
// session touching many distinct files doesn't grow the file unbounded.

const MAX_LASTRECALL_ENTRIES = 32;

/** Path of the per-session last-injected-recall fingerprint state file. */
function lastRecallStatePath(sessionId) {
  return join(bufferDir(), safeId(sessionId) + ".lastrecall.json");
}

/** Read a session's last-recall fingerprint map ({file: {hash, at}}), or {} on any error. */
export function readLastRecallState(sessionId) {
  try {
    return parseJSON(fs.readFileSync(lastRecallStatePath(sessionId), "utf8")) || {};
  } catch {
    return {};
  }
}

/**
 * Persist a session's last-recall fingerprint map (best-effort), bounded to
 * the MAX_LASTRECALL_ENTRIES most-recently-updated entries — oldest (by `at`)
 * evicted first — so a session touching many distinct files keeps this file
 * small.
 */
export function writeLastRecallState(sessionId, state) {
  try {
    fs.mkdirSync(bufferDir(), { recursive: true });
    let bounded = state;
    const entries = Object.entries(state || {});
    if (entries.length > MAX_LASTRECALL_ENTRIES) {
      entries.sort((a, b) => (b[1]?.at || 0) - (a[1]?.at || 0));
      bounded = Object.fromEntries(entries.slice(0, MAX_LASTRECALL_ENTRIES));
    }
    fs.writeFileSync(lastRecallStatePath(sessionId), JSON.stringify(bounded));
  } catch (e) {
    if (DEBUG) console.error("[memini] writeLastRecallState failed:", e?.message || e);
  }
}

/** Delete a session's last-recall fingerprint state (best-effort). */
export function deleteLastRecallState(sessionId) {
  try {
    fs.rmSync(lastRecallStatePath(sessionId), { force: true });
  } catch (e) {
    if (DEBUG) console.error("[memini] deleteLastRecallState failed:", e?.message || e);
  }
}

function briefingCachePath(sessionId) {
  return join(bufferDir(), safeId(sessionId) + ".briefing-hash");
}

/**
 * Check if the briefing content hash matches the cached one for this session.
 * Returns true if unchanged (caller should replay cached text), false if
 * changed or new (caller should render fresh text and cache the new hash).
 */
export function briefingUnchanged(sessionId, contentHash) {
  try {
    const cached = fs.readFileSync(briefingCachePath(sessionId), "utf8").trim();
    return cached === contentHash;
  } catch {
    return false;
  }
}

/** Cache a briefing content hash for a session. */
export function cacheBriefingHash(sessionId, contentHash) {
  try {
    fs.mkdirSync(bufferDir(), { recursive: true });
    fs.writeFileSync(briefingCachePath(sessionId), contentHash);
  } catch (e) {
    if (DEBUG) console.error("[memini] cacheBriefingHash failed:", e?.message || e);
  }
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
  const failedCmd = new Set();
  const okCmd = new Set();
  for (const ev of events) {
    const files = Array.isArray(ev.files) && ev.files.length ? ev.files : ev.file ? [ev.file] : [];
    for (const f of files) {
      if (typeof f !== "string" || !f) continue;
      fileCounts.set(f, (fileCounts.get(f) || 0) + 1);
    }
    if (ev.cmd) {
      if (!seenCmd.has(ev.cmd)) {
        seenCmd.add(ev.cmd);
        commands.push(ev.cmd);
      }
      if (ev.failed) failedCmd.add(ev.cmd);
      else okCmd.add(ev.cmd);
    }
  }

  const files = [...fileCounts.entries()]
    .sort((a, b) => b[1] - a[1])
    .map(([f, n]) => (n > 1 ? `${f} (${n})` : f));
  const topCommands = commands.slice(0, 10);

  // Mark a command "(failed)" only when it never succeeded this session, so a
  // failed→fixed pair reads as "X (failed); Y". A retried-then-passing command
  // is not marked.
  const renderCmd = (c) =>
    failedCmd.has(c) && !okCmd.has(c) ? `${truncate(c, 80)} (failed)` : truncate(c, 80);

  const parts = [`Session digest for ${project}: ${events.length} tool calls.`];
  if (files.length) parts.push(`Edited: ${files.slice(0, 15).join(", ")}.`);
  if (topCommands.length) parts.push(`Ran: ${topCommands.map(renderCmd).join("; ")}.`);

  return {
    content: parts.join(" "),
    summary: `Worked on ${files.length} file(s) in ${project}`,
    files: [...fileCounts.keys()],
    commands: topCommands,
    count: events.length,
    // Rendered anchors for the auto-save nudge: `files` sliced+annotated exactly
    // as the "Edited:" line above, and the subset of commands that only ever
    // failed (so a nudge can echo the session's activity in the digest's style).
    renderedFiles: files.slice(0, 15),
    failedCommands: topCommands.filter((c) => failedCmd.has(c) && !okCmd.has(c)),
  };
}

// --- Event-aware auto-save nudge ------------------------------------------
//
// The Stop hook nudges the model to persist durable memories, but a naive
// "every N user messages" nudge fires blind: it interrupts even when the model
// already saved, and it nudges trivial back-and-forth with nothing to save,
// training the model to no-op the block. These pure helpers make the nudge
// event-aware — suppress when saves are observed, defer trivial windows, and
// anchor real nudges in the session's actual files/commands. Stop wires them up.

/**
 * True for the memini memory-writing MCP tools. MCP tool names carry a
 * client-dependent prefix (e.g. mcp__plugin_memini_memini__memory_remember);
 * the suffix is stable, so match the bare names or the delimited __ suffix.
 * Deliberately excludes memory_recall/get/forget/list — only saves count.
 */
export function isMemorySaveTool(name) {
  if (typeof name !== "string" || !name) return false;
  return (
    name === "memory_remember" ||
    name === "memory_update" ||
    name.endsWith("__memory_remember") ||
    name.endsWith("__memory_update")
  );
}

/**
 * Single pass over a Claude Code transcript (JSONL string) → { userMessages,
 * memorySaves }. userMessages uses the exact same predicate as the auto-save
 * counter always has (skip sidechain/meta rows and non-real-user content).
 * memorySaves counts memory_remember/memory_update tool_use blocks in assistant
 * rows, INCLUDING sidechain assistant rows — a subagent's save is still a save.
 */
export function scanTranscriptStats(raw) {
  let userMessages = 0;
  let memorySaves = 0;
  if (typeof raw !== "string" || !raw) return { userMessages, memorySaves };
  for (const line of raw.split("\n")) {
    if (!line.trim()) continue;
    const r = parseJSON(line);
    if (!r) continue;
    if (r.type === "user") {
      if (r.isSidechain || r.isMeta) continue;
      if (isRealUserMessage(r.message?.content)) userMessages++;
    } else if (r.type === "assistant") {
      const c = r.message?.content;
      if (Array.isArray(c)) {
        for (const block of c) {
          if (block?.type === "tool_use" && isMemorySaveTool(block.name)) memorySaves++;
        }
      }
    }
  }
  return { userMessages, memorySaves };
}

/**
 * Decide what the Stop hook should do about an auto-save nudge, as a pure
 * function of the persisted save-state, the transcript stats, the buffered tool
 * events, and the two knobs (interval, minEvents). Returns:
 *   { action, variant?, nextState?, fresh? }
 * where action ∈ baseline | none | suppress | defer | nudge. `nextState` is the
 * set of save-state fields the caller must merge in (spreading existing state so
 * co-tenants like lastCapturedTurn survive); its absence means "leave state as
 * is". `fresh` (the events newer than the last activity baseline) is returned on
 * a nudge so the caller can build the anchor. See the decision table in the
 * task brief; the ordering of the branches below IS that table.
 */
export function evaluateAutoSave({ state, stats, events, now, interval, minEvents }) {
  const reBaseline = {
    lastSavedCount: stats.userMessages,
    lastSaveToolCount: stats.memorySaves,
    lastActivityBaselineTs: now,
  };

  // Row 1: no usable state (first sight, a legacy state missing the new fields,
  // or a count regression from a replaced transcript) → silent full baseline.
  if (
    !state ||
    typeof state.lastSavedCount !== "number" ||
    typeof state.lastSaveToolCount !== "number" ||
    typeof state.lastActivityBaselineTs !== "number" ||
    state.lastSavedCount > stats.userMessages
  ) {
    return { action: "baseline", nextState: reBaseline };
  }

  const msgs = stats.userMessages - state.lastSavedCount;
  const saves = stats.memorySaves - state.lastSaveToolCount;

  // Row 2: below the interval → nothing yet.
  if (msgs < interval) return { action: "none" };

  // Row 3: the model already saved this window → suppress and re-baseline, so we
  // don't interrupt a session that's keeping its memory current.
  if (saves > 0) return { action: "suppress", nextState: reBaseline };

  const fresh = Array.isArray(events)
    ? events.filter((ev) => ev && typeof ev.ts === "number" && ev.ts > state.lastActivityBaselineTs)
    : [];

  // Row 4: the activity gate is disabled → nudge at the interval unconditionally,
  // anchoring with fresh activity when there is any (legacy count-only behavior).
  if (minEvents === 0) {
    return { action: "nudge", variant: fresh.length > 0 ? "specifics" : "generic", nextState: reBaseline, fresh };
  }

  // Row 5: enough fresh activity → an anchored nudge.
  if (fresh.length >= minEvents) {
    return { action: "nudge", variant: "specifics", nextState: reBaseline, fresh };
  }

  // Row 6: a trivial window, still early → defer (no re-baseline) so the deltas
  // keep growing toward the escalation threshold instead of resetting.
  if (msgs < 2 * interval) return { action: "defer" };

  // Row 7: a trivial window that has now doubled the interval → nudge anyway, as a
  // discussion-variant (there may be decisions/preferences even without tool use).
  return { action: "nudge", variant: "discussion", nextState: reBaseline, fresh };
}

/**
 * Render the auto-save nudge text for a variant. ctx = { msgs, files, commands,
 * failedCommands }, where files/commands come straight from buildSessionDigest
 * (files pre-rendered with counts, commands raw, failedCommands the ones that
 * only failed); generic/discussion pass empty arrays. Pure: no I/O.
 */
export function renderAutoSaveNudge(variant, ctx = {}) {
  const { msgs = 0, files = [], commands = [], failedCommands = [] } = ctx;
  let anchor = "";
  if (variant === "specifics") {
    const clauses = [];
    if (files.length) clauses.push(`edited ${files.join(", ")}`);
    if (commands.length) {
      const rendered = commands.map(
        (c) => `"${truncate(c, 80)}"` + (failedCommands.includes(c) ? " (failed)" : ""),
      );
      clauses.push(`ran ${rendered.join(", ")}`);
    }
    if (clauses.length) anchor = `You ${clauses.join("; ")}. `;
  } else if (variant === "discussion") {
    anchor = "This was mostly discussion — check for decisions and preferences. ";
  }
  return (
    `[memini auto-save] ${msgs} user messages since the last save. ${anchor}` +
    "Scan this conversation for anything durable you have not yet saved: " +
    "decisions and their rationale, bug root causes, conventions, user preferences " +
    "or corrections, environment quirks, non-obvious commands. Persist each with the " +
    "memini memory_remember MCP tool — one self-contained fact per call; omit tier to " +
    "auto-classify. If a memory recalled this session proved wrong or outdated, fix it " +
    "now with memory_update, or memory_forget if it should not exist. Skip secrets, " +
    "task progress, and anything already saved or already in project docs. If none of " +
    "the above produced a durable decision, just stop again."
  );
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

// Memory directive: at SessionStart the plugin injects an instruction telling
// the agent to proactively persist durable facts via the memory_remember MCP
// tool. On by default; opt out with MEMINI_INLINE_EXTRACT=0. The Stop hook
// keeps a legacy scraper (parseMemoryBlocks) as a back-compat fallback for
// sessions still emitting inline <memory> blocks under the old instruction.
//
// The save-policy invariants below (what is durable, visibility, correction
// hygiene) are canonical in internal/api/mcp/mcp.go serverInstructions; keep
// the two phrasings in sync.

export const MEMORY_INSTRUCTION = `

<memini-memory-directive>
You have persistent cross-session memory via the memini memory_remember MCP
tool. Saving is your job — do not wait for the user to ask. Save one memory
per durable fact when you learn:
- a decision and the reason it was made
- a bug's root cause, or a gotcha worth flagging next time
- a project convention (layout, style, test/deploy commands)
- a stated user preference, or a correction the user gives you — a
  correction IS a preference
- an environment or tool quirk, or a non-obvious command/workflow

When the user says "remember this" or corrects you, call memory_remember
first, then acknowledge — and save an explicit request unconditionally, even
if it seems trivial or already stored; secrets and credentials are the one
exception. Before ending a turn in which you
learned something durable, make sure it was saved.

Rules:
- Each memory must be self-contained, readable without this conversation's
  context. State facts, not commands: "User prefers concise replies" (good),
  "Always reply concisely" (bad).
- visibility: "personal" for anything true of the USER wherever they go
  (their preferences, habits, how they like to work); "project" (the
  default) for anything specific to this codebase. Getting this wrong is
  the common failure: a preference saved as "project" is stranded here and
  will not follow them to their next repo.
- Omit tier to let the server classify. Tag a critical, always-relevant
  fact "pinned" so it surfaces in every session briefing.
- Never save secrets or credentials, transient session state, task
  progress, or facts already in CLAUDE.md or project docs.
- If a stored memory turns out to be wrong or outdated, fix it immediately:
  correct it in place with the memory_update MCP tool, or delete it with
  memory_forget if it should not exist. Never leave a memory you know is
  incorrect in place.
- Never print memory markup or JSON memory payloads in your reply text.
  Memories are saved only through the MCP tools. If the tools are
  unavailable, do nothing.
</memini-memory-directive>`;

// Injected by SessionStart after a compaction (wired by a later task): durable
// facts learned before the compaction may have fallen out of the model's
// visible context, so prompt it to flush anything not yet persisted.
export const COMPACT_RECOVERY_DIRECTIVE = `

<memini-compact-recovery>
Context was just compacted. Durable facts learned before compaction may no longer be visible to you. If you remember learning something durable this session that you have not saved with the memini memory_remember MCP tool, save it now — one self-contained fact per call. If everything durable is already saved, continue.
</memini-compact-recovery>`;

/**
 * Parse all <memory>...</memory> blocks from a text. Returns an array of
 * memory objects ({content}) extracted from the JSON inside each block.
 * Malformed JSON or empty arrays yield []. Never throws.
 */
export function parseMemoryBlocks(text) {
  if (typeof text !== "string" || !text) return [];
  const out = [];
  const re = /<memory>\s*([\s\S]*?)\s*<\/memory>/gi;
  let m;
  while ((m = re.exec(text)) !== null) {
    const raw = m[1].trim();
    if (!raw) continue;
    try {
      const obj = JSON.parse(raw);
      if (obj && Array.isArray(obj.memories)) {
        for (const item of obj.memories) {
          if (item && typeof item.content === "string" && item.content.trim()) {
            out.push({ content: item.content.trim() });
          }
        }
      }
    } catch {
      // malformed JSON in the block — skip it, keep scanning
    }
  }
  return out;
}

/**
 * Extract assistant message text from a Claude Code transcript (JSONL string).
 * Returns an array of strings (one per assistant text message). Tool-use-only
 * turns (no text block) are skipped.
 */
export function extractAssistantText(transcript) {
  if (typeof transcript !== "string" || !transcript) return [];
  const out = [];
  for (const line of transcript.split("\n")) {
    if (!line.trim()) continue;
    const r = parseJSON(line);
    if (!r || r.type !== "assistant") continue;
    const c = r.message?.content;
    if (typeof c === "string") {
      out.push(c);
    } else if (Array.isArray(c)) {
      // Claude Code assistant messages are arrays of content blocks;
      // text blocks carry the model's prose.
      for (const block of c) {
        if (block?.type === "text" && typeof block.text === "string") {
          out.push(block.text);
        }
      }
    }
  }
  return out;
}

/**
 * Report whether a transcript entry's `message.content` is a real user turn:
 * a string (tool_result entries are arrays) that isn't slash-command or
 * local-command scaffolding. Shared by the turn extractor and the auto-save
 * message counter so both skip the same noise.
 */
export function isRealUserMessage(content) {
  if (typeof content !== "string") return false; // arrays are tool_results, not user turns
  // Skipped: slash-command / local-command scaffolding, memini's own injected
  // recall blocks (<memini-context>/<memini-pretool> — capturing one would echo
  // recalled memories back into memory), hook-injected system reminders, and
  // harness-injected background-task events (<task-notification> /
  // "[SYSTEM NOTIFICATION ...]"), which are not user turns.
  return !/^\s*(<(local-command|command-|memini-|system-reminder|task-notification)|\[SYSTEM NOTIFICATION)/.test(content);
}

/**
 * Extract the latest complete user→assistant turn from a Claude Code transcript
 * (JSONL string). Returns { userText, assistantText, assistantId } — the last
 * real user message (string content only; tool_result arrays and slash/local
 * command noise are skipped) and the final assistant prose, plus the assistant
 * message id for dedup. Fields are "" when absent. Never throws.
 *
 * Mirrors the opencode plugin's extractLastTurn so Claude gets the same
 * episodic per-turn capture.
 */
export function extractLastTurn(transcript) {
  let userText = "";
  let assistantText = "";
  let assistantId = "";
  if (typeof transcript !== "string" || !transcript) return { userText, assistantText, assistantId };
  for (const line of transcript.split("\n")) {
    if (!line.trim()) continue;
    const r = parseJSON(line);
    if (!r || r.isSidechain || r.isMeta) continue;
    if (r.type === "user") {
      const c = r.message?.content;
      if (!isRealUserMessage(c)) continue;
      userText = c.trim();
    } else if (r.type === "assistant") {
      const c = r.message?.content;
      let text = "";
      if (typeof c === "string") {
        text = c;
      } else if (Array.isArray(c)) {
        text = c
          .filter((b) => b?.type === "text" && typeof b.text === "string")
          .map((b) => b.text)
          .join("\n");
      }
      text = text.trim();
      // Keep the last assistant message that actually has prose; tool-use-only
      // turns carry no text and shouldn't blank out the captured reply.
      if (text) {
        assistantText = text;
        assistantId = r.message?.id || r.uuid || "";
      }
    }
  }
  return { userText, assistantText, assistantId };
}

/**
 * Read a transcript file and return its raw content. Returns "" on any error
 * (file missing, unreadable). Used by the Stop hook to scan for inline
 * <memory> blocks emitted during the session.
 */
export function readTranscript(transcriptPath) {
  if (!transcriptPath || typeof transcriptPath !== "string") return "";
  try {
    return fs.readFileSync(transcriptPath, "utf8");
  } catch {
    return "";
  }
}
