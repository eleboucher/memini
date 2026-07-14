/**
 * memini memory plugin for opencode v2 (the Plugin.define / `setup` API).
 *
 * This is the v2 sibling of memini.js. It targets the documented v2 plugin
 * contract (https://v2.opencode.ai — "Plugins"):
 *   - ctx.session.hook("request", …): recall memories relevant to the incoming
 *     turn and inject them into the model request (the `system` prompt) before
 *     dispatch. This is the v2 equivalent of v1's `chat.message`.
 *   - ctx.event.subscribe("session.idle"): once a session goes idle, capture the
 *     completed user/assistant turn into memini. The v2 equivalent of v1's
 *     `event` hook. Started detached (never awaited inside `setup`, per the docs)
 *     and torn down by the returned cleanup.
 *   - ctx.tool.transform(t => t.add(…)): register the read-only `memini_status`
 *     tool. The v2 equivalent of v1's `tool: { memini_status }`.
 *
 * Everything that is not opencode-contract-specific — config/namespace
 * resolution, the handshake, formatting, token budgeting, status rendering, the
 * REST client — is imported from memini.js so the two plugin generations stay
 * byte-for-byte identical in behaviour. Only the host wiring differs.
 *
 * Dependency-free, like memini.js: `Plugin.define` is an identity function
 * upstream (it returns its argument unchanged), so the module default is a plain
 * `{ id, setup }` object. Importing @opencode-ai/plugin would add a runtime
 * dependency this plugin has never needed and buys nothing.
 *
 * The v2 plugin API is beta and its ctx is still gaining hooks upstream. Every
 * capability below is feature-detected: on a build where `ctx.session.hook`,
 * `ctx.event.subscribe`, or `ctx.tool.transform` is absent, that capability logs
 * once and no-ops rather than throwing and taking down plugin activation.
 */

import {
  HANDSHAKE_TTL_MS,
  resolveConfig,
  buildFacts,
  effectiveConfig,
  memoizeAsync,
  extractPartsText,
  formatResults,
  fitByTokens,
  labelsEnv,
  extractLastTurn,
  lastAssistantFailed,
  describeSettings,
  renderStatus,
  createClient,
} from "./memini.js";

const INJECT_PREAMBLE =
  "Relevant long-term memory from memini (background context — prefer " +
  "current workspace state and the user's instructions):";

// messageText pulls the plain text out of one v2 request message, tolerating the
// shapes the beta may hand us: `content` as a string, `content` as an array of
// `{ type: "text", text }` parts, or a v1-style `parts` array.
function messageText(msg) {
  if (!msg) return "";
  if (typeof msg.content === "string") return msg.content.trim();
  if (Array.isArray(msg.content)) {
    return msg.content
      .map((p) => (p && typeof p.text === "string" ? p.text : ""))
      .join("\n")
      .trim();
  }
  if (Array.isArray(msg.parts)) return extractPartsText(msg.parts);
  return "";
}

// extractQueryFromRequest returns the latest user text from a request event's
// `messages`, falling back to the last non-empty message so a recall still fires
// on unusual message layouts. Exported for testing.
export function extractQueryFromRequest(event) {
  const messages = Array.isArray(event && event.messages) ? event.messages : [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const role = messages[i] && (messages[i].role || (messages[i].info && messages[i].info.role));
    if (role === "user") {
      const text = messageText(messages[i]);
      if (text) return text;
    }
  }
  for (let i = messages.length - 1; i >= 0; i--) {
    const text = messageText(messages[i]);
    if (text) return text;
  }
  return "";
}

// injectContext places the rendered memory block where the model will see it:
// the request event's `system` array first (transient, not persisted as a
// message part), falling back to prepending a system message. Returns true when
// it found a slot. Exported for testing.
export function injectContext(event, block) {
  if (!event || !block) return false;
  if (Array.isArray(event.system)) {
    event.system.push(block);
    return true;
  }
  if (Array.isArray(event.messages)) {
    event.messages.unshift({ role: "system", content: block });
    return true;
  }
  return false;
}

// fetchSessionMessages reads a session's message list through whichever server
// client method the running build exposes ([{info, parts}, …] is what
// extractLastTurn expects). The v2 ctx is "essentially a server client", but the
// exact accessor for GET /api/session/{id}/message isn't pinned in the beta, so
// try the plausible names and unwrap the common envelope shapes.
async function fetchSessionMessages(ctx, sessionID) {
  const session = ctx && ctx.session;
  if (!session) return [];
  const unwrap = (res) =>
    Array.isArray(res) ? res
    : Array.isArray(res && res.data) ? res.data
    : Array.isArray(res && res.messages) ? res.messages
    : [];
  const attempts = [
    () => session.messages && session.messages({ path: { id: sessionID } }),
    () => session.message && session.message({ path: { id: sessionID } }),
    () => session.messages && session.messages(sessionID),
  ];
  for (const attempt of attempts) {
    try {
      const res = await attempt();
      const arr = unwrap(res);
      if (arr.length) return arr;
    } catch {
      /* try the next accessor shape */
    }
  }
  return [];
}

