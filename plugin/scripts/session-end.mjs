#!/usr/bin/env node
// SessionEnd hook. Records a brief session-end marker so SessionStart can
// surface "what was I doing last time" next time. Also drops the
// working-directory path into metadata so search-by-path becomes possible.

import { readStdin, parseJSON, resolveProject, postRemember, DEBUG } from "./_shared.mjs";

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();
  const reason = payload.reason || "unknown";
  const project = resolveProject(cwd);

  if (DEBUG)
    console.error(`[memini] SessionEnd project=${project} session=${sessionId} reason=${reason}`);

  await postRemember(`Session ended in ${project} (reason: ${reason})`, project, {
    tier: "episodic",
    tags: ["session-marker", project],
    id: `session-end:${sessionId}`,
    summary: `Session ${sessionId} ended`,
    metadata: { session_id: sessionId, reason },
  });
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] SessionEnd error:", e);
});
