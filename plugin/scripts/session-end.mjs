#!/usr/bin/env node
// SessionEnd hook. Distills the session's buffered tool events into a single
// dense episodic digest ("what I did this session: edited X/Y, ran Z"), so
// SessionStart can surface it next time. Writes nothing when no events were
// buffered (a bare end marker is content-free and only pollutes recall).
// Deletes the buffer afterwards either way.

import {
  readStdin,
  parseJSON,
  resolveProject,
  postRemember,
  readSessionEvents,
  buildSessionDigest,
  deleteSessionBuffer,
  DEBUG,
} from "./_shared.mjs";

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();
  const reason = payload.reason || "unknown";
  const project = resolveProject(cwd);

  const digest = buildSessionDigest(readSessionEvents(sessionId), project);

  if (DEBUG)
    console.error(
      `[memini] SessionEnd project=${project} session=${sessionId} reason=${reason} events=${digest?.count || 0}`,
    );

  // No buffered events → nothing to digest; a bare end marker is just noise.
  if (digest)
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

  deleteSessionBuffer(sessionId); // always, even when no digest was written
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] SessionEnd error:", e);
});
