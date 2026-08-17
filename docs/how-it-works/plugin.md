# How the plugin works

The short version: the Claude Code / Codex plugin is seven lifecycle hooks, an MCP server registration, and a shared client core bundled into the plugin. The hooks inject memory into the agent's context at three points (session start, each prompt, each file-tool call) and capture what happened back out (each turn, before compaction, at session end). The MCP registration gives the model explicit tools (`memory_remember`, `memory_recall`, ...) over the same server. One server handshake per session decides the namespace and every behavioral setting; everything else runs off that cached answer.

Installation and configuration live in the [plugin README](../../plugin/README.md); the env-var split between server and client is in [env vars](../reference/env-vars.md). This page is about mechanism: what fires when, what gets injected, and where the sharp edges are.

## The seven hooks

Claude Code wires all seven events; Codex wires six (it has no reliable final-session event, so the session-end digest is Claude-only and Codex relies on the rolling Stop checkpoints instead).

| Event                    | Network                               | What it does                                                                                                                         | What it injects                                         |
| ------------------------ | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------- |
| SessionStart             | always (live handshake)               | Resolves namespace + settings, caches the handshake for every other hook, fetches the briefing                                       | The session briefing block plus a save-policy directive |
| UserPromptSubmit         | never (cache only)                    | Searches memory with the user's own prompt text                                                                                      | Top recall hits relevant to what the user just asked    |
| PreToolUse               | never (cache only, search call gated) | Searches memory by file path before file tools run                                                                                   | Per-file hits ("we decided X about this file")          |
| PostToolUse              | never                                 | Buffers state-changing tool events locally; tracks memory-tool reads for dedupe                                                      | Nothing                                                 |
| Stop                     | on cache miss                         | Captures the last user→assistant turn as episodic memory; writes a short-lived checkpoint; may nudge the agent to save durable facts | Nothing (may block once with a save nudge)              |
| PreCompact               | on cache miss                         | Writes a pre-compaction checkpoint; resets injection-dedupe state (the context is about to be rebuilt)                               | Nothing                                                 |
| SessionEnd (Claude only) | on cache miss                         | Writes the final session digest, superseding the Stop checkpoint; deletes all per-session state                                      | Nothing                                                 |

Every hook is best-effort: if memini is down or slow, the prompt, tool call, or session proceeds without memory — never blocked, never crashed.

## The handshake

SessionStart is the only hook that always performs a live handshake. It gathers the project's facts — the git remote URL, the repository toplevel path and basename, the working directory basename, the agent name (`MEMINI_AGENT`), and any `MEMINI_NAMESPACE` override — and posts them to the server. The server answers with the resolved namespace (and how it resolved it: pin, env, derived, key default), the caller's identity, the read set, and a fully resolved copy of all behavioral settings.

That answer is cached per session for ten minutes, keyed by the harness process id, the working directory, and a hash of the facts — so a changed remote or a moved directory is a cache miss, not a stale hit. The hot-path hooks (UserPromptSubmit, PreToolUse, PostToolUse) read only this cache and make zero resolution round-trips. Stop refreshes the cache on a miss once per turn, which is what keeps it warm through a long session.

On any handshake failure the client is **degraded**: the namespace it derived locally is a guess, not the server's authority, and recalling against a possibly-wrong namespace is exactly the "recall looks where writes don't land" failure the handshake exists to prevent. So degraded hooks go quiet rather than guessing — the recall surfaces skip entirely and stay network-free until a later turn's refresh succeeds.

The MCP side has the same problem in a harder shape: the headers helper that stamps each MCP connection with the project namespace is started with the plugin's own install directory as its working directory, not the project. It recovers the real project directory through a chain — an explicit project-dir variable if the harness sets one, else the parent process's working directory (the parent is the session, and its cwd is the project), else a per-session cache file a hook recorded. With no project signal at all it sends auth-only headers and lets the server apply the key's default namespace rather than scatter memories into a namespace named after the plugin's version directory.

## Injection surface 1: the session briefing

One query-less, header-scoped briefing call returns a layered view — pinned memories, durable facts, how-to procedures, and recent activity — already ranked server-side, plus a scope header naming the ancestor chain the namespace inherits from. The hook renders it as one read-only block:

```
<memini-context project="acme/phoenix" read-only>
Scope: acme/phoenix ← acme(4)
Pinned:
- Deploys go through the staging cluster first — never straight to prod [m:9f21ab04]
Decisions & conventions:
- Queue backend is Postgres (SKIP LOCKED), not Redis — decided 2026-03 [m:4c77d1e8]
Recent activity:
- [3d] Refactored the retry loop in the worker pool [m:b0e4429a]
</memini-context>
```

