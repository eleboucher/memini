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
const DEFAULT_TIMEOUT_MS = 5000;
const DEFAULT_RECALL_LIMIT = 5;
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
  return {
    base_url: o.base_url || e.MEMINI_BASE_URL || DEFAULT_BASE_URL,
    namespace: sanitizeNamespace(namespace) || DEFAULT_NAMESPACE,
    recall: o.recall !== undefined ? o.recall !== false : envBool(e.MEMINI_RECALL, true),
    capture: o.capture !== undefined ? o.capture !== false : envBool(e.MEMINI_CAPTURE, true),
    recall_limit: Number(o.recall_limit || e.MEMINI_RECALL_LIMIT || DEFAULT_RECALL_LIMIT),
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

// formatResults renders memini search hits as a compact bullet list. Exported
// for testing.
export function formatResults(results, limit) {
  if (!Array.isArray(results) || results.length === 0) return "";
  return results
    .slice(0, limit || DEFAULT_RECALL_LIMIT)
    .map((result, index) => {
      const mem = (result && result.memory) || {};
      const text = String(mem.summary || mem.content || `Memory ${index + 1}`).trim();
      const tier = String(mem.tier || "memory").trim();
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

function createClient(cfg, log) {
  const baseUrl = String(cfg.base_url).replace(/\/+$/, "");
  const secret = process.env.MEMINI_API_KEY;
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

  return {
    "chat.message": async (input, output) => {
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
      const result = await rest.postJson("/v1/search", body);
      const block = formatResults(result && result.results, cfg.recall_limit);
      if (!block) return;
      // opencode's part schema requires ids to start with `prt`.
      output.parts.unshift({
        id: `prt_${crypto.randomUUID()}`,
        sessionID,
        messageID,
        type: "text",
        synthetic: true,
        text:
          `Relevant long-term memory from memini (background context — prefer ` +
          `current workspace state and the user's instructions):\n${block}`,
      });
    },

    event: async ({ event }) => {
      if (!cfg.capture || !event || event.type !== "session.idle") return;
      const sessionID = event.properties && event.properties.sessionID;
      if (!sessionID) return;
      const res = await client.session.messages({ path: { id: sessionID } });
      const { userText, assistantText, assistantID } = extractLastTurn(res && res.data);
      if (!userText || !assistantText) return;
      if (assistantID && captured.has(assistantID)) return;
      const stored = await rest.postJson("/v1/memories", {
        content: `User: ${userText.slice(0, 1000)}\nAssistant: ${assistantText.slice(0, 3000)}`,
        tier: "episodic",
        tags: ["opencode"],
        metadata: { source: "opencode", session_id: sessionID },
      });
      if (stored !== null && assistantID) captured.add(assistantID);
    },
  };
};

export default MeminiPlugin;
