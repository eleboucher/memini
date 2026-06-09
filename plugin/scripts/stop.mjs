#!/usr/bin/env node
// Stop hook. Fires when the user stops the agent mid-turn. Writes a short-lived
// (working tier, 24h) checkpoint distilled from the buffered tool events so a
// long session doesn't lose its tail. Unlike SessionEnd it does NOT delete the
// buffer — the session may continue and SessionEnd writes the durable digest.

import {
  readStdin,
  parseJSON,
  resolveProject,
  postRemember,
  readSessionEvents,
  buildSessionDigest,
  DEBUG,
} from "./_shared.mjs";

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
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] Stop error:", e);
});