Three details worth knowing:

- **Token budget with a drop order.** The briefing has a token ceiling (default 600, enforced both server-side and as a client fallback). When it overflows, whole sections are dropped from the tail first: recent, then procedures, then facts. Pinned is the curated top-of-mind set and is never dropped whole — as a last resort its lowest-ranked bullets are trimmed. Any trim, on either layer, is announced with a visible truncation footer.
- **Skip on unchanged content.** SessionStart can fire more than once per session (startup, then resume, clear, or compact). When the briefing is byte-identical to what was already injected, re-injecting only spends tokens, so it is skipped — except right after a compaction, where the original injection was summarized away with the rest of the context and the skip's premise is false. Resume keeps the skip: its context is intact.
- **The directive follows the fire source.** A fresh context (startup, clear) gets the standing save-policy instruction; a resume gets nothing (the transcript replay already carries it); a compaction gets only a short "flush unsaved facts" recovery nudge.

SessionStart is also where one-time hygiene lives: it warns once on stderr about removed env vars still exported in a shell rc (`MEMINI_URL`, `MEMINI_TOKEN`, `MEMINI_MCP_URL`, `MEMINI_NAMESPACE_SCOPE` — all silently ignored everywhere else; see [env vars](../reference/env-vars.md)).

## Injection surface 2: per-prompt recall

The prompt is the query. Before the model sees each user prompt, the hook searches memory with the user's actual words, so "what did we decide about auth tokens" recalls the auth decision before the model starts answering — something a file-path query can never surface.

This surface is strictly cache-only and fails closed:

- **Degraded means silent.** No valid cached handshake, no recall, no network, no output. It self-heals when a later turn refreshes the cache.
- **Prompt-shape gates.** Prompts that start with `/`, `!`, or `#` are commands to the harness, not questions, and are skipped; so are empty prompts and prompts under twelve characters ("yes", "continue" — steering, not queries). Very long prompts are truncated to a head of two thousand characters before querying: the head carries the intent, and a pasted stack trace makes a terrible semantic query.
- **The counter bumps before the gates.** A per-session prompt counter drives the injection cooldown windows, and it advances on every prompt — including the short and slash-prefixed ones that never recall — so a run of steering turns cannot freeze the cooldown into forever-dedupe.

Hits already in context (injected by the briefing, a previous prompt, or a pre-tool block) are excluded while their cooldown windows are open, so the top-k is spent on memories the context does not yet hold.

## Injection surface 3: pre-tool context

Before a file tool runs, the hook queries memory by file path — "Edit on internal/auth.go" — and injects what is known about that file. By default only the file-content tools trigger it (Read, Write, Edit, MultiEdit); the hook is wired to a wider event set that includes Glob and Grep, but those are excluded from the default allowlist because pattern-derived queries are near-zero-signal and each one costs a server embed and rerank. At most three files per tool call are queried.

Two mechanisms keep a burst of edits from spamming the context:

- **A per-file call gate.** After a real search for a file, further touches of the same file within the gate window (default 90 seconds) skip the server call entirely — the file was just recalled and its memories are already in context. The gate tracks the last _call_, not the last injection, and a gated skip never refreshes it, so a file cannot be starved forever.
- **A fingerprint plus a latch.** Each injection is fingerprinted from the served memories' identities (not their rendered text), so serving the same set again is suppressed as a duplicate. The first unchanged re-serve is deliberately let through to the client-side filter — so an in-place content update, which hashes differently, can still resurface — and only that unchanged pass latches the id into server-side exclusion, freeing a result slot on later calls.

The latch has a stated blind spot: once an id is latched into server-side exclusion, a _content update_ to that memory stays invisible on this surface until its cooldown windows lapse (default 30 minutes or three prompts, whichever is later) — the server never returns it, so the client-side content-aware filter never gets the chance to notice the change.

## Turn capture and echo suppression

On each Stop, the hook extracts the session's latest user→assistant turn from the transcript and stores it as an episodic memory marked as a turn capture, tagged with the session id. Each side is truncated under server-resolved caps (defaults: one thousand characters of user text, three thousand of assistant text), with any cut visibly marked, and the write is deduplicated on the assistant message id so repeated Stop firings of one turn store at most once.

A turn just captured is still sitting in the live context, so three layers keep it from echoing straight back in:

1. **A server-side fresh-turn window**: recall hides just-captured turns by default.
2. **A client-side filter**: the recall hooks drop any turn-capture hit younger than thirty minutes, catching captures written before a resume or clear rolled the session id.
3. **Per-session exclusion**: every recall the hooks issue excludes rows tagged with this session's id — checkpoints and digests included.

