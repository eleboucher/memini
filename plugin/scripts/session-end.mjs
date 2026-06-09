#!/usr/bin/env node
// SessionEnd hook. Distills the session's buffered tool events into a single
// dense episodic digest ("what I did this session: edited X/Y, ran Z"), so
// SessionStart can surface it next time. Falls back to a bare end marker when
// nothing was buffered. Deletes the buffer afterwards.

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
  const content = digest
    ? digest.content
    : `Session ended in ${project} (reason: ${reason})`;

  if (DEBUG)
    console.error(
      `[memini] SessionEnd project=${project} session=${sessionId} reason=${reason} events=${digest?.count || 0}`,
    );

  await postRemember(content, project, {
    tier: "episodic",
    tags: ["session-marker", project],
    id: `session-end:${sessionId}`,
    summary: digest?.summary || `Session ${sessionId} ended`,
    metadata: {
      session_id: sessionId,
      reason,
      files: digest?.files || [],
      commands: digest?.commands || [],
    },
  });

  deleteSessionBuffer(sessionId);
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] SessionEnd error:", e);
});
