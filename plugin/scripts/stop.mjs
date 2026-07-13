#!/usr/bin/env node
// Stop hook. Two jobs:
//   1. Write a short-lived (working tier, 24h) checkpoint distilled from the
//      buffered tool events so a long session doesn't lose its tail. Unlike
//      SessionEnd it does NOT delete the buffer — the session may continue and
//      SessionEnd writes the durable digest.
//   2. Auto-save nudge: every MEMINI_AUTO_SAVE_INTERVAL user messages, block
//      the agent once and ask it to persist durable facts/decisions via the
//      memini memory_remember MCP tool. On by default; opt out with
//      MEMINI_AUTO_SAVE=0.

import fs from "node:fs";
import crypto from "node:crypto";
import {
  readStdin,
  parseJSON,
  getSessionContext,
  writeSessionCwd,
  postRemember,
  readSessionEvents,
  buildSessionDigest,
  readSaveState,
  writeSaveState,
  parseMemoryBlocks,
  extractAssistantText,
  extractLastTurn,
  isRealUserMessage,
  readTranscript,
  DEBUG,
} from "./_shared.mjs";

const autoSaveReason = (n) =>
  `[memini auto-save] ${n} user messages since the last save. Before stopping, ` +
  `review this conversation for durable decisions, facts, and user preferences, ` +
  `and persist each with the memini memory_remember MCP tool (tier "semantic" ` +
  `for facts/decisions, "procedural" for how-tos; one memory per item). ` +
  `If nothing new is worth keeping, just stop again.`;

/** Count real user messages in a Claude Code transcript (JSONL string). */
function countUserMessages(raw) {
  let n = 0;
  for (const line of raw.split("\n")) {
    if (!line.trim()) continue;
    const r = parseJSON(line);
    if (!r || r.type !== "user" || r.isSidechain || r.isMeta) continue;
    if (!isRealUserMessage(r.message?.content)) continue;
    n++;
  }
  return n;
}

/**
 * Decide whether to block the agent with an auto-save nudge. Returns a reason
 * string to block with, or null to pass through. Never throws. The auto_save
 * on/off switch and the interval come from the resolved session context (env
 * override > server setting > default), so an operator can tune both without
 * touching the machine.
 */
function autoSaveReasonFor(payload, sessionId, ctx) {
  if (!ctx.setting("auto_save").value) return null; // opt-out
  if (payload.stop_hook_active) return null; // loop guard: already in a save cycle
  const tp = payload.transcript_path;
  if (!tp) return null;
  let count;
  try {
    count = countUserMessages(fs.readFileSync(tp, "utf8"));
  } catch {
    return null; // unreadable transcript → never block
  }
  const state = readSaveState(sessionId);
  const last = state && typeof state.lastSavedCount === "number" ? state.lastSavedCount : null;
  // First sight, or the transcript was replaced/cleared (count regressed):
  // re-baseline silently so we don't block on a resumed session's backlog.
  if (last === null || last > count) {
    writeSaveState(sessionId, { ...(state || {}), lastSavedCount: count, updatedAt: new Date().toISOString() });
    return null;
  }
  const interval = Math.max(1, ctx.setting("auto_save_interval").value || 10);
  if (count - last < interval) return null;
  // Update at block time, not after the agent saves — so even if it saves
  // nothing we wait another full interval before nudging again. Spread the
  // existing state so we don't clobber captureTurn's lastCapturedTurn (written
  // earlier in this same Stop run).
  writeSaveState(sessionId, { ...state, lastSavedCount: count, updatedAt: new Date().toISOString() });
  return autoSaveReason(count - last);
}

/**
 * Capture the session's latest user→assistant turn as an episodic memory, so
 * Claude gets the same automatic per-turn recall layer opencode has (via its
 * session.idle hook). On by default; opt out with MEMINI_CAPTURE_TURNS=0.
 * Deduped on the assistant message id stored in the save-state, so the repeated
 * Stop firings of one turn write at most once. Never throws.
 */
