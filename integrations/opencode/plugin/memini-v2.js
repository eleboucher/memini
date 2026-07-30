/**
 * memini memory plugin for opencode v2 (the Plugin.define / `setup` API).
 *
 * This is the v2 sibling of memini.js. It targets the v2 plugin contract as
 * verified against opencode2 v0.0.0-next-16502 (@opencode-ai/plugin `next`),
 * which differs from the docs at https://opencode.ai/v2/docs/build/plugins in
 * several places — every shape below is feature-detected and was confirmed
 * against the live runtime:
 *
 *   - RECALL — ctx.session.hook("context", …): the docs call this hook
 *     "request", but this build fires "context" (unknown names register without
 *     error and simply never fire). The event is
 *     { sessionID, agent, model, system, messages, tools }, where `system` is an
 *     array of { type: "text", text } parts and `messages` are AI-SDK-shaped
 *     ({ id, role, content: [{ type: "text", text } …] }). The hook fires once
 *     per model DISPATCH — i.e. again on every tool-loop continuation step — so
 *     recall is deduplicated per TURN (last user message id): the search runs
 *     once per user message and the rendered block is re-injected from cache on
 *     continuation dispatches of the same turn. This is the v2 equivalent of
 *     v1's `chat.message`.
 *   - CAPTURE — ctx.event.subscribe() (no type argument; the whole public
 *     server event stream, across every project the service hosts). There is no
 *     `session.idle` in v2: a turn's boundary is the
 *     `session.execution.succeeded` / `.failed` / `.interrupted` event. There is
 *     also no message-listing method on ctx.session (create/get/prompt/command/
 *     synthetic/generate/interrupt only; session.get returns SessionInfo with
 *     no messages). Capture is therefore event-driven: the user text comes from
 *     `session.input.admitted` (data.input.data.text) and the assistant text
 *     from `session.text.ended` parts (keyed by assistantMessageID + ordinal),
 *     flushed to memini when the execution terminal event arrives. v1's
 *     failed-turn marking maps to execution.failed / execution.interrupted.
 *   - STATUS TOOL — ctx.tool.transform(t => t.add(…)): `add` takes ONE
 *     structural Tool.Info object ({ name, input, description, execute,
 *     options? }) — not the docs' three-argument add(name, def, options). The
 *     input schema field is `input` (raw JSON Schema is fine) and the result is
 *     { content: [{ type: "text", text }] }.
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
 * once and no-ops rather than throwing and taking down plugin activation. Note
 * that ctx.app has no `log` method on current builds (it is { name, version,
 * channel }), so diagnostics stay silent — they are best-effort by design.
 *
 * KNOWN LIMITATION (beta): the v2 service hosts every project in ONE process
 * with ONE plugin instance, and hooks/events are service-global. The namespace
 * is resolved from the plugin process's cwd (typically the service's start
 * directory), so sessions from other projects would recall/capture against the
 * same namespace. Pin `MEMINI_NAMESPACE` if needed and re-check the upstream
 * ctx as it gains per-location routing.
 */

import {
  HANDSHAKE_TTL_MS,
  resolveConfig,
  buildFacts,
  effectiveConfig,
  buildTurnCapture,
  memoizeAsync,
  extractPartsText,
  formatResults,
  fitByTokens,
  labelsEnv,
  describeSettings,
  renderStatus,
  createClient,
  injectedIdentity,
  injectedSuppressed,
  postSearchWithFloor,
  resolveSessionAncestry,
} from "./memini.js";

const INJECT_PREAMBLE =
  "Relevant long-term memory from memini (background context — prefer " +
  "current workspace state and the user's instructions):";
const BUDGET_EXPIRED = Symbol("memini-recall-budget-expired");
// OpenCode activates a plugin per location while event.subscribe() is global.
// Assistant message IDs are globally unique, so this prevents duplicate writes.
const capturedAssistantIDs = new Set();
const MAX_CAPTURED_ASSISTANT_IDS = 1000;

