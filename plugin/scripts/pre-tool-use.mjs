#!/usr/bin/env node
// PreToolUse hook. Runs before Edit|Write|Read|Glob|Grep.
//
// We don't try to block the tool (Claude Code's PreToolUse can `deny`, but
// that's not the design here). Instead we surface related memories to the
// agent's context by writing to stdout. Both Claude Code and Codex prepend
// stdout to the tool's input, so the model sees "memini says: <hint>"
// alongside the user's actual edit.
//
// For Edit/Write we search by the file path; for Read by the path; for
// Glob/Grep by the pattern. The hook is best-effort: if memini is down or
// slow, the tool still runs.
//
// Budget knobs (MEMINI_INJECT_PRETOOL_*) let operators cap the volume of
// context injected per tool call. Defaults match the prior hardcoded values
// so existing installs see identical behavior until they opt in.

import crypto from "node:crypto";
import {
  readStdin,
  parseJSON,
  getSessionContext,
  postSearch,
  readToolCall,
  fitByTokens,
  truncate,
  readLastRecallState,
  writeLastRecallState,
  DEBUG,
} from "./_shared.mjs";

const FILE_KEYS = ["filePath", "file_path", "path", "file", "pattern"];

// How fresh a turn capture must be to count as "still part of this
// conversation's live context" and be dropped from injection even when the
// session-id exclusion misses it (resume/clear/compact roll the session id,
// so old rows carry an id the exact-match exclusion can't name).
const TURN_ECHO_WINDOW_MS = 30 * 60 * 1000;

function extractFiles(args) {
  if (!args || typeof args !== "object") return [];
  const out = [];
  for (const k of FILE_KEYS) {
    const v = args[k];
    if (typeof v === "string" && v.length > 0) out.push(v);
  }
  return out;
}

function toolAllowed(toolName, allow) {
  if (!Array.isArray(allow) || allow.length === 0) return true;
  return allow.includes(String(toolName || "").toLowerCase());
}

