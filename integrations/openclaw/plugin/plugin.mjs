/**
 * memini memory-slot plugin for OpenClaw.
 *
 * Claims plugins.slots.memory via api.registerMemoryCapability, plus:
 *   - before_agent_start: recall relevant memories, prepend as context
 *   - agent_end: capture the completed turn into memini
 *
 * Talks to memini over REST (/v1/search, /v1/memories), scoped by the
 * X-Memini-Namespace header. Default endpoint http://localhost:8080.
 */

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
    fallback_on_error: { type: "boolean" },
    timeout_ms: { type: "number" },
  },
};

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

  async function postJson(path, payload) {
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers = { "Content-Type": "application/json", "X-Memini-Namespace": namespace };
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

  return { postJson, baseUrl, namespace };
}

const plugin = {
  id: "memini",
  name: "memini",
  description: "Shared cross-session memory via a memini service.",
  configSchema,
  register(api) {
    const cfg = {
      enabled: api.pluginConfig?.enabled !== false,
      base_url: api.pluginConfig?.base_url || DEFAULT_BASE_URL,
      namespace: api.pluginConfig?.namespace || DEFAULT_NAMESPACE,
      fallback_on_error: api.pluginConfig?.fallback_on_error !== false,
      timeout_ms: api.pluginConfig?.timeout_ms || DEFAULT_TIMEOUT_MS,
    };
    const client = createClient(cfg, api);

    if (typeof api.registerMemoryCapability === "function") {
      api.registerMemoryCapability({
        promptBuilder: () => [
          `Long-term memory: memini at ${client.baseUrl}, namespace "${client.namespace}".`,
          "Relevant memories are recalled before each turn and turns are captured after. Treat recalled context as background; prefer current workspace state and user instructions.",
        ],
      });
    }

    api.on("before_agent_start", async (event) => {
      if (!cfg.enabled) return;
      const prompt = typeof event?.prompt === "string" ? event.prompt.trim() : "";
      if (!prompt) return;
      const result = await client.postJson("/v1/search", { query: prompt, limit: 5 });
      const block = formatResults(result?.results || []);
      if (!block) return;
      return { prependContext: `Relevant long-term memory from memini:\n${block}` };
    });

    api.on("agent_end", async (event) => {
      if (!cfg.enabled || !event?.success || !Array.isArray(event.messages)) return;
      const userText = lastTextByRole(event.messages, "user");
      const assistantText = lastTextByRole(event.messages, "assistant");
      if (!userText || !assistantText) return;
      await client.postJson("/v1/memories", {
        content: `User: ${userText.slice(0, 1000)}\nAssistant: ${assistantText.slice(0, 3000)}`,
        tier: "episodic",
        metadata: { source: "openclaw" },
      });
    });
  },
};

export default plugin;
