/**
 * memini memory plugin for opencode.
 *
 * Hooks opencode's plugin API so memory is automatic — the model never has to
 * call a tool:
 *   - chat.message: recall memories relevant to the incoming user message and
 *     inject them as a synthetic context part before the turn runs.
 *   - event (session.idle): capture the completed user/assistant turn into
 *     memini as episodic memory once the session goes idle.
 *
 * Talks to memini over REST (/v1/search, /v1/memories), scoped by the
 * X-Memini-Namespace header. Default endpoint http://localhost:8080.
 *
 * Config comes from the plugin options (the [name, options] form in
 * opencode.json), with env-var fallbacks; secrets like MEMINI_API_KEY come from
 * the environment. See the options/env table in ../README.md.
 */

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 30000;
const DEFAULT_RECALL_LIMIT = 3;
const DEFAULT_NAMESPACE = "opencode";
const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

function envBool(value, fallback) {
  if (value === undefined || value === null || value === "") return fallback;
  return !/^(0|false|no|off)$/i.test(String(value).trim());
}

// sanitizeNamespace keeps the X-Memini-Namespace value header-safe (the server
// sanitizes too, but the header should be clean): alnum, dot, dash, underscore;
// collapse the rest to dashes and trim.
function sanitizeNamespace(s) {
  return String(s).trim().replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
}

// deriveNamespace scopes memory to the project: the basename of the git
// worktree (the repo dir name), which is the same scheme memini auto-resolves
// from a git repo. Use the same namespace across your other agents to share
// memory. Returns "" when no path is given.
export function deriveNamespace(worktree) {
  if (typeof worktree !== "string" || !worktree.trim()) return "";
  const base = worktree.replace(/[\\/]+$/, "").split(/[\\/]/).pop() || "";
  return sanitizeNamespace(base);
}

// resolveConfig merges env vars with the options object (options win), filling
// in defaults. Exported for testing.
export function resolveConfig(env, options, worktree) {
  const e = env || {};
  const o = options || {};
  const namespace =
    o.namespace || e.MEMINI_NAMESPACE || deriveNamespace(worktree) || DEFAULT_NAMESPACE;
  // Number.isFinite guard: malformed env / option falls through to the next
  // source instead of NaN flowing into the request body.
  const recall_limit = (() => {
    for (const v of [o.recall_limit, e.MEMINI_RECALL_LIMIT, DEFAULT_RECALL_LIMIT]) {
      const n = Number(v);
      if (Number.isFinite(n) && n >= 0) return n;
    }
    return DEFAULT_RECALL_LIMIT;
  })();
  return {
    base_url: o.base_url || e.MEMINI_BASE_URL || e.MEMINI_URL || DEFAULT_BASE_URL,
    namespace: sanitizeNamespace(namespace) || DEFAULT_NAMESPACE,
    recall: o.recall !== undefined ? o.recall !== false : envBool(e.MEMINI_RECALL, true),
    capture: o.capture !== undefined ? o.capture !== false : envBool(e.MEMINI_CAPTURE, true),
    recall_limit,
    recall_max_tokens:
      o.recall_max_tokens !== undefined
        ? Number(o.recall_max_tokens) || 0
        : intEnv("MEMINI_INJECT_RECALL_MAX_TOK", 0),
    recall_min_score:
      o.recall_min_score !== undefined
        ? Number(o.recall_min_score) || 0
        : floatEnv("MEMINI_INJECT_RECALL_MIN_SCORE", 0),
    timeout_ms: Number(o.timeout_ms || e.MEMINI_TIMEOUT_MS || DEFAULT_TIMEOUT_MS),
    fallback_on_error:
      o.fallback_on_error !== undefined
        ? o.fallback_on_error !== false
        : envBool(e.MEMINI_FALLBACK, true),
  };
}

// extractPartsText joins the text of a message's parts, skipping our injected
// recall context (synthetic) and ignored parts so captured turns hold only what
// the user wrote and what the assistant replied. Exported for testing.
export function extractPartsText(parts) {
  if (!Array.isArray(parts)) return "";
  return parts
    .filter((p) => p && p.type === "text" && p.synthetic !== true && p.ignored !== true)
    .map((p) => (typeof p.text === "string" ? p.text : ""))
    .join("\n")
    .trim();
}

