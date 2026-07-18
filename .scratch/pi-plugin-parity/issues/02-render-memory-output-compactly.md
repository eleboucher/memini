# Render memory output compactly in Pi

Status: ready-for-agent
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
