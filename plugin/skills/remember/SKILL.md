---
name: remember
description: >-
  Save a durable fact, decision, or preference to memini. Use this skill
  proactively when the user says "remember this", after discovering a bug,
  after making an architectural decision, after learning a project
  convention, or whenever something learned in this session should outlive
  the session itself. The MCP tool name is `memory_remember`.
---

# remember (memini)

memini is a memory service. To save a fact, call the `memory_remember` MCP
tool with:

- `content` (required) — the fact itself, atomic and self-contained
- `tier` — `semantic` for durable knowledge, `procedural` for how-to,
  `episodic` for events, `working` for transient notes
- `tags` (optional) — array of keywords for later search; tag a critical,
  always-relevant fact (the user's identity, a hard constraint) `pinned` so it
  surfaces in every session briefing
- `metadata.category` (optional) — a topic bucket so the memory can be browsed
  and filtered by subject later (e.g. `bug_fixes`, `architecture_decisions`,
  `coding_conventions`). Use a canonical value from `docs/categories.md`. This is
  orthogonal to `tier`: tier is the memory's _kind_, category is its _topic_.
- `summary` (optional) — short summary; defaults to first 200 chars of content
- `confidence` (optional) — 0..1 for durable (`semantic`/`procedural`) facts;
  omit to let it start uncorroborated and earn trust as the fact recurs. Durable
  facts gain confidence each time they're re-observed and lose it if never
  recalled, so saving a real, lasting fact as `semantic` (not `working`) is what
  lets it rise above one-off noise over time.

## Result fields to check

- `stored: false` — the write was dropped by the episodic value gate (low-signal
  content, e.g. too short); not an error, just not saved. Rephrase with more
  substance if it's actually worth keeping.
- `merge_hint` — present when the content nearly duplicates an existing memory
  (`merge_hint.similar_id`, `.similar_content`, `.score`). The new content was
  still stored as its own memory; call the `memory_update` MCP tool with
  `id: merge_hint.similar_id` to fold the new information into the existing
  memory instead of leaving two near-duplicates, or ignore the hint to keep
  both as-is.
- `degraded: "pending_embed"` — embeddings were unavailable at write time, so
  the memory was stored without a vector (still keyword-searchable). It is
  backfilled with a vector automatically in the background — don't retry the
  write or treat this as a failure.

## When to call

- The user says "remember this", "save this", "don't forget", "note that".
- You discover a non-obvious bug or root cause worth flagging next time.
- You make an architectural decision (e.g. "we chose jose over jsonwebtoken
  for Edge compatibility"). Capture the _why_, not just the _what_.
- You learn a project convention (test layout, deploy command, code style).
- The user states a preference (response style, formatting, tool choice).

## When NOT to call

- The fact is already in `CLAUDE.md`, `.cursorrules`, or project docs.
- The fact is recoverable trivially from the codebase.
- The fact is a one-off (e.g. "today is Tuesday") with no future relevance.

## Examples

Call with tier `semantic`, `metadata.category` `architecture_decisions`:

> The auth middleware uses `jose` rather than `jsonwebtoken` because we
> deploy to Cloudflare Workers, which can't run jsonwebtoken's C++ bindings.

Call with tier `procedural`:

> To regenerate the embedding cache, run `mise run embeddings:warm`.

Call with tier `episodic`:

> The 2026-06-09 outage was caused by a Postgres connection-pool
> exhaustion under load. The fix was raising `max_connections` to 200
> and adding pgbouncer.

Keep memories atomic — one fact per call. Search works better on small,
focused records than on walls of text.
