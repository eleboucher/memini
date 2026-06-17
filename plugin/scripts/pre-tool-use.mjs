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
//
// Budget knobs (MEMINI_INJECT_PRETOOL_*) let operators cap the volume of
// context injected per tool call. Defaults match the prior hardcoded values
// so existing installs see identical behavior until they opt in.

import {
  readStdin,
  parseJSON,
  resolveProject,
  postSearch,
  readToolCall,
  intEnv,
  floatEnv,
  listEnv,
  labelsEnv,
  fitByTokens,
  truncate,
  DEBUG,
} from "./_shared.mjs";

const FILE_KEYS = ["filePath", "file_path", "path", "file", "pattern"];

// Default tool allowlist mirrors the matcher in hooks.json. Operators can
// override via MEMINI_INJECT_PRETOOL_TOOLS ("Read|Write|Edit") to add or
// remove tools without editing the manifest.
const DEFAULT_PRETOOL_TOOLS = ["read", "write", "edit", "glob", "grep"];

function extractFiles(args) {
  if (!args || typeof args !== "object") return [];
  const out = [];
  for (const k of FILE_KEYS) {
    const v = args[k];
    if (typeof v === "string" && v.length > 0) out.push(v);
  }
  return out;
}

function toolAllowed(toolName, allow) {
  if (!Array.isArray(allow) || allow.length === 0) return true;
  return allow.includes(String(toolName || "").toLowerCase());
}

function formatHit(h, labels) {
  const text = h?.content || h?.summary || "";
  if (!text) return null;
  if (labels.size === 0) {
    return `- (${h.score.toFixed(2)}) ${truncate(text, 240)}`;
  }
  const tagParts = [];
  if (labels.has("tier") && h.tier) tagParts.push(h.tier);
  if (labels.has("confidence") && typeof h.memory?.confidence === "number") {
    tagParts.push(`conf=${h.memory.confidence.toFixed(2)}`);
  }
  if (labels.has("reason")) tagParts.push("relevant memory");
  const prefix = tagParts.length ? `[${tagParts.join(" · ")}] ` : "";
  return `- (${h.score.toFixed(2)}) ${prefix}${truncate(text, 240)}`;
}

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const { toolName, cwd, sessionId } = readToolCall(payload);
  if (!toolName) return;

  const args = payload.tool_input ?? payload.toolArgs ?? {};
  const project = resolveProject(cwd);

  if (DEBUG)
    console.error(`[memini] PreToolUse tool=${toolName} project=${project} session=${sessionId}`);

  // Tool allowlist override (defaults to the manifest's matcher set).
  const allow = listEnv("MEMINI_INJECT_PRETOOL_TOOLS");
  const effectiveAllow = allow.length ? allow : DEFAULT_PRETOOL_TOOLS;
  if (!toolAllowed(toolName, effectiveAllow)) return;

  const files = extractFiles(args);
  if (files.length === 0) return;

  const itemsPerFile = intEnv("MEMINI_INJECT_PRETOOL_ITEMS", 3);
  const maxTokens = intEnv("MEMINI_INJECT_PRETOOL_MAX_TOK", 0);
  const minScore = floatEnv("MEMINI_INJECT_PRETOOL_MIN_SCORE", 0);
  const labels = labelsEnv();

  // One short query per file is the sweet spot. memini's hybrid retrieval
  // makes per-file queries cheap; bundling them would dilute the score.
  const out = [`<memini-pretool tool="${toolName}">`];
  let any = false;
  let totalDropped = 0;
  for (const f of files.slice(0, 3)) {
    const q = `${toolName} on ${f}`;
    // Exclude this session's own captured digests (Stop checkpoint / SessionEnd
    // digest, both tagged session_id): they're still in the live context, so
    // surfacing them just echoes what the agent already did this session. Prior
    // sessions' digests stay recallable.
    const exclude = sessionId ? { session_id: sessionId } : undefined;
    const hits = await postSearch(q, project, {
      limit: itemsPerFile,
      exclude,
      minScore,
    });
    if (hits.length === 0) continue;
    any = true;
    out.push(`File: ${f}`);
    // Render then trim by token budget (within a single file's block) so a
    // tight cap drops the lowest-scoring hits per file first.
    const lines = hits.map((h) => formatHit(h, labels)).filter(Boolean);
    const fit = fitByTokens(lines, maxTokens);
    out.push(...fit.items);
    totalDropped += fit.dropped;
  }
  if (!any) return;
  if (totalDropped > 0) out.push(`[... ${totalDropped} item(s) truncated by token budget]`);
  out.push("</memini-pretool>");
  // PreToolUse plain stdout is NOT shown to the model (it goes to the debug
  // log) — context must be returned as JSON additionalContext.
  process.stdout.write(
    JSON.stringify({
      hookSpecificOutput: { hookEventName: "PreToolUse", additionalContext: out.join("\n") },
    }),
  );
  if (DEBUG) {
    console.error(
      `[memini] PreToolUse injected ${out.length - 2} lines for ${files.slice(0, 3).length} file(s) ` +
        `(itemsPerFile=${itemsPerFile}, minScore=${minScore}, maxTokens=${maxTokens || "∞"}, dropped=${totalDropped})`,
    );
  }
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PreToolUse error:", e);
});