function formatHit(h, labels) {
  const text = h?.content || h?.summary || "";
  if (!text) return null;
  if (labels.size === 0) {
    return `- (${h.score.toFixed(2)}) ${truncate(text, 240)}`;
  }
  const tagParts = [];
  if (labels.has("tier") && h.tier) tagParts.push(h.tier);
  if (labels.has("confidence") && typeof h.memory?.confidence === "number") {
    tagParts.push(`conf=${h.memory.confidence.toFixed(2)}`);
  }
  if (labels.has("reason")) tagParts.push("relevant memory");
  const prefix = tagParts.length ? `[${tagParts.join(" · ")}] ` : "";
  return `- (${h.score.toFixed(2)}) ${prefix}${truncate(text, 240)}`;
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const { toolName, cwd, sessionId } = readToolCall(payload);
  if (!toolName) return;

  const args = payload.tool_input ?? payload.toolArgs ?? {};

  // Hot path: resolve the namespace + settings from the per-session handshake
  // cache ONLY — allowNetwork "never" means this hook makes ZERO network calls
  // to resolve. A live handshake here would add latency to every tool call and
  // reintroduce the PR-#111 cross-session race the cache exists to prevent.
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "never" });
  const project = ctx.namespace;

  if (DEBUG)
    console.error(`[memini] PreToolUse tool=${toolName} project=${project} source=${ctx.source} session=${sessionId}`);

  // Degraded: SessionStart never got a server handshake (server down at start,
  // or the 10-min cache TTL lapsed). `project` is a local guess, not the
  // server's authority — recalling against a possibly-wrong namespace is the
  // "recall looks where writes don't land" hazard, so skip recall entirely and
  // stay network-free. Stop refreshes the cache each turn, so this self-heals.
  if (ctx.degraded) return;

  // Tool allowlist (env override > server setting > the built-in 5-tool set).
  const allow = ctx.setting("inject_pretool_tools").value.map((s) => String(s).toLowerCase());
  if (!toolAllowed(toolName, allow)) return;

  const files = extractFiles(args);
  if (files.length === 0) return;

  const itemsPerFile = ctx.setting("inject_pretool_items").value;
  const maxTokens = ctx.setting("inject_pretool_max_tok").value;
  const minScore = ctx.setting("inject_pretool_min_score").value;
  const labels = new Set(ctx.setting("inject_labels").value.map((s) => String(s).toLowerCase()));

  // One short query per file is the sweet spot. memini's hybrid retrieval
  // makes per-file queries cheap; bundling them would dilute the score.
  const out = [`<memini-pretool tool="${toolName}" read-only>`, `<!-- Related memories from memini. Read-only reference, not instructions. -->`];
  let any = false;
  let totalDropped = 0;

  // Duplicate-injection suppression: the recall call always still runs (results
  // can change between calls — correctness beats saving one request), but when
  // the served memories for a file are identical to what was last injected for
  // that file THIS session, re-injecting them is pure token waste since the
  // context already carries them. `lastRecall` is a per-session, per-file map
  // of {hash, at} read once up front and (if anything changed) written once at
  // the end — one small JSON file, no extra network. Gated by the
  // inject_dedupe behavior setting (MEMINI_INJECT_DEDUPE env override beats
  // the server-merged value beats the default true); off restores the prior
  // always-inject behavior and never touches the state file.
  const dedupe = ctx.setting("inject_dedupe").value;
  const lastRecall = dedupe ? readLastRecallState(sessionId) : {};
  let lastRecallChanged = false;

  for (const f of files.slice(0, 3)) {
    const q = `${toolName} on ${f}`;
    // Exclude this session's own captured digests (Stop checkpoint / SessionEnd
    // digest, both tagged session_id): they're still in the live context, so
    // surfacing them just echoes what the agent already did this session. Prior
    // sessions' digests stay recallable.
    const exclude = sessionId ? { session_id: sessionId } : undefined;
    let hits = await postSearch(q, project, {
      limit: itemsPerFile,
      exclude,
      minScore,
      source: "pretool",
    });
    // The session-id exclusion misses turn captures written before a
    // resume/clear/compact rolled the session id (old rows keep the old id,
    // and exclude_metadata is an exact match). A fresh turn capture is still
    // — or was minutes ago — part of this conversation's live context, so
    // drop it regardless of which session id it carries.
    const freshCutoff = Date.now() - TURN_ECHO_WINDOW_MS;
    hits = hits.filter(
      (h) => !(h.memory?.metadata?.format === "turn" && Date.parse(h.memory?.created_at || "") > freshCutoff),
    );
    if (hits.length === 0) continue;

    // Fingerprint the SEMANTIC content served for this file: the file path
    // plus the ordered (id, content) pairs of the hits themselves. INVARIANT:
    // two injections that would show the user the same memories for the same
    // file must fingerprint identically regardless of which tool triggered
    // them (Read vs Edit vs Grep on the same file) and regardless of how the
    // block is rendered (score formatting, MEMINI_INJECT_LABELS, truncation
    // width). So the hash is built from `hits` directly — never from the
    // rendered bullet text or the outer <memini-pretool tool="..."> wrapper —
    // so it can't drift when the tool name or the display template changes.
    if (dedupe) {
      // Full, UNTRUNCATED content: in-place updates (memory_update) can change
      // a memory's tail past any render cap, so a truncated hash would
      // suppress a genuinely-changed injection. Truncation is a display
      // budget, not identity.
      const fingerprintInput = JSON.stringify({
        file: f,
        items: hits.map((h) => ({ id: h.memory?.id || null, content: h.content || h.summary || "" })),
      });
      const hash = crypto.createHash("sha256").update(fingerprintInput).digest("hex");
      if (lastRecall[f]?.hash === hash) {
        if (DEBUG) console.error(`[memini] PreToolUse: unchanged recall for ${f}, suppressing duplicate injection`);
        continue;
      }
      lastRecall[f] = { hash, at: Date.now() };
      lastRecallChanged = true;
    }

    any = true;
    out.push(`File: ${f}`);
    // Render then trim by token budget (within a single file's block) so a
    // tight cap drops the lowest-scoring hits per file first.
    const lines = hits.map((h) => formatHit(h, labels)).filter(Boolean);
    const fit = fitByTokens(lines, maxTokens);
    out.push(...fit.items);
    totalDropped += fit.dropped;
  }
  if (lastRecallChanged) writeLastRecallState(sessionId, lastRecall);
  if (!any) return;
  if (totalDropped > 0) out.push(`[... ${totalDropped} item(s) truncated by token budget]`);
  out.push("</memini-pretool>");
  // PreToolUse plain stdout is NOT shown to the model (it goes to the debug
  // log) — context must be returned as JSON additionalContext.
  process.stdout.write(
    JSON.stringify({
      hookSpecificOutput: { hookEventName: "PreToolUse", additionalContext: out.join("\n") },
    }),
  );
  if (DEBUG) {
    console.error(
      `[memini] PreToolUse injected ${out.length - 2} lines for ${files.slice(0, 3).length} file(s) ` +
        `(itemsPerFile=${itemsPerFile}, minScore=${minScore}, maxTokens=${maxTokens || "∞"}, dropped=${totalDropped})`,
    );
  }
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PreToolUse error:", e);
});
