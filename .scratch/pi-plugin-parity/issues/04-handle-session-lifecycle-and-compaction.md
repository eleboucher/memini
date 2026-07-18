# Handle Pi session lifecycle and compaction

Status: resolved
Type: feature
Blocked by: 01

## What to build

Memini context should be present when a Pi session starts, remain coherent through resume and reload, survive compaction, and capture only the final settled turn.

## Acceptance criteria

- [ ] A session receives one bounded layered briefing automatically without requiring the model to spend a tool call.
- [ ] Compaction clears context-coupled suppression state and re-injects a fresh briefing before the next model request.
- [ ] Resume/reload behavior neither loses valid suppression state nor duplicates a briefing already present in intact context.
- [ ] Turn capture occurs once after automatic retries, compaction retries, and queued continuations have settled.
- [ ] Pre-compaction or shutdown checkpoint behavior preserves useful activity that ordinary turn text would otherwise lose, where Pi exposes enough evidence.
- [ ] Lifecycle tests cover startup, resume, reload, compaction, retry, and shutdown behavior.

## Comments

Resolved on `fix/pi-plugin-parity`.

Validation evidence:

- `session_start` injects one query-less, per-section/global-token-bounded briefing and skips duplication when an intact briefing remains on startup, resume, or reload; missing context is restored.
- Branch-aware `memini-state` entries persist/reconstruct prompt counters, injected IDs, reset generations, and settled-capture IDs across reload/resume/tree navigation; explicit read tools feed the same suppression state.
- `session_compact` clears context-coupled suppression, persists the generation reset, and queues a forced fresh briefing via steer with `triggerTurn:false`, including overflow retry coverage.
- Turn capture moved from `agent_end` to `agent_settled`, extracts the final successful assistant prose from the active branch, deduplicates by assistant entry ID, and records success only after the write lands.
- Pre-compaction and non-reload shutdown hooks write bounded state-changing-tool digests only when enabled, non-empty, and session-identified.
- Focused lifecycle tests cover startup, resume, reload, tree reconstruction, compaction/overflow retry, settled capture, duplicate settling, checkpoint gates, and shutdown; `npm test`, `npm run typecheck`, and `npm run build` pass.