`include_fresh_turns` on `memory_recall` opts back in, for the rare "what did I just say" query. See [MCP tools](../reference/mcp-tools.md).

Stop also writes a short-lived working-tier checkpoint distilled from the buffered tool events, so a long session does not lose its tail to a crash. At SessionEnd the final episodic digest supersedes that checkpoint — one durable "what I did this session" record instead of two overlapping ones.

## Settings: who wins

Every behavioral knob — thirty of them, covering capture, recall, injection budgets, cooldowns, and nudges — resolves the same way: a local env override beats the server-merged value (per-key setting over global default) beats the built-in default. The env override is a debug lever and it **silently wins**: nothing in normal operation tells you a forgotten `export MEMINI_RECALL=0` in a shell rc is why recall stopped. The one surface that reveals it is `/memini:status`, which runs a live handshake and prints every setting with its provenance:

```
SETTINGS
  recall                       off                    <- env (overriding server)
  recall_limit                 3                      (default)
  inject_briefing_max_tok      800                    <- server (global)
  capture_turns                on                     (default)
```

An `<- env (overriding server)` row is the tell. The full knob list is in [env vars](../reference/env-vars.md).

## Known limitations

- **Codex's MCP namespace is static.** Codex configures the bundled MCP server with a fixed header-from-env mapping — there is no equivalent of the dynamic per-connection headers helper Claude Code runs — so under Codex the MCP tools' namespace comes from `MEMINI_NAMESPACE` (or the key's default), not from per-project resolution. The Codex _hooks_ still resolve per-project; only the MCP transport is pinned.
- **Pin changes need `/reload-plugins` on the MCP side.** Hooks pick up a new or changed pin on their next handshake, but Claude Code resolves MCP headers only when the server connects — until the plugin reconnects, the MCP tools keep writing to the old namespace while the hooks use the new one. That split is exactly what `memini doctor` diagnoses.
- **The pre-tool latch blind spot**, described above: a content update to a latched memory stays invisible on the pre-tool surface until its cooldown windows lapse.
- **Degraded mode is silent by design.** When the handshake fails, the recall hooks do nothing and say nothing (SessionStart prints one stderr notice; the per-prompt and pre-tool hooks just skip). Set `MEMINI_DEBUG=1` to see every hook's decision on stderr.

For where recalled memories come from and how they are ranked, see [recall](./recall.md); for where writes land, [namespaces](./namespaces.md) and [scoping](../scopes.md); for what happens to a captured turn over time, [lifecycle](./lifecycle.md).

## Source map

- `plugin/hooks/hooks.claude.json` — the seven Claude Code events; `plugin/hooks/hooks.codex.json` — the six Codex events. Deliberately not named `hooks.json`: Claude Code loads that default path in addition to the manifest-declared one, so a file there would run every hook twice.
- `plugin/.mcp.json` — the Claude Code MCP registration: `${MEMINI_BASE_URL:-http://localhost:8080}/mcp` (flat expansion; the `/mcp` suffix lives outside the braces) plus the `headersHelper` shell snippet. `plugin/.mcp.codex.json` — the static Codex form (`env_http_headers`).
- `plugin/scripts/session-start.mjs` — briefing fetch, budget/drop order, unchanged-hash skip, directive selection, removed-vars warning, `migrateOverrideToPin`.
- `plugin/scripts/user-prompt-submit.mjs` — prompt-shape gates, counter bump, cooldown exclusion. `plugin/scripts/pre-tool-use.mjs` — tool allowlist, per-file gate, `pretoolFingerprint`, the latch. `plugin/scripts/stop.mjs` — `captureTurn`, stop checkpoint, `autoSaveReasonFor`. `plugin/scripts/pre-compact.mjs`, `plugin/scripts/session-end.mjs`, `plugin/scripts/post-tool-use.mjs` — checkpoints, digest supersession, event buffering. `plugin/scripts/mcp-headers.mjs` — the headers helper.
- `plugin/scripts/_shared.mjs` — `getSessionContext`, `getBriefing` (header-scoped `GET /v1/namespaces/briefing`), `filterFreshTurnEchoes`, `postInjected` (the telemetry beacon).
- `packages/memini-client/src/` — the shared core bundled into the plugin as `_client.gen.mjs`: `handshake.ts` (`performHandshake`, `readCachedHandshake`, `HANDSHAKE_TTL_MS`), `facts.ts` (`gatherFacts`, `factsFingerprint`), `session.ts` (`resolveHarnessCwd`, `processCwd`), `settings.ts` (`BEHAVIOR_KNOBS`, `effectiveSetting`), `bootstrap.ts`, `capture.ts` (`buildTurnCapture`).
