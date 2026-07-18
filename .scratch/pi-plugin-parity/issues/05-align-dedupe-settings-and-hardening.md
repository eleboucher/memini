# Align dedupe, settings, and injection hardening

Status: resolved
Type: feature
Blocked by: 01, 03, 04

## What to build

All Pi memory surfaces should share coherent cooldown/dedupe state, honor server and environment behavior settings, avoid noisy searches, and treat stored memory as untrusted background context.

## Acceptance criteria

- [x] `inject_dedupe=false` disables exclusion, filtering, and recording consistently.
- [x] Explicit recall/briefing/get results feed the same state as automatic briefing and prompt recall.
- [x] Updates and deletes make corrected content eligible to surface without waiting for an unrelated session eviction.
- [x] Prompt recall skips short steering text, caps oversized queries, and records prompt-source provenance.
- [x] Label and minimum-capture settings use environment-over-server-over-default precedence.
- [x] Injected memory and server-authored notes are bounded and cannot forge Memini wrapper boundaries.
- [x] Tests cover cooldown modes, cross-surface reads, same-ID updates, degraded search, and poisoning-shaped content.

## Comments

Resolved on `fix/pi-plugin-parity`.

Implementation evidence:

- One persisted v2 state now tracks prompt count plus per-memory content identity,
  timestamp, and prompt number. Automatic briefing/prompt recall use hashes;
  explicit recall/briefing/list/get use a conservative read sentinel. Legacy v1
  branch state migrates safely.
- The resolved `inject_dedupe` setting gates state writes, server exclusions,
  fresh-turn exclusion, client filtering, compaction reset, and read recording.
  Briefing and explicit reads suppress later prompt recall across surfaces.
- Successful update, delete, and id-bearing upsert evict stale read state so a
  corrected/recreated same ID is eligible immediately; changed automatic-read
  content also bypasses stale client-side suppression by hash.
- Prompt recall advances its counter before shape/recall gates, skips blank,
  command-shaped, and sub-12-character steering prompts, caps server queries at
  2000 characters, and sends `source: "prompt"`. Degraded searches with no
  usable hit remain silent.
- `inject_labels` and `min_capture_chars` now resolve with the shared
  environment-over-server-over-default setting path. Minimum capture length is
  enforced on the user side of the settled turn.
- Automatic context uses fixed read-only wrappers, escapes case-insensitive
  Memini-shaped boundary tags before truncation, bounds bullets, summaries,
  provenance, scope, and degraded notes, and keeps explicit tool JSON complete.
  Tool descriptions mark stored data as untrusted read-only reference.
- Focused helper tests cover dedupe-off behavior, time/prompt cooldowns,
  briefing/tool-to-prompt suppression, content changes and correction eviction,
  prompt guards/query provenance, empty degraded search, setting precedence,
  minimum capture, and poisoning-shaped content.
