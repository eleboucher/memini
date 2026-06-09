#!/usr/bin/env node
// Stop hook. Fires when the user stops the agent mid-turn. We don't
// summarize here (memini has no separate summarize endpoint) — we just
// record a checkpoint so a long session doesn't lose its tail. The
// SessionEnd hook writes the more durable marker.

import { readStdin, parseJSON, resolveProject, postRemember, DEBUG } from "./_shared.mjs";

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();
  const project = resolveProject(cwd);

  if (DEBUG) console.error(`[memini] Stop project=${project} session=${sessionId}`);

  await postRemember(`Stop checkpoint in ${project}`, project, {
    tier: "working",
    tags: ["stop-checkpoint", project],
    id: `stop:${sessionId}:${Date.now()}`,
  });
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] Stop error:", e);
});
