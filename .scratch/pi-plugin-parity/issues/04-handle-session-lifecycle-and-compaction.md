# Handle Pi session lifecycle and compaction

Status: ready-for-agent
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