export async function setup(ctx) {
  const options = (ctx && ctx.options) || {};
  // v2 setup(ctx) carries no worktree/directory the way v1's PluginInput did;
  // opencode runs the plugin in the project root, so cwd is the project dir —
  // the same input resolveConfig/deriveNamespace expect.
  const dir = process.cwd();
  const log = {
    warn: (message) => {
      console.error(`[memini] ${message}`);
    },
  };

  const cfg = resolveConfig(process.env, options, dir);
  const rest = createClient(cfg, log);

  // Handshake memoized per plugin instance (10-minute TTL), identical to the v1
  // plugin: a null handshake (fail-soft) falls back to cfg's local resolution.
  const getHandshake = memoizeAsync(
    () => rest.handshake(buildFacts(dir, process.env)),
    HANDSHAKE_TTL_MS,
  );
  const currentConfig = async () => effectiveConfig(cfg, await getHandshake());

  // Warm the connection (DNS/TCP/TLS) so a cold start doesn't eat the first
  // recall budget. Silent: even a 404 warms the path.
  if (cfg.recall || cfg.capture) {
    try {
      fetch(`${rest.baseUrl}/healthz`, { signal: AbortSignal.timeout(3000) }).catch(() => {});
    } catch {
      /* ignore */
    }
  }

  // Assistant ids already captured, so repeated idle events for one turn don't
  // write duplicates. Memory ids already injected per session, so an unchanged
  // match isn't re-injected turn after turn. Both bounded for a long-lived host.
  const captured = new Set();
  const injectedBySession = new Map();
  const MAX_TRACKED_SESSIONS = 200;
  const rememberInjected = (session, ids) => {
    let seen = injectedBySession.get(session);
    if (!seen) {
      seen = new Set();
      injectedBySession.set(session, seen);
      while (injectedBySession.size > MAX_TRACKED_SESSIONS) {
        const oldest = injectedBySession.keys().next().value;
        if (oldest === undefined) break;
        injectedBySession.delete(oldest);
      }
    }
    for (const id of ids) if (id) seen.add(id);
  };

  const cleanups = [];
  const disposeReg = (reg) => {
    if (typeof reg === "function") cleanups.push(reg);
    else if (reg && typeof reg.dispose === "function") cleanups.push(() => reg.dispose());
  };

  // --- RECALL: ctx.session.hook("request") -------------------------------
  //
  // opencode awaits this hook immediately before model dispatch (a throw fails
  // the turn — the doc's "a hook failure fails the operation it intercepts"), so
  // the callback swallows its own errors and races the search against
  // recall_budget_ms: if memini is slow, the turn proceeds without memory rather
  // than freezing for the full timeout_ms.
  if (ctx && ctx.session && typeof ctx.session.hook === "function") {
    const reg = await ctx.session.hook("request", async (event) => {
      try {
        const live = await currentConfig();
        if (!live.recall) return;
        const query = extractQueryFromRequest(event);
        if (!query) return;
        const sessionID = event.sessionID || event.sessionId || (event.session && event.session.id) || "";

        const body = { query, limit: live.recall_limit };
        // Exclude this session's own captured turns: they're still in the live
        // context, so recalling them just echoes the conversation back a turn
        // behind. Past sessions still recall.
        if (sessionID) body.exclude_metadata = { session_id: sessionID };
        if (live.recall_min_score > 0) body.min_score = live.recall_min_score;

        // Blocking, like v1's chat.message: opencode awaits this hook before
        // dispatch. postJson is bounded by cfg.timeout_ms and fail-soft, so a
        // slow/unreachable memini degrades to no memory this turn, never a throw.
        const result = await rest.postJson("/v1/search", body, live.namespace);

        const floor = live.recall_min_score > 0 ? live.recall_min_score : 0;
        let rawHits = Array.isArray(result && result.results) ? result.results : [];
        if (sessionID) {
          const seen = injectedBySession.get(sessionID);
          if (seen && seen.size) rawHits = rawHits.filter((r) => !seen.has(r && r.memory && r.memory.id));
        }
        const filtered =
          floor > 0
            ? rawHits.filter((r) => (typeof (r && r.score) === "number" ? r.score : 0) >= floor)
            : rawHits;
        const hits = formatResults(filtered, live.recall_limit, labelsEnv());
        if (hits.length === 0) return;
        const fit = fitByTokens(hits, live.recall_max_tokens);
        if (fit.items.length === 0) return;

        const lines = [INJECT_PREAMBLE, ...fit.items];
        if (result && result.degraded) {
          lines.push(
            `[memini: ${result.note || "semantic search unavailable — results are keyword-only and may be incomplete"}]`,
          );
        }
        if (fit.dropped > 0) lines.push(`[... ${fit.dropped} item(s) truncated by token budget]`);

        if (injectContext(event, lines.join("\n")) && sessionID) {
          rememberInjected(
            sessionID,
            filtered.map((r) => r && r.memory && r.memory.id).filter(Boolean),
          );
        }
      } catch (error) {
        log.warn(`request hook failed: ${String(error)}`);
      }
    });
    disposeReg(reg);
  } else if (cfg.recall) {
    log.warn("recall unavailable: ctx.session.hook is not present on this opencode build");
  }

  // --- STATUS TOOL: ctx.tool.transform(t => t.add(...)) ------------------
  if (ctx && ctx.tool && typeof ctx.tool.transform === "function") {
    const reg = await ctx.tool.transform((tools) => {
      tools.add({
        name: "memini_status",
        description:
          "Show the memini memory settings in force for this project: which namespace memories " +
          "are written to and recalled from, where that namespace came from (the namespace option, " +
          "MEMINI_NAMESPACE, a server-resolved handshake, or the git worktree fallback), what it " +
          "would be without the env/option pin, and any misconfiguration worth flagging. Read-only; " +
          "secrets are redacted. Call it when the user asks what memini is doing, why a memory " +
          "cannot be recalled, or which namespace is in use.",
        jsonSchema: { type: "object", properties: {}, additionalProperties: false },
        options: { codemode: false },
        execute: async () => {
          try {
            const report = describeSettings(process.env, options, dir);
            // Overlay the live, handshake-aware values so the tool reports what
            // the hooks actually did on their last handshake.
            const live = await currentConfig();
            report.namespace.effective = live.namespace;
            report.namespace.source = live.namespace_source;
            report.memory.recall = live.recall;
            report.memory.capture = live.capture;
            report.memory.recall_limit = live.recall_limit;
            report.memory.recall_max_tokens = live.recall_max_tokens;
            report.memory.recall_min_score = live.recall_min_score;
            const text = renderStatus(report);
            return {
              structured: {
                namespace: report.namespace.effective,
                source: report.namespace.source,
              },
              content: [{ type: "text", text }],
            };
          } catch (error) {
            return { content: [{ type: "text", text: `memini status failed: ${String(error)}` }] };
          }
        },
      });
    });
    disposeReg(reg);
  } else {
    log.warn("memini_status unavailable: ctx.tool.transform is not present on this opencode build");
  }

  // --- CAPTURE: ctx.event.subscribe("session.idle") ----------------------
  //
  // Detached: the docs say not to await an infinite stream inside setup. We spawn
  // the consumer and hand back an AbortController-based cleanup.
  const handleIdle = async (event) => {
    const live = await currentConfig();
    if (!live.capture) return;
    // The stream may be narrower or broader than session.idle depending on how
    // subscribe filters; guard on the type when present.
    if (event && event.type && event.type !== "session.idle") return;
    const sessionID =
      (event && event.properties && event.properties.sessionID) || (event && event.sessionID);
    if (!sessionID) return;
    const messages = await fetchSessionMessages(ctx, sessionID);
    const { userText, assistantText, assistantID } = extractLastTurn(messages);
    if (!userText || !assistantText) return;
    if (assistantID && captured.has(assistantID)) return;
    const metadata = { source: "opencode", session_id: sessionID, format: "turn" };
    if (lastAssistantFailed(messages)) metadata.failed = true;
    const stored = await rest.postJson(
      "/v1/memories",
      {
        content: `${userText.slice(0, 1000)}\n\n${assistantText.slice(0, 3000)}`,
        tags: ["opencode"],
        metadata,
      },
      live.namespace,
    );
    if (stored !== null && assistantID) captured.add(assistantID);
  };

  if (cfg.capture && ctx && ctx.event && typeof ctx.event.subscribe === "function") {
    const controller = new AbortController();
    const task = (async () => {
      let stream;
      try {
        stream = ctx.event.subscribe("session.idle");
      } catch (error) {
        log.warn(`capture unavailable: ctx.event.subscribe threw: ${String(error)}`);
        return;
      }
      if (!stream || typeof stream[Symbol.asyncIterator] !== "function") {
        log.warn("capture unavailable: ctx.event.subscribe did not return an async iterable");
        return;
      }
      try {
        for await (const event of stream) {
          if (controller.signal.aborted) break;
          try {
            await handleIdle(event);
          } catch (error) {
            log.warn(`event hook failed: ${String(error)}`);
          }
        }
      } catch (error) {
        if (!controller.signal.aborted) log.warn(`event stream failed: ${String(error)}`);
      }
    })();
    cleanups.push(async () => {
      controller.abort();
      await task.catch(() => {});
    });
  } else if (cfg.capture) {
    log.warn("capture unavailable: ctx.event.subscribe is not present on this opencode build");
  }

  // Cleanup: dispose hook/tool registrations and stop the capture consumer when
  // the plugin is disabled, reloaded, or shut down.
  return async () => {
    for (const cleanup of cleanups) {
      try {
        await cleanup();
      } catch {
        /* ignore cleanup failures */
      }
    }
  };
}

// Plugin.define is identity upstream, so a plain { id, setup } is the module
// default — no @opencode-ai/plugin dependency needed. See the file header.
export default { id: "memini", setup };