function claimCapturedAssistant(id) {
  if (capturedAssistantIDs.has(id)) return false;
  capturedAssistantIDs.add(id);
  while (capturedAssistantIDs.size > MAX_CAPTURED_ASSISTANT_IDS) {
    const oldest = capturedAssistantIDs.keys().next().value;
    if (oldest === undefined) break;
    capturedAssistantIDs.delete(oldest);
  }
  return true;
}

export function resetForTests() {
  capturedAssistantIDs.clear();
}

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

// extractQueryFromRequest returns the latest user text from a context event's
// `messages`, falling back to the last non-empty message so a recall still fires
// on unusual message layouts. Exported for testing.
export function extractQueryFromRequest(event) {
  const lastUser = lastUserMessage(event);
  if (lastUser) {
    const text = messageText(lastUser);
    if (text) return text;
  }
  const messages = Array.isArray(event && event.messages) ? event.messages : [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const text = messageText(messages[i]);
    if (text) return text;
  }
  return "";
}

// lastUserMessage returns the most recent user-role message, or null.
function lastUserMessage(event) {
  const messages = Array.isArray(event && event.messages) ? event.messages : [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const role = messages[i] && (messages[i].role || (messages[i].info && messages[i].info.role));
    if (role === "user") return messages[i];
  }
  return null;
}

// turnKey identifies a user TURN (as opposed to one model dispatch): the
// context hook re-fires on every tool-loop continuation with the same last
// user message. Exported for testing.
export function turnKey(event, query) {
  const sessionID = event && event.sessionID ? String(event.sessionID) : "";
  const lastUser = lastUserMessage(event);
  const id = lastUser && (lastUser.id || (lastUser.info && lastUser.info.id));
  return `${sessionID}|${id || query}`;
}

// injectContext places the rendered memory block where the model will see it:
// the event's `system` array first (an array of { type: "text", text } parts,
// transient and not persisted as a message), falling back to prepending a
// system message. Returns true when it found a slot. Exported for testing.
export function injectContext(event, block) {
  if (!event || !block) return false;
  if (Array.isArray(event.system)) {
    event.system.push({ type: "text", text: block });
    return true;
  }
  if (Array.isArray(event.messages)) {
    event.messages.unshift({ role: "system", content: block });
    return true;
  }
  return false;
}

function contextAlreadyInjected(event) {
  return Array.isArray(event && event.system) && event.system.some((part) => {
    const text = typeof part === "string" ? part : part && part.text;
    return typeof text === "string" && text.startsWith(INJECT_PREAMBLE);
  });
}