// formatResults returns an array of bullet lines; the caller passes it to
// fitByTokens to apply a token ceiling, then joins + appends a footer.
//
// `labels` (optional) toggles the rich prefix: empty -> "- (tier) text" (the
// prior format, kept identical so snapshots don't break); non-empty ->
// "[tier · conf · age] text", same shape as the Claude Code plugin's
// formatMemory in plugin/scripts/session-start.mjs. Exported for testing.
export function formatResults(results, limit, labels) {
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
      const tagParts = [];
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
    .filter(Boolean);
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

// createPlaintextBearerAuthGuard refuses (MEMINI_REQUIRE_HTTPS=1) or warns once
// when a bearer token would be sent over plaintext HTTP to a non-loopback host.
// Exported for testing.
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

// --- Injection budget ----------------------------------------------------
//
// Near-verbatim copies of plugin/scripts/_shared.mjs. The opencode plugin
// ships standalone on npm so it can't import across the tree; copy matches
// the precedent set by createPlaintextBearerAuthGuard above. Keep contracts
// identical when both sides change.

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
 * labelsEnv parses MEMINI_INJECT_LABELS into a Set of enabled labels.
 * Recognized: "tier", "confidence", "age", "reason". Empty/unset returns an
 * empty Set — the format helpers then skip every label.
 */
export function labelsEnv(name = "MEMINI_INJECT_LABELS") {
  const raw = process.env[name];
  if (!raw) return new Set();
  return new Set(
    raw
      .split(/[|,]/)
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean),
  );
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
 * Truncate to `max` bytes, suffix with a marker. Same shape as the Claude
 * Code plugin's truncate helper.
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

function createClient(cfg, log) {
  const baseUrl = String(cfg.base_url).replace(/\/+$/, "");
  const secret = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
  const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m) => log.warn(m));
  if (process.env.MEMINI_REQUIRE_HTTPS === "1") guardPlaintextBearerAuth(baseUrl, secret);

  async function postJson(path, payload) {
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers = { "Content-Type": "application/json", "X-Memini-Namespace": cfg.namespace };
    if (secret) headers.Authorization = `Bearer ${secret}`;
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(cfg.timeout_ms),
      });
      if (!res.ok) {
        if (cfg.fallback_on_error) return null;
        const body = await res.text().catch(() => "");
        throw new Error(`memini ${path} failed: ${res.status} ${body}`);
      }
      return await res.json();
    } catch (error) {
      if (!cfg.fallback_on_error) throw error;
      log.warn(`memini: ${String(error)}`);
      return null;
    }
  }

  return { postJson, baseUrl };
}

// extractLastTurn returns the latest user and assistant text from the message
// list returned by client.session.messages ([{info, parts}, ...]), plus the id
// of the assistant message (for dedup). Iterates in reverse to short-circuit.
// Exported for testing.
export function extractLastTurn(messages) {
  let userText = "";
  let assistantText = "";
  let assistantID = "";
  if (!Array.isArray(messages)) return { userText, assistantText, assistantID };
  for (const entry of [...messages].reverse()) {
    const info = entry && entry.info;
    if (!info) continue;
    const text = extractPartsText(entry.parts);
    if (!text) continue;
    if (info.role === "user" && !userText) {
      userText = text;
    } else if (info.role === "assistant" && !assistantText) {
      assistantText = text;
      assistantID = info.id || "";
    }
    if (userText && assistantText) break;
  }
  return { userText, assistantText, assistantID };
}

// lastAssistantFailed reports whether the latest assistant turn errored, so the
// capture can flag it (the distiller mines failed→fixed turns into recovery).
// Exported for testing.
export function lastAssistantFailed(messages) {
  if (!Array.isArray(messages)) return false;
  for (const entry of [...messages].reverse()) {
    if (entry && entry.info && entry.info.role === "assistant") {
      return !!entry.info.error;
    }
  }
  return false;
}

