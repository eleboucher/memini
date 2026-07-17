#!/usr/bin/env node
// PostToolUse hook. Records what just happened to a local session buffer
// instead of POSTing a memory per call. Two consumers read the buffer: the
// session digest (distilled into one dense memory by session-end.mjs — far less
// noise than dozens of thin tool-use fragments) and the Stop hook's event-aware
// auto-save nudge (which anchors its nudge in the session's fresh files/commands
// and only fires once the buffer holds enough activity). Zero network traffic on
// this hot path either way.
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
  readInjectedState,
  writeInjectedState,
  DEBUG,
} from "./_shared.mjs";

const FILE_KEYS = ["filePath", "file_path", "path", "file", "pattern"];
const RECORDED = new Set(["edit", "multiedit", "write", "bash", "notebookedit", "agent", "task", "apply_patch"]);

// memini's own MCP READ tools: what they return lands in the transcript like
// any tool output, so their memory ids feed the cross-surface injected state
// (see _shared.mjs) and the auto-recall surfaces stop re-injecting what the
// model already pulled explicitly. Matches both MCP namings — a plain server
// ("mcp__memini__memory_recall") and the plugin-scoped form
// ("mcp__plugin_memini_memini__memory_recall"). Write tools (remember/update)
// are not tracked: their results carry no content, and the content came from
// the model, which already has it.
const MEMORY_READ_TOOL = /^mcp__.*memini.*__memory_(recall|briefing|get)$/i;

// parseToolResult digs the JSON payload out of the harness's tool_response:
// MCP results arrive as {content:[{type:"text", text:"<json>"}]}, but a plain
// JSON string or an already-parsed object are accepted too. Null on anything
// unparseable — a malformed response is ignored, never a crash.
function parseToolResult(res) {
  if (typeof res === "string") return parseJSON(res);
  if (!res || typeof res !== "object") return null;
  if (Array.isArray(res.content)) {
    for (const c of res.content) {
      if (c && typeof c.text === "string") {
        const parsed = parseJSON(c.text);
        if (parsed) return parsed;
      }
    }
    return null;
  }
  return res;
}

// collectMemoryIds walks a parsed tool result (depth-limited) collecting the
// ids of memory-shaped objects — a string `id` next to content or summary.
// One walker covers all three read shapes: recall's flat results[], briefing's
// nested {memory, from} items, and get's single memory object.
function collectMemoryIds(node, out, depth = 0) {
  if (!node || typeof node !== "object" || depth > 6) return;
  if (Array.isArray(node)) {
    for (const item of node.slice(0, 200)) collectMemoryIds(item, out, depth + 1);
    return;
  }
  if (typeof node.id === "string" && node.id && (typeof node.content === "string" || typeof node.summary === "string")) {
    out.add(node.id);
  }
  for (const v of Object.values(node)) collectMemoryIds(v, out, depth + 1);
}

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

  // memini memory READS route to the injected state, never the event buffer —
  // a recall isn't session activity worth digesting. Recorded with a sentinel
  // identity ("") because a concise tool response may carry truncated content:
  // content identity is unknowable, so suppression for tool-sourced entries is
  // by id alone (pre-tool-use honors the sentinel). An id a hook already
  // recorded keeps its real hash — content-aware resurfacing survives.
  if (MEMORY_READ_TOOL.test(String(toolName || ""))) {
    if (!sessionId) return;
    const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "never" });
    if (!ctx.setting("inject_dedupe").value) return;
    const ids = new Set();
    collectMemoryIds(parseToolResult(payload.tool_response ?? payload.tool_output), ids);
    if (ids.size === 0) return;
    const injected = readInjectedState(sessionId);
    let recorded = false;
    for (const id of ids) {
      if (!(id in injected)) {
        injected[id] = "";
        recorded = true;
      }
    }
    if (recorded) writeInjectedState(sessionId, injected);
    if (DEBUG) console.error(`[memini] PostToolUse recorded ${ids.size} tool-read id(s) tool=${toolName} session=${sessionId}`);
    return;
  }

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

  // The buffer feeds two consumers — the session digest and the auto-save
  // nudge's activity anchor — so buffer when EITHER wants it and skip the write
  // only when neither will ever read it (both off). PostToolUse fires on every
  // state-changing tool call, so this gate matters on a hot path.
  //
  // Cache-only (allowNetwork "never"): this hook must make ZERO network calls.
  // If the per-session handshake hasn't cached a setting yet, the settings fall
  // back to their env-override-or-defaults (both on), so we buffer-when-unsure —
  // harmless, because the digest is re-gated at Stop/PreCompact/SessionEnd (which
  // won't write it if session_digest is actually off) and the nudge only reads
  // the buffer locally.
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "never" });
  const digestOn = ctx.setting("session_digest").value;
  const autoSaveOn = ctx.setting("auto_save").value;
  const minEvents = Math.max(0, ctx.setting("auto_save_min_events").value ?? 3);
  if (!digestOn && !(autoSaveOn && minEvents > 0)) return;

  appendSessionEvent(sessionId, event);
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PostToolUse error:", e);
});
