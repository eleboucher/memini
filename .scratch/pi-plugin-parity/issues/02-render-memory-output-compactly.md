# Render memory output compactly in Pi

Status: resolved
Type: feature
Blocked by: None

## What to build

Memini should remain fully informative to the model without dumping raw JSON or large automatic-recall blocks into the human transcript.

## Acceptance criteria

- [ ] Explicit memory tools render a concise one-line result by default instead of escaped raw JSON.
- [ ] Expanded rendering shows bounded, human-readable details such as count, tier, score, provenance, and short summaries.
- [ ] Full structured content remains unchanged and available to the model and session log.
- [ ] Automatic recall messages use a compact or hidden human-facing presentation while remaining in model context.
- [ ] Renderer tests cover large recall payloads and error/degraded results.

## Comments

Resolved on `fix/pi-plugin-parity`.

Validation evidence:

- All native memory tools now share compact `renderCall`/`renderResult` functions backed by typed renderer details; collapsed output is one line and expanded output is capped at eight items with tier, score, provenance, and short summaries.
- Automatic `memini-recall` and `memini-briefing` messages use registered compact message renderers.
- Tool `content[0].text` remains the complete JSON payload; renderer tests assert content is byte-for-byte unchanged after collapsed and expanded rendering.
- Large, degraded, unavailable, and explicit error results are covered by focused tests; `npm test`, `npm run typecheck`, and `npm run build` pass.