export const MeminiPlugin = async ({ client, worktree, directory }, options) => {
  const log = {
    warn: (message) => {
      // client.app.log is opencode's structured logger; fall back to stderr.
      try {
        client?.app?.log?.({ body: { service: "memini", level: "warn", message } });
      } catch {
        /* ignore logging failures */
      }
      console.error(`[memini] ${message}`);
    },
  };

  const cfg = resolveConfig(process.env, options, worktree || directory);
  const rest = createClient(cfg, log);
  // Assistant message ids already captured, so repeated session.idle events for
  // the same turn don't write duplicates.
  const captured = new Set();

  // opencode runs chat.message via an unguarded Effect.promise (a throw aborts the
  // turn) and dispatches event hooks fire-and-forget, so a hook must never reject:
  // swallow and log instead.
  const guard = (name, fn) => async (...args) => {
    try {
      return await fn(...args);
    } catch (error) {
      log.warn(`${name} hook failed: ${String(error)}`);
    }
  };

  return {
    "chat.message": guard("chat.message", async (input, output) => {
      if (!cfg.recall) return;
      const query = extractPartsText(output && output.parts);
      if (!query) return;
      // Borrow sessionID/messageID from the real parts when the hook input
      // omits them (messageID is optional in the contract), so the injected
      // part is attributed to the same message.
      const sibling = output.parts.find((p) => p && p.type === "text") || {};
      const sessionID = input.sessionID || sibling.sessionID;
      const messageID = input.messageID || sibling.messageID;
      const body = { query, limit: cfg.recall_limit };
      // Exclude this session's own captured turns: they're still in the live
      // context, so recalling them just echoes the conversation back a turn
      // behind. Captures from other (past) sessions are still recalled.
      if (sessionID) body.exclude_metadata = { session_id: sessionID };
      // min_score (fused-score floor) is optional and matches the wire knob
      // the Claude Code plugin's pre-tool-use hook uses; client-side re-filter
      // is a belt-and-braces guard against score-normalization edge cases.
      if (cfg.recall_min_score > 0) body.min_score = cfg.recall_min_score;
      const result = await rest.postJson("/v1/search", body);
      // Client-side score floor: filter the raw hit list before formatting so
      // the bullet array only contains hits the operator asked for. Without
      // this, the server's default floor could leak low-quality hits in
      // regardless of cfg.recall_min_score.
      const floor = cfg.recall_min_score > 0 ? cfg.recall_min_score : 0;
      const rawHits = Array.isArray(result && result.results) ? result.results : [];
      const filtered = floor > 0
        ? rawHits.filter((r) => (typeof r?.score === "number" ? r.score : 0) >= floor)
        : rawHits;
      const labels = labelsEnv();
      const hits = formatResults(filtered, cfg.recall_limit, labels);
      if (hits.length === 0) return;
      // Apply the token ceiling to the rendered bullet lines; with max=0
      // (the default) fitByTokens returns the full list unchanged, so the
      // behaviour matches the prior "no cap" code path for existing installs.
      const fit = fitByTokens(hits, cfg.recall_max_tokens);
      if (fit.items.length === 0) return;
      const lines = [
        `Relevant long-term memory from memini (background context — prefer ` +
          `current workspace state and the user's instructions):`,
        ...fit.items,
      ];
      if (fit.dropped > 0) lines.push(`[... ${fit.dropped} item(s) truncated by token budget]`);
      // opencode's part schema requires ids to start with `prt`.
      output.parts.unshift({
        id: `prt_${crypto.randomUUID()}`,
        sessionID,
        messageID,
        type: "text",
        synthetic: true,
        text: lines.join("\n"),
      });
    }),

    event: guard("event", async ({ event }) => {
      if (!cfg.capture || !event || event.type !== "session.idle") return;
      const sessionID = event.properties && event.properties.sessionID;
      if (!sessionID) return;
      const res = await client.session.messages({ path: { id: sessionID } });
      const { userText, assistantText, assistantID } = extractLastTurn(res && res.data);
      if (!userText || !assistantText) return;
      if (assistantID && captured.has(assistantID)) return;
      const metadata = { source: "opencode", session_id: sessionID, format: "turn" };
      if (lastAssistantFailed(res && res.data)) metadata.failed = true;
      const stored = await rest.postJson("/v1/memories", {
        content: `${userText.slice(0, 1000)}\n\n${assistantText.slice(0, 3000)}`,
        tier: "episodic",
        tags: ["opencode"],
        metadata,
      });
      if (stored !== null && assistantID) captured.add(assistantID);
    }),
  };
};

export default { id: "memini", server: MeminiPlugin };
