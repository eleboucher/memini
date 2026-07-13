#!/usr/bin/env node
// PreCompact hook. Fires right before Claude Code compacts the context window.
// Distills the buffered tool events into a durable episodic checkpoint so the
// session's work survives compaction. Unlike SessionEnd it does NOT delete the
// buffer — the session continues after compaction and SessionEnd still writes
// the final digest. Claude-Code-only: Codex has no compaction lifecycle event.

import {
  readStdin,
  parseJSON,
  getSessionContext,
  postRemember,
  readSessionEvents,
  buildSessionDigest,
  DEBUG,
} from "./_shared.mjs";

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "on-miss", timeoutMs: 2000 });
  const project = ctx.namespace;

  const digest = buildSessionDigest(readSessionEvents(sessionId), project);
  if (!digest) return; // nothing buffered → no checkpoint, no noise
  // session_digest off → no activity records at all. This checkpoint exists to
  // rescue the digest from a compaction, so with digests off there is nothing
  // to rescue.
  if (!ctx.setting("session_digest").value) return;
  // A checkpoint tagged session_id:"unknown" shares one exclusion bucket with
  // every other unknown-id session (exact-match exclusion), so skip it.
  if (sessionId === "unknown") return;

  if (DEBUG)
    console.error(`[memini] PreCompact project=${project} session=${sessionId} events=${digest.count}`);

  await postRemember(`Pre-compaction checkpoint: ${digest.content}`, project, {
    tier: "episodic",
    tags: ["precompact-checkpoint", project],
    id: `precompact:${sessionId}`,
    summary: digest.summary,
    metadata: { session_id: sessionId, trigger: payload.trigger || "unknown" },
  });
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PreCompact error:", e);
});