async function captureTurn(payload, sessionId, project, ctx) {
  if (!ctx.setting("capture_turns").value) return;
  // A save cycle (the agent re-run after an auto-save block) isn't a real user
  // turn — skip it so we don't capture the nudge handling as conversation.
  if (payload.stop_hook_active) return;
  if (!payload.transcript_path) return;
  // "unknown" is the defensive fallback for a payload with no session id. A
  // capture tagged with it shares one exclusion bucket with every other
  // unknown-id session (pre-tool-use excludes by exact session_id match), so
  // cross-session turns would echo into each other. No identity → no capture.
  if (!sessionId || sessionId === "unknown") return;
  const { userText, assistantText, assistantId } = extractLastTurn(readTranscript(payload.transcript_path));
  if (!userText || !assistantText) return;
  // Dedup key: the assistant message id when present, else a content hash — so a
  // transcript whose entries carry no id/uuid still can't re-capture the same
  // turn on every Stop firing.
  const dedupKey =
    assistantId || crypto.createHash("sha256").update(`${userText}\n${assistantText}`).digest("hex").slice(0, 16);
  const state = readSaveState(sessionId) || {};
  if (state.lastCapturedTurn === dedupKey) return; // already captured this turn
  const stored = await postRemember(`${userText.slice(0, 1000)}\n\n${assistantText.slice(0, 3000)}`, project, {
    tier: "episodic",
    tags: ["turn-capture", project],
    metadata: { source: "turn_capture", session_id: sessionId, format: "turn" },
  });
  if (stored !== null)
    writeSaveState(sessionId, { ...state, lastCapturedTurn: dedupKey, updatedAt: new Date().toISOString() });
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  // A server write tagged session_id:"unknown" shares one exclusion bucket
  // with every other unknown-id session (pre-tool-use excludes by exact
  // session_id match), so cross-session rows would echo into each other. No
  // identity → no server writes; local buffer bookkeeping is unaffected.
  const hasSessionIdentity = sessionId !== "unknown";
  const cwd = payload.cwd || process.cwd();

  // Cache-first namespace + settings: a valid per-session handshake is reused;
  // only a miss (a session started while the server was down, or a 10-min TTL
  // lapse) pays a live handshake. Stop fires once per assistant turn, so this
  // is also what keeps the shared cache fresh for the network-free hot-path
  // hooks (Pre/PostToolUse) through a long session.
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "on-miss", timeoutMs: 2000 });
  const project = ctx.namespace;

  // Refresh this session's recorded project dir. Stop fires once per assistant
  // turn, so any session actually in use stays comfortably inside
  // SESSION_CWD_TTL_MS — which is what lets the TTL be short enough to bound
  // pid reuse without ever expiring under a live session.
  writeSessionCwd(process.ppid, cwd);

  const digest = buildSessionDigest(readSessionEvents(sessionId), project);

  if (DEBUG)
    console.error(`[memini] Stop project=${project} session=${sessionId} events=${digest?.count || 0}`);

  // No buffered events → nothing to checkpoint; a bare marker is just noise.
  // session_digest off → no activity records at all (this checkpoint is the
  // crash-safety copy of the SessionEnd digest, so it goes with it).
  if (digest && hasSessionIdentity && ctx.setting("session_digest").value)
    await postRemember(digest.content, project, {
      tier: "working",
      tags: ["stop-checkpoint", project],
      id: `stop:${sessionId}`,
      summary: digest.summary,
      metadata: { source: "stop_checkpoint", session_id: sessionId },
    });

  // Legacy inline-block scrape (on by default; opt out with MEMINI_INLINE_EXTRACT=0):
  // back-compat fallback for sessions started under the old instruction that asked
  // for <memory> blocks in the reply text. New sessions save via the memory_remember
  // MCP tool directly; any block that still shows up here is persisted as a durable
  // semantic fact so nothing is lost.
  if (ctx.setting("inline_extract").value && hasSessionIdentity && payload.transcript_path) {
    const transcript = readTranscript(payload.transcript_path);
    const assistantTexts = extractAssistantText(transcript);
    const allBlocks = [];
    for (const text of assistantTexts) {
      for (const mem of parseMemoryBlocks(text)) {
        allBlocks.push(mem.content);
      }
    }
    if (DEBUG && allBlocks.length > 0)
      console.error(`[memini] Inline extraction: ${allBlocks.length} memory block(s) from transcript`);
    for (const content of allBlocks) {
      await postRemember(content, project, {
        tier: "semantic",
        tags: ["inline-extract", project],
        metadata: { source: "inline_extract", session_id: sessionId },
      });
    }
  }

  await captureTurn(payload, sessionId, project, ctx);

  const reason = autoSaveReasonFor(payload, sessionId, ctx);
  if (reason) process.stdout.write(JSON.stringify({ decision: "block", reason }));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] Stop error:", e);
});
