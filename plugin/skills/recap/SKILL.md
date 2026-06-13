---
name: recap
description: >-
  Summarize what's known about a project, area, or topic from memini. Use
  this skill when the user asks "catch me up on X", "what's the state of
  Y", or "summarize what we know about Z". Builds on recall with a
  larger limit and tiered grouping. The MCP tool name is `memory_recall`.
---

# recap (memini)

memini is a memory service. The fastest recap is the `memory_briefing` tool: it
returns pinned context, durable facts, how-to procedures, and recent activity
for the namespace in one query-less call, already grouped — use it for a
"where are we" overview. For a topic-specific recap, call `memory_recall` with a
broad query and a higher `limit` (10–20) and group the results by `tier`.

## When to call

- The user starts a new session and wants a "where are we" overview.
- The user is taking over work from another agent (or themselves, last
  week).
- Onboarding onto a project the user has been working on for a while.

## Pattern

Call `memory_recall` once with a broad query and `limit: 20`. Don't make
multiple small queries — memini's RRF ranking already handles breadth. When the
recap is scoped to a known subject rather than a fuzzy topic ("catch me up on
the deployment runbook"), use `memory_list` with `tags` / `metadata` filters
instead — it browses that bucket without a query (see the `recall` skill).

Group the response into:

1. **Decisions & conventions** (tier `semantic`) — durable facts about the
   project that should constrain current work.
2. **Procedures** (tier `procedural`) — how-to steps the user will need.
3. **Recent events** (tier `episodic`) — what happened recently, ordered
   by recency if possible.

Don't include working-tier memories in a recap — those are transient by
design.

When you attribute a decision or fact to memory, quote the stored content
verbatim rather than paraphrasing it into something firmer than it says.

If the result set is empty, say so. Don't fabricate a recap from general
knowledge of the codebase. If memini is unreachable, report that and
suggest `memini doctor` rather than guessing.
