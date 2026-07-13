#!/usr/bin/env node
// PostToolUse hook. Records what just happened to a local session buffer
// instead of POSTing a memory per call. The buffer is distilled into one dense
// digest memory by session-end.mjs — far less noise than dozens of thin
// tool-use fragments, and zero network traffic on this hot path.
//
// Only state-changing tools are buffered (Edit/Write/Bash/...); read-only tools
// are surfaced live by PreToolUse and carry no recall value here. The matcher in
// hooks.json already narrows the events; this is a defensive second filter.

import {
  readStdin,
  parseJSON,
  readToolCall,
  appendSessionEvent,
  getSessionContext,
  DEBUG,
} from "./_shared.mjs";

const FILE_KEYS = ["filePath", "file_path", "path", "file", "pattern"];
const RECORDED = new Set(["edit", "multiedit", "write", "bash", "notebookedit", "agent", "task", "apply_patch"]);

function firstFile(args) {
  if (!args || typeof args !== "object") return "";
  for (const k of FILE_KEYS) {
    const v = args[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return "";
}

function patchText(input) {
  if (typeof input === "string") return input;
  if (!input || typeof input !== "object") return "";
  for (const k of ["patch", "input", "cmd", "command"]) {
    const v = input[k];
    if (typeof v === "string" && v.includes("*** Begin Patch")) return v;
  }
  return "";
}

function filesFromApplyPatch(input) {
  const text = patchText(input);
  if (!text) return [];
  const files = [];
  const seen = new Set();
  for (const line of text.split("\n")) {
    const m = line.match(/^\*\*\* (?:Add|Update|Delete) File: (.+)$/) || line.match(/^\*\*\* Move to: (.+)$/);
    if (!m) continue;
    const file = m[1].trim();
    if (!file || seen.has(file)) continue;
    seen.add(file);
    files.push(file);
  }
  return files;
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const { toolName, toolInput, sessionId, cwd } = readToolCall(payload);
  const toolKey = String(toolName || "").toLowerCase();
  if (!toolName || !RECORDED.has(toolKey)) return;

  const args = toolInput && typeof toolInput === "object" ? toolInput : {};
  const cmd = typeof args.command === "string" ? args.command : args.cmd;
  const files = toolKey === "apply_patch" ? filesFromApplyPatch(toolInput) : [];
  const file = files[0] || firstFile(args);

  if (DEBUG) console.error(`[memini] PostToolUse buffer tool=${toolName} session=${sessionId}`);

  const event = {
    ts: Date.now(),
    tool: toolName,
    file,
    files: files.length ? files : undefined,
    cmd: typeof cmd === "string" ? cmd : "",
  };
  // Mark failures (the harness's error flag) so the session digest can surface
  // failed→fixed command sequences for the distiller to mine.
  if (payload.tool_response?.is_error || payload.is_error) event.failed = true;

  // The buffer exists only to be distilled into a session digest. When
  // session_digest is off nothing will ever read it, so don't pay the write on
  // this hot path (PostToolUse fires on every state-changing tool call).
  //
  // Cache-only (allowNetwork "never"): this hook must make ZERO network calls.
  // If the per-session handshake hasn't cached a setting yet, session_digest
  // falls back to its env-override-or-default (on), so we buffer-when-unsure —
  // harmless, because the digest is re-gated at Stop/PreCompact/SessionEnd,
  // which won't write it if session_digest is actually off.
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "never" });
  if (!ctx.setting("session_digest").value) return;

  appendSessionEvent(sessionId, event);
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PostToolUse error:", e);
});
