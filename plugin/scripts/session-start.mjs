#!/usr/bin/env node
// SessionStart hook.
//
// Behavior:
//   1. Read the agent's SessionStart payload from stdin (cwd, session_id, ...).
//   2. Resolve the project namespace from data.cwd.
//   3. Search memini for memories tagged with the session id or recent in
//      the project, and write a short context block to stdout. Claude Code
//      and Codex both prepend stdout to the agent's context window.
//
// We don't try to be exhaustive here — a "what was I doing last time in
// this project" hint is more useful than a wall of text. Limit to 5 hits.

import {
  readStdin,
  parseJSON,
  resolveProject,
  postSearch,
  cleanStaleBuffers,
  DEBUG,
} from "./_shared.mjs";

// Buffers older than this are abandoned (crashed/killed sessions) and removed.
const STALE_BUFFER_MS = 7 * 24 * 60 * 60 * 1000;

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId;
  const cwd = payload.cwd || process.cwd();
  const project = resolveProject(cwd);

  // Hygiene: drop session buffers left behind by sessions that never ended.
  cleanStaleBuffers(STALE_BUFFER_MS);

  if (DEBUG) console.error(`[memini] SessionStart project=${project} session=${sessionId}`);

  // 1. Look for the last session-end marker for this project. That gives the
  //    agent a short "you last worked on X" hint instead of a long search.
  const last = await postSearch("session ended in " + project, project, {
    limit: 1,
    tiers: ["episodic"],
  });
  const recent = await postSearch("recent decisions and conventions in " + project, project, {
    limit: 4,
    tiers: ["semantic", "procedural"],
  });

  if (last.length === 0 && recent.length === 0) return;

  const lines = [];
  lines.push(`<memini-context project="${project}">`);
  if (last[0]) {
    lines.push(`Last session (${last[0].memory?.metadata?.session_id || "?"}): ${last[0].content}`);
  }
  for (const r of recent) {
    if (r.content && r.content !== last[0]?.content) {
      lines.push(`- ${r.content}`);
    }
  }
  lines.push("</memini-context>");

  // Both Claude Code and Codex interpret stdout as additional context.
  process.stdout.write(lines.join("\n"));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] SessionStart error:", e);
});
