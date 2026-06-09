// Shared utilities for the memini plugin's hook scripts.
//
// Mirrors the layout of agentmemory's plugin/scripts/_shared.mjs:
//   - resolveProject:   env > git toplevel basename > cwd basename
//   - readStdin:        drain stdin to a UTF-8 string
//   - jsonRequest:      POST JSON with bearer-token + namespace headers
//   - postSearch:       POST /v1/search and return result.memory[] of {content,score}
//   - postRemember:     POST /v1/memories
//   - debug:            gated by MEMINI_DEBUG=1
//
// Hooks are .mjs (not .ts) so they run in plain `node` without a build step.

import { execSync } from "node:child_process";
import { basename } from "node:path";

export const DEBUG = process.env["MEMINI_DEBUG"] === "1";

/**
 * Resolve the project (namespace) for a hook invocation.
 * Order: MEMINI_NAMESPACE env > git toplevel basename in cwd > cwd basename.
 *
 * Mirrors agentmemory's resolveProject: the agent supplies `data.cwd` with
 * its real working directory, so the resolver runs there. This is the
 * authoritative source — the server-side auto-resolve is only a fallback
 * for clients that send no namespace.
 */
export function resolveProject(cwd) {
  const nsEnv = process.env["MEMINI_NAMESPACE"];
  if (nsEnv && nsEnv.trim()) return nsEnv.trim();
  const dir = cwd && cwd.trim() ? cwd : process.cwd();
  try {
    const top = execSync("git rev-parse --show-toplevel", {
      cwd: dir,
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 500,
    })
      .toString()
      .trim();
    if (top) return basename(top);
  } catch {}
  return basename(dir);
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

const REST_URL = process.env["MEMINI_URL"] || "http://localhost:8080";
const SECRET = process.env["MEMINI_TOKEN"] || process.env["MEMINI_API_KEY"] || "";

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
 */
export async function postSearch(query, namespace, { limit = 5, tiers } = {}) {
  const body = { query, limit };
  if (tiers) body.tiers = tiers;
  const res = await postJSON("/v1/search", body, namespace);
  if (!res || !Array.isArray(res.results)) return [];
  return res.results.map((r) => ({
    content: r?.memory?.content || "",
    score: typeof r?.score === "number" ? r.score : 0,
    memory: r?.memory || null,
  }));
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
