#!/usr/bin/env node
// SessionEnd hook. Distills the session's buffered tool events into a single
// dense episodic digest ("what I did this session: edited X/Y, ran Z"), so
// SessionStart can surface it next time. Writes nothing when no events were
// buffered (a bare end marker is content-free and only pollutes recall).
// Deletes the buffer afterwards either way.

import {
  readStdin,
  parseJSON,
  getSessionContext,
  postRemember,
  postSupersede,
  readSessionEvents,
  buildSessionDigest,
  deleteSessionBuffer,
  deleteSessionCwd,
  deleteCachedHandshake,
  DEBUG,
} from "./_shared.mjs";

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();
  const reason = payload.reason || "unknown";
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "on-miss", timeoutMs: 2000 });
  const project = ctx.namespace;

  const digest = buildSessionDigest(readSessionEvents(sessionId), project);

  if (DEBUG)
    console.error(
      `[memini] SessionEnd project=${project} session=${sessionId} reason=${reason} events=${digest?.count || 0}`,
    );

  // No buffered events → nothing to digest; a bare end marker is just noise.
  // No session identity → no write either: a digest tagged session_id:"unknown"
  // shares one exclusion bucket with every other unknown-id session, so
  // cross-session rows would echo into each other.
  // session_digest off → the operator does not want activity records at all.
  if (digest && sessionId !== "unknown" && ctx.setting("session_digest").value) {
    await postRemember(digest.content, project, {
      tier: "episodic",
      tags: ["session-marker", project],
      id: `session-end:${sessionId}`,
      summary: digest.summary,
      metadata: {
        session_id: sessionId,
        reason,
        files: digest.files,
        commands: digest.commands,
      },
    });
    // Supersede the stop:<sessionId> row the Stop hook emitted this session —
    // identical content when Stop fired on this same final turn, a stale partial
    // checkpoint otherwise; either way the long-lived digest replaces it.
    // postSupersede is a no-op when the target is missing (404 → null), so
    // always call it.
    await postSupersede(`stop:${sessionId}`, `session-end:${sessionId}`, project);
  }

  deleteSessionBuffer(sessionId); // always, even when no digest was written

  // Drop this session's recorded project dir AND its cached handshake. A pid is
  // not a durable identity — the OS recycles them, Windows especially fast — so
  // leaving either record behind means a later, unrelated session that inherits
  // the same pid could read it and target the WRONG repo's namespace/settings.
  // Deleting on a clean exit closes that window entirely; only a crash leaves a
  // record behind, and those are bounded by their TTLs (SESSION_CWD_TTL_MS /
  // HANDSHAKE_TTL_MS).
  deleteSessionCwd(process.ppid);
  deleteCachedHandshake(process.ppid);
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] SessionEnd error:", e);
});
