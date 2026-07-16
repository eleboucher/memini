#!/usr/bin/env node
// Stop hook. Two jobs:
//   1. Write a short-lived (working tier, 24h) checkpoint distilled from the
//      buffered tool events so a long session doesn't lose its tail. Unlike
//      SessionEnd it does NOT delete the buffer — the session may continue and
//      SessionEnd writes the durable digest.
//   2. Auto-save nudge: an event-aware heuristic (evaluateAutoSave). Once per
//      MEMINI_AUTO_SAVE_INTERVAL user messages it may block the agent and ask it
//      to persist durable facts via the memini memory_remember MCP tool — but it
//      SUPPRESSES the nudge when the model already saved this window, DEFERS
//      trivial windows (fewer than MEMINI_AUTO_SAVE_MIN_EVENTS buffered tool
//      events) until the interval doubles, and ANCHORS a real nudge in the
//      session's actual files/commands. On by default; opt out with
//      MEMINI_AUTO_SAVE=0.
//
// SubagentStop is deliberately left unwired: Stop fires only for the main
// agent, so subagent sessions never trigger a nudge.

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
  scanTranscriptStats,
  evaluateAutoSave,
  renderAutoSaveNudge,
  parseMemoryBlocks,
  extractAssistantText,
  extractLastTurn,
  readTranscript,
  buildTurnCapture,
  DEBUG,
} from "./_shared.mjs";

/**
 * Decide whether to block the agent with an auto-save nudge. Returns a reason
 * string to block with, or null to pass through. Never throws. The decision is
 * an event-aware heuristic (evaluateAutoSave): it scans the transcript once for
 * user-message and memory-save counts, reads the buffered tool events, and folds
 * them against the persisted save-state. It suppresses when the model already
 * saved, defers trivial windows, and anchors a real nudge in the session's
 * files/commands. The auto_save switch, interval, and min-events knobs come from
 * the resolved session context (env override > server setting > default).
 */
function autoSaveReasonFor(payload, sessionId, project, ctx) {
  if (!ctx.setting("auto_save").value) return null; // opt-out
  if (payload.stop_hook_active) return null; // loop guard: already in a save cycle
  const tp = payload.transcript_path;
  if (!tp) return null;
  let raw;
  try {
    raw = fs.readFileSync(tp, "utf8");
  } catch {
    return null; // unreadable transcript → never block
  }
  const stats = scanTranscriptStats(raw);
  const events = readSessionEvents(sessionId);
  const state = readSaveState(sessionId);
  const interval = Math.max(1, ctx.setting("auto_save_interval").value || 10);
  const minEvents = Math.max(0, ctx.setting("auto_save_min_events").value ?? 3);
  const result = evaluateAutoSave({ state, stats, events, now: Date.now(), interval, minEvents });

  // Persist the re-baseline (baseline/suppress/nudge) if the heuristic asked for
  // one. Spread the existing state so we don't clobber captureTurn's
  // lastCapturedTurn, written earlier in this same Stop run. defer/none carry no
  // nextState, so the deltas keep growing.
  if (result.nextState)
    writeSaveState(sessionId, { ...(state || {}), ...result.nextState, updatedAt: new Date().toISOString() });

  if (result.action !== "nudge") return null;

  const prior = state && typeof state.lastSavedCount === "number" ? state.lastSavedCount : 0;
  const msgs = stats.userMessages - prior;
  let nudgeCtx = { msgs, files: [], commands: [], failedCommands: [] };
  if (result.variant === "specifics") {
    // Anchor on the fresh events only (a null digest → empty anchors).
    const digest = buildSessionDigest(result.fresh, project);
    if (digest)
      nudgeCtx = { msgs, files: digest.renderedFiles, commands: digest.commands, failedCommands: digest.failedCommands };
  }
  return renderAutoSaveNudge(result.variant, nudgeCtx);
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
  // The bounds are server-resolved (capture_user_max_chars /
  // capture_assistant_max_chars), not baked in here: how much of a turn is
  // worth keeping is a property of the deployment's store and recall budget,
  // which the server knows and this hook does not. buildTurnCapture marks a cut
  // and never splits a character — see @memini/client's capture.ts. Note the
  // dedup key above is computed from the untruncated text, so changing a bound
  // can't re-capture a turn already stored.
  const body = buildTurnCapture(
    userText,
    assistantText,
    ctx.setting("capture_user_max_chars").value,
    ctx.setting("capture_assistant_max_chars").value,
  );
  const stored = await postRemember(body, project, {
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

  const reason = autoSaveReasonFor(payload, sessionId, project, ctx);
  if (reason) process.stdout.write(JSON.stringify({ decision: "block", reason }));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] Stop error:", e);
});
