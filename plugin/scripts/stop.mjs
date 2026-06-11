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
import {
  readStdin,
  parseJSON,
  resolveProject,
  postRemember,
  readSessionEvents,
  buildSessionDigest,
  readSaveState,
  writeSaveState,
  DEBUG,
} from "./_shared.mjs";

const DEFAULT_INTERVAL = 15;

const autoSaveReason = (n) =>
  `[memini auto-save] ${n} user messages since the last save. Before stopping, ` +
  `review this conversation for durable decisions, facts, and user preferences, ` +
  `and persist each with the memini memory_remember MCP tool (tier "semantic", ` +
  `one memory per item). If nothing new is worth keeping, just stop again.`;

/** Count real user messages in a Claude Code transcript (JSONL string). */
function countUserMessages(raw) {
  let n = 0;
  for (const line of raw.split("\n")) {
    if (!line.trim()) continue;
    const r = parseJSON(line);
    if (!r || r.type !== "user" || r.isSidechain || r.isMeta) continue;
    const c = r.message?.content;
    if (typeof c !== "string") continue; // arrays are tool_results, not user turns
    if (/^\s*<(local-command|command-)/.test(c)) continue; // slash-command / local-command noise
    n++;
  }
  return n;
}

function autoSaveInterval() {
  const v = parseInt(process.env.MEMINI_AUTO_SAVE_INTERVAL ?? "", 10);
  return Number.isFinite(v) && v >= 1 ? v : DEFAULT_INTERVAL;
}

/**
 * Decide whether to block the agent with an auto-save nudge. Returns a reason
 * string to block with, or null to pass through. Never throws.
 */
function autoSaveReasonFor(payload, sessionId) {
  if (process.env.MEMINI_AUTO_SAVE === "0") return null; // opt-out
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
    writeSaveState(sessionId, { lastSavedCount: count, updatedAt: new Date().toISOString() });
    return null;
  }
  if (count - last < autoSaveInterval()) return null;
  // Update at block time, not after the agent saves — so even if it saves
  // nothing we wait another full interval before nudging again.
  writeSaveState(sessionId, { lastSavedCount: count, updatedAt: new Date().toISOString() });
  return autoSaveReason(count - last);
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();
  const project = resolveProject(cwd);

  const digest = buildSessionDigest(readSessionEvents(sessionId), project);
  const content = digest ? digest.content : `Stop checkpoint in ${project}`;

  if (DEBUG)
    console.error(`[memini] Stop project=${project} session=${sessionId} events=${digest?.count || 0}`);

  await postRemember(content, project, {
    tier: "working",
    tags: ["stop-checkpoint", project],
    id: `stop:${sessionId}`,
    summary: digest?.summary,
    metadata: { session_id: sessionId },
  });

  const reason = autoSaveReasonFor(payload, sessionId);
  if (reason) process.stdout.write(JSON.stringify({ decision: "block", reason }));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] Stop error:", e);
});
