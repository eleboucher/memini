# Align dedupe, settings, and injection hardening

Status: ready-for-agent
Type: feature
Blocked by: 01, 03, 04

## What to build

All Pi memory surfaces should share coherent cooldown/dedupe state, honor server and environment behavior settings, avoid noisy searches, and treat stored memory as untrusted background context.

## Acceptance criteria

- [ ] `inject_dedupe=false` disables exclusion, filtering, and recording consistently.
- [ ] Explicit recall/briefing/get results feed the same state as automatic briefing and prompt recall.
- [ ] Updates and deletes make corrected content eligible to surface without waiting for an unrelated session eviction.
- [ ] Prompt recall skips short steering text, caps oversized queries, and records prompt-source provenance.
- [ ] Label and minimum-capture settings use environment-over-server-over-default precedence.
- [ ] Injected memory and server-authored notes are bounded and cannot forge Memini wrapper boundaries.
- [ ] Tests cover cooldown modes, cross-surface reads, same-ID updates, degraded search, and poisoning-shaped content.

## Comments
