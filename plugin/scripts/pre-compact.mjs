#!/usr/bin/env node
// PreCompact hook. Fires right before Claude Code or Codex compacts context.
// Distills the buffered tool events into a durable episodic checkpoint so the
// session's work survives compaction. Unlike SessionEnd it does NOT delete the
// buffer — the session continues after compaction. Claude SessionEnd later
// writes the final digest; Codex retains rolling Stop checkpoints instead.

import {
  readStdin,
  parseJSON,
  getSessionContext,
  postRemember,
  readSessionEvents,
  buildSessionDigest,
  deleteLastRecallState,
  deleteInjectedState,
  DEBUG,
} from "./_shared.mjs";

async function main() {
  const payload = parseJSON(await readStdin()) || {};
  const sessionId = payload.session_id || payload.sessionId || "unknown";
  const cwd = payload.cwd || process.cwd();

  // Compaction evicts earlier PreToolUse injections from context (that's the
  // whole point of compacting), so the last-recall fingerprints recorded
  // against them are now stale — the next recall for a file must re-inject
  // even if the served memories are unchanged. Same for the prompt-recall
  // injected-id state: "already in context" stopped being true the moment the
  // context was rebuilt. Unconditional and independent of whether this
  // session buffered any events worth checkpointing below.
  deleteLastRecallState(sessionId);
  deleteInjectedState(sessionId);

  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "on-miss", timeoutMs: 2000 });
  const project = ctx.namespace;

  const digest = buildSessionDigest(readSessionEvents(sessionId), project);
  if (!digest) {
    if (process.env.PLUGIN_ROOT) process.stdout.write("{}");
    return;
  }
  // session_digest off → no activity records at all. This checkpoint exists to
  // rescue the digest from a compaction, so with digests off there is nothing
  // to rescue.
  if (!ctx.setting("session_digest").value) {
    if (process.env.PLUGIN_ROOT) process.stdout.write("{}");
    return;
  }
  // A checkpoint tagged session_id:"unknown" shares one exclusion bucket with
  // every other unknown-id session (exact-match exclusion), so skip it.
  if (sessionId === "unknown") {
    if (process.env.PLUGIN_ROOT) process.stdout.write("{}");
    return;
  }

  if (DEBUG)
    console.error(`[memini] PreCompact project=${project} session=${sessionId} events=${digest.count}`);

  await postRemember(`Pre-compaction checkpoint: ${digest.content}`, project, {
    tier: "episodic",
    tags: ["precompact-checkpoint", project],
    id: `precompact:${sessionId}`,
    summary: digest.summary,
    metadata: { session_id: sessionId, trigger: payload.trigger || "unknown" },
  });
  if (process.env.PLUGIN_ROOT) process.stdout.write("{}");
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] PreCompact error:", e);
});