export async function setup(ctx) {
  const options = (ctx && ctx.options) || {};
  // v2 setup(ctx) carries no worktree/directory the way v1's PluginInput did;
  // opencode runs the plugin in the project root, so cwd is the project dir —
  // the same input resolveConfig/deriveNamespace expect.
  const dir = process.cwd();
  const emitLog = async (level, message) => {
    try {
      if (typeof ctx?.app?.log !== "function") return;
      await ctx.app.log({ body: { service: "memini", level, message } });
    } catch {
      // Logging is best-effort and must never affect plugin behavior.
    }
  };
  const log = {
    warn: (message) => {
      void emitLog("warn", message);
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

  // Assistant ids already captured, so repeated idle events for one turn don't
  // write duplicates. Memory ids already injected per session — the enforce
  // core's { n, ids } shape (n = prompt counter, bumped once per request-hook
  // fire; ids maps memory id → { h, at, n }), judged by injectedSuppressed
  // against the inject_cooldown_ms / inject_cooldown_prompts windows: an
  // unchanged match is suppressed while inside EITHER window, re-served once
  // BOTH lapse, and re-served immediately when its content changed (h
  // mismatch). Both maps bounded for a long-lived host.
  const injectedBySession = new Map(); // session -> { n, ids: Map<id, {h, at, n}> }
  const MAX_TRACKED_SESSIONS = 200;
  const MAX_INJECTED_PER_SESSION = 200;
  const sessionSeen = (session) => {
    let state = injectedBySession.get(session);
    if (!state) {
      state = { n: 0, ids: new Map() };
      injectedBySession.set(session, state);
      while (injectedBySession.size > MAX_TRACKED_SESSIONS) {
        const oldest = injectedBySession.keys().next().value;
        if (oldest === undefined) break;
        injectedBySession.delete(oldest);
      }
    }
    return state;
  };
  const rememberInjected = (state, hits) => {
    const now = Date.now();
    for (const r of hits) {
      const id = r?.memory?.id;
      if (!id) continue;
      // delete+set refreshes the stamp and the insertion order, so the size
      // cap evicts the least-recently-shown id first.
      state.ids.delete(id);
      state.ids.set(id, { h: injectedIdentity(r?.memory), at: now, n: state.n });
    }
    while (state.ids.size > MAX_INJECTED_PER_SESSION) {
      const oldest = state.ids.keys().next().value;
      if (oldest === undefined) break;
      state.ids.delete(oldest);
    }
  };

  const cleanups = [];
  const disposeReg = (reg) => {
    if (typeof reg === "function") cleanups.push(reg);
    else if (reg && typeof reg.dispose === "function") cleanups.push(() => reg.dispose());
  };

  // --- RECALL: ctx.session.hook("context") --------------------------------
  //
  // opencode awaits this hook immediately before model dispatch (the doc's "a
  // hook failure fails the operation it intercepts"), so the callback swallows
  // its own errors and races the search against recall_budget_ms: if memini is
  // slow, the turn proceeds without memory rather than freezing for the full
  // timeout_ms.
  //
  // The build verified against fires "context"; the docs name the same hook
  // "request". Unknown names register without error and never fire, so both
  // are registered: on any given build only the implemented one runs, and a
  // build that ever fires both for one dispatch is neutralized by the
  // per-turn dedup plus the injected-event WeakSet below.
  if (ctx && ctx.session && typeof ctx.session.hook === "function") {
    // Events whose system/messages this hook generation already injected —
    // guards against a build that fires both names for one dispatch.
    const injectedEvents = new WeakSet();
    const handleRequest = async (event) => {
      try {
        const live = await currentConfig();
        if (!live.recall) return;
        const query = extractQueryFromRequest(event);
        if (!query) return;
        const sessionID = event.sessionID || event.sessionId || (event.session && event.session.id) || "";
        const seen = sessionID ? sessionSeen(sessionID) : null;

        // One USER TURN == one prompt for the cooldown's prompt dimension. The
        // hook re-fires per tool-loop step of the same turn, so dedupe on the
        // last user message: a repeat turnKey skips the search and the counter,
        // but re-injects this turn's cached block into the new dispatch's
        // system (each dispatch rebuilds its request from scratch).
        const key = turnKey(event, query);
        if (seen && seen.lastTurnKey === key) {
          if (seen.block && !contextAlreadyInjected(event) && !injectedEvents.has(event) && injectContext(event, seen.block)) {
            injectedEvents.add(event);
          }
          return;
        }
        if (seen) {
          seen.lastTurnKey = key;
          seen.block = undefined;
          seen.n += 1;
        }
        const cooldownOpts = () => ({
          now: Date.now(),
          counter: seen ? seen.n : 0,
          cooldownMs: live.inject_cooldown_ms,
          cooldownPrompts: live.inject_cooldown_prompts,
        });

        const body = { query, limit: live.recall_limit };
        // Exclude this session's own captured turns: they're still in the live
        // context, so recalling them just echoes the conversation back a turn
        // behind. Past sessions still recall.
        if (sessionID) body.exclude_metadata = { session_id: sessionID };
        // inject_recall_min_score floors the FINAL composite score server-side
        // via min_rank_score (not the fused-scale min_score), matching the
        // Claude Code plugin. A knob >= 1 is out of the server's range, so it
        // clamps to a client-only floor rather than 400ing every search.
        const rankFloorInRange = live.recall_min_score > 0 && live.recall_min_score < 1;
        if (rankFloorInRange) body.min_rank_score = live.recall_min_score;

        // Blocking, like v1's chat.message: opencode awaits this hook before
        // dispatch. postSearchWithFloor is bounded by cfg.timeout_ms and
        // fail-soft, and on an older server's 400 it retries once with
        // min_rank_score stripped (v2 sends no exclude_ids), so a slow or
        // out-of-date memini degrades to no memory this turn, never a throw.
        const searchPromise = postSearchWithFloor(
          rest.postJson,
          body,
          live.namespace,
        );
        // Keep the rejection handler attached even after the request budget
        // expires: late results are discarded, but late errors remain visible.
        const settled = searchPromise.catch((error) => {
          log.warn(`memini: ${String(error)}`);
          return { data: null, rankFloorStripped: false };
        });
        let search;
        if (live.recall_budget_ms > 0) {
          let timer;
          const budget = new Promise((resolve) => {
            timer = setTimeout(() => resolve(BUDGET_EXPIRED), live.recall_budget_ms);
          });
          search = await Promise.race([settled, budget]);
          clearTimeout(timer);
          if (search === BUDGET_EXPIRED) {
            log.warn(`recall exceeded its ${live.recall_budget_ms}ms budget; late results discarded`);
            return;
          }
        } else {
          search = await settled;
        }
        const { data: result, rankFloorStripped } = search;

        // Client composite floor is a fallback ONLY: it runs when the knob was
        // clamped to client-only (>= 1) or the retry stripped min_rank_score. A
        // server that enforced the floor is authoritative and not re-filtered.
        const serverEnforcedFloor = rankFloorInRange && !rankFloorStripped;
        const floor = live.recall_min_score > 0 && !serverEnforcedFloor ? live.recall_min_score : 0;
        let rawHits = Array.isArray(result && result.results) ? result.results : [];
        // Windowed cooldown, judged PER HIT against its content identity: an
        // in-window unchanged hit is dropped, a lapsed one re-serves, and an
        // UPDATED one (h mismatch) bypasses the window and re-injects.
        if (seen && seen.ids.size) {
          const opts = cooldownOpts();
          rawHits = rawHits.filter((r) => {
            const entry = seen.ids.get(r && r.memory && r.memory.id);
            return !(entry && injectedSuppressed(entry, injectedIdentity(r && r.memory), opts));
          });
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

        const block = lines.join("\n");
        if (!contextAlreadyInjected(event) && injectContext(event, block)) {
          injectedEvents.add(event);
          if (seen) {
            // Cache for re-injection on this turn's continuation dispatches,
            // and record only the slice formatResults actually renders,
            // stamped {h, at, n} so the windowed cooldown can judge later.
            seen.block = block;
            rememberInjected(seen, filtered.slice(0, live.recall_limit || 3));
          }
        }
      } catch (error) {
        log.warn(`request hook failed: ${String(error)}`);
      }
    };
    for (const name of ["context", "request"]) {
      const reg = await ctx.session.hook(name, handleRequest);
      disposeReg(reg);
    }
  } else if (cfg.recall) {
    log.warn("recall unavailable: ctx.session.hook is not present on this opencode build");
  }

  // --- STATUS TOOL: ctx.tool.transform(t => t.add(info)) ------------------
  //
  // `add` takes ONE structural Tool.Info: { name, input, description, execute,
  // options? } (verified against next-16502; the docs' three-argument
  // add(name, def, options) is wrong for this build). `input` is a raw JSON
  // Schema; the result is { content: [{ type: "text", text }] }.
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
        input: { type: "object", properties: {}, additionalProperties: false },
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
            return { content: [{ type: "text", text }] };
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

  // --- CAPTURE: ctx.event.subscribe() -------------------------------------
  //
  // Detached: the docs say not to await an infinite stream inside setup. We
  // spawn the consumer and hand back an AbortController-based cleanup.
  //
  // There is no `session.idle` and no message list accessor in v2, so a turn
  // is reconstructed from the public event stream:
  //   session.input.admitted      -> user text    (data.input.data.text)
  //   session.text.ended          -> assistant part (data: sessionID,
  //                                  assistantMessageID, ordinal, text)
  //   session.execution.succeeded -> flush        (failed / interrupted flush
  //                                  with metadata.failed = true)
  // An execution event with no pending user text (compaction, synthetic
  // input, a turn that started before this plugin generation loaded) captures
  // nothing — same as v1's skip on an empty turn.
  const MAX_TRACKED_CAPTURE = 200;
  const pendingBySession = new Map(); // sessionID -> { userText, texts: Map<aid, Map<ordinal, text>>, lastAid }
  const pendingState = (sessionID) => {
    let state = pendingBySession.get(sessionID);
    if (!state) {
      state = { userText: "", texts: new Map(), lastAid: "" };
      pendingBySession.set(sessionID, state);
      while (pendingBySession.size > MAX_TRACKED_CAPTURE) {
        const oldest = pendingBySession.keys().next().value;
        if (oldest === undefined) break;
        pendingBySession.delete(oldest);
      }
    }
    return state;
  };
  const flushTurn = async (sessionID, failed) => {
    const live = await currentConfig();
    if (!live.capture) return;
    const pending = pendingBySession.get(sessionID);
    pendingBySession.delete(sessionID);
    if (!pending || !pending.userText || !pending.lastAid) return;
    const parts = pending.texts.get(pending.lastAid);
    const assistantText = parts
      ? [...parts.entries()].sort((a, b) => a[0] - b[0]).map(([, text]) => text).join("\n").trim()
      : "";
    if (!assistantText) return;
    const assistantID = pending.lastAid;
    if (!claimCapturedAssistant(assistantID)) return;
    const ancestry = await resolveSessionAncestry(
      {
        get: (input) => {
          const id = typeof input === "string" ? input : input?.sessionID || input?.path?.id;
          return ctx.session.get({ sessionID: id });
        },
      },
      sessionID,
    );
    if (ancestry.session_type !== "root" && !live.capture_child_sessions) {
      capturedAssistantIDs.delete(assistantID);
      return;
    }
    const metadata = { source: "opencode", session_id: sessionID, format: "turn", ...ancestry };
    if (failed) metadata.failed = true;
    const stored = await rest.postJson(
      "/v1/memories",
      {
        content: buildTurnCapture(pending.userText, assistantText, live.capture_user_max_chars, live.capture_assistant_max_chars),
        tags: ["opencode"],
        metadata,
      },
      live.namespace,
    );
    if (stored === null) capturedAssistantIDs.delete(assistantID);
  };
  const handleEvent = async (event) => {
    const type = event && event.type;
    const data = (event && event.data) || {};
    const sessionID = data.sessionID || (data.properties && data.properties.sessionID);
    if (!sessionID) return;
    if (type === "session.input.admitted") {
      const text = data.input && data.input.type === "user" ? data.input.data && data.input.data.text : "";
      const pending = pendingState(sessionID);
      pending.userText = typeof text === "string" ? text : "";
      pending.texts = new Map();
      pending.lastAid = "";
      return;
    }
    if (type === "session.text.ended") {
      if (!data.assistantMessageID || typeof data.text !== "string") return;
      const pending = pendingState(sessionID);
      let parts = pending.texts.get(data.assistantMessageID);
      if (!parts) {
        parts = new Map();
        pending.texts.set(data.assistantMessageID, parts);
      }
      parts.set(data.ordinal ?? parts.size, data.text);
      pending.lastAid = data.assistantMessageID;
      return;
    }
    if (type === "session.execution.succeeded") return flushTurn(sessionID, false);
    if (type === "session.execution.failed" || type === "session.execution.interrupted") {
      return flushTurn(sessionID, true);
    }
  };

  if (cfg.capture && ctx && ctx.event && typeof ctx.event.subscribe === "function") {
    const controller = new AbortController();
    let iterator;
    const task = (async () => {
      let stream;
      try {
        stream = ctx.event.subscribe();
      } catch (error) {
        log.warn(`capture unavailable: ctx.event.subscribe threw: ${String(error)}`);
        return;
      }
      if (!stream || typeof stream[Symbol.asyncIterator] !== "function") {
        log.warn("capture unavailable: ctx.event.subscribe did not return an async iterable");
        return;
      }
      try {
        iterator = stream[Symbol.asyncIterator]();
        for (;;) {
          if (controller.signal.aborted) break;
          const { value: event, done } = await iterator.next();
          if (done) break;
          try {
            await handleEvent(event);
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
      try {
        if (iterator && typeof iterator.return === "function") await iterator.return();
      } catch {
        /* closing the event stream is best-effort */
      }
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
