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
- `summary` (optional) — short summary; defaults to first 200 chars of content
- `confidence` (optional) — 0..1 for durable (`semantic`/`procedural`) facts;
  omit to let it start uncorroborated and earn trust as the fact recurs. Durable
  facts gain confidence each time they're re-observed and lose it if never
  recalled, so saving a real, lasting fact as `semantic` (not `working`) is what
  lets it rise above one-off noise over time.

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

Call with tier `semantic`:

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
