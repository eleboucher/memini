#!/usr/bin/env node
// PostToolUse hook. Records what just happened. We classify the tool into
// a short label so downstream search can find it by intent ("what did I
// edit in auth.go?") without grepping raw tool output.
//
// We only record a memory for tools that actually changed something
// observable (file edits, commands that touched state). Read-only tools
// like Read/Glob/Grep do not produce a memory — they were already
// surfaced by PreToolUse.

import {
  readStdin,
  parseJSON,
  resolveProject,
  postRemember,
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

function summarize(toolName, args, output) {
  const files = extractFiles(args);
  const fileHint = files.length > 0 ? ` ${files[0]}` : "";
  const outHint = truncate(typeof output === "string" ? output : "", 160);
  const head = `${toolName}${fileHint}`;
  if (outHint) return `${head}: ${outHint}`;
  if (args && typeof args === "object") {
    const cmd = args.command || args.cmd;
    if (typeof cmd === "string") return `${head}: ${truncate(cmd, 160)}`;
  }
  return head;
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const { toolName, toolInput, toolOutput, sessionId, cwd } = readToolCall(payload);
  if (!toolName) return;

  // Skip read-only tools — they were surfaced by PreToolUse, and recording
  // them would just bloat episodic memory with no recall value.
  if (["Read", "Glob", "Grep", "WebFetch", "WebSearch", "TodoWrite"].includes(toolName)) return;

  const project = resolveProject(cwd);
  const content = summarize(toolName, toolInput, toolOutput);

  if (DEBUG)
    console.error(`[memini] PostToolUse tool=${toolName} project=${project} session=${sessionId}`);

  await postRemember(content, project, {
    tier: "episodic",
    tags: ["tool-use", toolName, project],
  });
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PostToolUse error:", e);
});
