#!/usr/bin/env node
// PreToolUse hook. Runs before Edit|Write|Read|Glob|Grep.
//
// We don't try to block the tool (Claude Code's PreToolUse can `deny`, but
// that's not the design here). Instead we surface related memories to the
// agent's context by writing to stdout. Both Claude Code and Codex prepend
// stdout to the tool's input, so the model sees "memini says: <hint>"
// alongside the user's actual edit.
//
// For Edit/Write we search by the file path; for Read by the path; for
// Glob/Grep by the pattern. The hook is best-effort: if memini is down or
// slow, the tool still runs.

import {
  readStdin,
  parseJSON,
  resolveProject,
  postSearch,
  readToolCall,
  truncate,
  DEBUG,
} from "./_shared.mjs";

const FILE_KEYS = ["filePath", "file_path", "path", "file", "pattern"];

function extractFiles(args) {
  if (!args || typeof args !== "object") return [];
  const out = [];
  for (const k of FILE_KEYS) {
    const v = args[k];
    if (typeof v === "string" && v.length > 0) out.push(v);
  }
  return out;
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const { toolName, cwd, sessionId } = readToolCall(payload);
  if (!toolName) return;

  const args = payload.tool_input ?? payload.toolArgs ?? {};
  const project = resolveProject(cwd);

  if (DEBUG)
    console.error(`[memini] PreToolUse tool=${toolName} project=${project} session=${sessionId}`);

  const files = extractFiles(args);
  if (files.length === 0) return;

  // One short query per file is the sweet spot. memini's hybrid retrieval
  // makes per-file queries cheap; bundling them would dilute the score.
  const out = [`<memini-pretool tool="${toolName}">`];
  let any = false;
  for (const f of files.slice(0, 3)) {
    const q = `${toolName} on ${f}`;
    const hits = await postSearch(q, project, { limit: 3 });
    if (hits.length === 0) continue;
    any = true;
    out.push(`File: ${f}`);
    for (const h of hits) {
      if (!h.content) continue;
      out.push(`- (${h.score.toFixed(2)}) ${truncate(h.content, 240)}`);
    }
  }
  if (!any) return;
  out.push("</memini-pretool>");
  // PreToolUse plain stdout is NOT shown to the model (it goes to the debug
  // log) — context must be returned as JSON additionalContext.
  process.stdout.write(
    JSON.stringify({
      hookSpecificOutput: { hookEventName: "PreToolUse", additionalContext: out.join("\n") },
    }),
  );
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PreToolUse error:", e);
});
