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
  getBriefing,
  cleanStaleBuffers,
  writePluginRoot,
  DEBUG,
} from "./_shared.mjs";

// Buffers older than this are abandoned (crashed/killed sessions) and removed.
const STALE_BUFFER_MS = 7 * 24 * 60 * 60 * 1000;

// section appends a labelled, bulleted group of memory contents to lines.
function section(lines, label, mems, max) {
  if (!Array.isArray(mems) || mems.length === 0) return;
  lines.push(label + ":");
  for (const m of mems.slice(0, max)) {
    const text = (m?.summary || m?.content || "").trim();
    if (text) lines.push(`- ${text.slice(0, 280)}`);
  }
}

async function main() {
  // Record the plugin root so the MCP headersHelper (which doesn't receive
  // ${CLAUDE_PLUGIN_ROOT}) can locate mcp-headers.mjs. Cheap and idempotent.
  writePluginRoot();

  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId;
  const cwd = payload.cwd || process.cwd();
  const project = resolveProject(cwd);

  // Hygiene: drop session buffers left behind by sessions that never ended.
  cleanStaleBuffers(STALE_BUFFER_MS);

  if (DEBUG) console.error(`[memini] SessionStart project=${project} session=${sessionId}`);

  // A single query-less briefing call returns a layered view: pinned identity,
  // durable facts/procedures, and recent activity — server-side ranked, so the
  // hook injects useful context without N searches.
  const b = await getBriefing(project, 5);
  if (!b) return;
  const sections = [b.pinned, b.facts, b.procedures, b.recent];
  if (sections.every((s) => !Array.isArray(s) || s.length === 0)) return;

  const lines = [`<memini-context project="${project}">`];
  section(lines, "Pinned", b.pinned, 5);
  section(lines, "Decisions & conventions", b.facts, 5);
  section(lines, "How-to", b.procedures, 5);
  section(lines, "Recent activity", b.recent, 3);
  lines.push("</memini-context>");

  // Both Claude Code and Codex interpret stdout as additional context.
  process.stdout.write(lines.join("\n"));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] SessionStart error:", e);
});
