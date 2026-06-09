#!/usr/bin/env node
// PostToolUse hook. Records what just happened to a local session buffer
// instead of POSTing a memory per call. The buffer is distilled into one dense
// digest memory by session-end.mjs — far less noise than dozens of thin
// tool-use fragments, and zero network traffic on this hot path.
//
// Only state-changing tools are buffered (Edit/Write/Bash/...); read-only tools
// are surfaced live by PreToolUse and carry no recall value here. The matcher in
// hooks.json already narrows the events; this is a defensive second filter.

import { readStdin, parseJSON, readToolCall, appendSessionEvent, DEBUG } from "./_shared.mjs";

const FILE_KEYS = ["filePath", "file_path", "path", "file", "pattern"];
const RECORDED = new Set(["Edit", "MultiEdit", "Write", "Bash", "NotebookEdit", "Agent", "Task"]);

function firstFile(args) {
  if (!args || typeof args !== "object") return "";
  for (const k of FILE_KEYS) {
    const v = args[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return "";
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const { toolName, toolInput, sessionId } = readToolCall(payload);
  if (!toolName || !RECORDED.has(toolName)) return;

  const args = toolInput && typeof toolInput === "object" ? toolInput : {};
  const cmd = typeof args.command === "string" ? args.command : args.cmd;

  if (DEBUG) console.error(`[memini] PostToolUse buffer tool=${toolName} session=${sessionId}`);

  appendSessionEvent(sessionId, {
    ts: Date.now(),
    tool: toolName,
    file: firstFile(args),
    cmd: typeof cmd === "string" ? cmd : "",
  });
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PostToolUse error:", e);
});
