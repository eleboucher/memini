---
description: Search memini for what you know about something
argument-hint: "<what to search for>"
---

Search memini using the `memory_recall` MCP tool:

$ARGUMENTS

Guidance:

- Use a short, descriptive query. `memory_recall` is hybrid (semantic + keyword),
  so natural phrasing beats keyword soup — "JWT auth setup", not "jwt auth token
  login config".
- Default `scope` (`full`) reads this project plus inherited context — ancestor
  namespaces, your personal namespace, and any links. Use `project` to see only
  this project's own memories, or `everywhere` to also reach into nested
  sub-projects.
- If the first query comes back thin, try `query_rewrite: true` — it expands the
  query into variants and fuses the results, which finds things a single phrasing
  misses.

Then report what you found:

- Summarize the memories, do not dump the raw JSON.
- Each result carries a `from` field. An empty one means it is this project's own
  memory; anything else names the namespace it was inherited from. Mention it when
  it matters — "that came from your personal namespace, not this project" is often
  the useful part of the answer.
- Results carry `created_at`. If two memories conflict, prefer the newer one and
  **say that they conflict** rather than silently picking.
- **If nothing comes back, say so.** Empty means nothing is known. Do not fill the
  gap by inventing a plausible "remembered" fact — answer from first principles
  and make clear that is what you are doing.
