---
name: remember
description: >-
  Save a durable fact, decision, or preference to memini. Use this skill
  proactively — do not wait to be asked — when the user says "remember this"
  or corrects you, after discovering a bug's root cause, after making an
  architectural decision, after learning a project convention or environment
  quirk, or whenever something learned in this session should outlive the
  session itself. The MCP tool is `memory_remember` on the bundled `memini` server.
---

<!-- Save-policy invariants (when to call, when not, secrets exclusion) are
canonical in internal/api/mcp/mcp.go serverInstructions; keep this in sync. -->

# remember (memini)

memini is a memory service. To save a fact, call the `memory_remember` MCP
tool with:

- `content` (required) — the fact itself, atomic and self-contained
- `tier` — `semantic` for durable knowledge, `procedural` for how-to,
  `episodic` for events, `working` for transient notes; omit to let the server
  auto-classify
- `tags` (optional) — array of keywords for later search; tag a critical,
  always-relevant fact (the user's identity, a hard constraint) `pinned` so it
  surfaces in every session briefing
- `metadata.category` (optional) — a topic bucket so the memory can be browsed
  and filtered by subject later. This is orthogonal to `tier`: tier is the
  memory's _kind_, category is its _topic_. Any string is accepted, but pick one
  of the canonical values so filtering keeps working: `architecture_decisions`,
  `anti_patterns`, `task_learnings`, `tooling_setup`, `bug_fixes`,
  `coding_conventions`, `user_preferences`, `dependency_decisions`,
  `performance_findings`, `security_constraints`, `testing_patterns`,
  `data_model`, `api_contracts`, `deployment_runbook`, `team_norms`,
  `domain_glossary`, `experiment_results`.
- `visibility` (optional) — who should know this: `project` (default, this
  project only), `personal` (about the user, follows them everywhere), or an
  ancestor namespace name read off the `memory_briefing` Scope line (e.g. the
  team or org level) to share it up that chain. An unrecognized ancestor name
  errors listing the valid chain. Ignored for `episodic`/`working` writes —
  those always stay in the project regardless of `visibility`, so a session
  digest can never pollute a shared ancestor or the user's personal namespace.
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
- `reinforced: true` — the fact was already known: no new memory was created, the
  existing one was strengthened, and `id` names that pre-existing memory rather
  than anything you just wrote. Don't report it to the user as a new save, and be
  careful updating or forgetting that id.
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

Do not wait to be asked. Capture at the moment of learning, not at session end.

- The user says "remember this", "save this", "don't forget", "note that",
  "next time", "going forward", "always/never do X". Call `memory_remember`
  FIRST, then acknowledge. On an explicit request, save unconditionally —
  even if it seems trivial, fleeting, or already stored (the server
  reinforces or merges near-duplicates; that is its job, not yours).
- The user corrects you. A correction IS a preference — capture it.
- You make an architectural decision (e.g. "we chose jose over jsonwebtoken
  for Edge compatibility"). Capture the _why_, not just the _what_.
- You discover a non-obvious bug or root cause worth flagging next time.
- You learn a project convention (test layout, deploy command, code style).
- You hit an environment or tool quirk (e.g. "tests need DOCKER_HOST unset
  on this machine").
- You learn a non-obvious command or workflow the project depends on.

## When NOT to call

- Secrets, credentials, API keys, tokens — never, even on explicit request
  (this outranks the save-unconditionally rule; explain and point to a
  secret manager instead).
- Transient session state (what file you just opened, the branch you happen
  to be on mid-task).
- Task progress or TODO state — the plugin's session digests capture
  activity automatically; memories are for what stays true.
- Facts already in `CLAUDE.md`, `.cursorrules`, or project docs, or
  trivially recoverable from the codebase.
- Text the user is merely asking you to translate, edit, or summarize —
  content passing through your hands is not a fact about the user or
  project.

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

State facts, not commands — a memory is a claim about the world, not an
instruction to your future self:

- Good: "User prefers pnpm over npm in all their projects" (declarative,
  visibility `personal`)
- Bad: "Always use pnpm" (a command — future recall cannot tell where it
  applies or why)
- Good: "TestAuthRefresh flakes because of a hard-coded 100ms timeout in
  auth/refresh_test.go; the fix pattern is deadline-based waiting"
- Bad: "Fixed a flaky test today" (not self-contained, no future value)

If a stored memory turns out to be wrong or outdated, fix it immediately:
correct it in place with `memory_update`, or delete it with `memory_forget`
if it should not exist. Never leave a memory you know is incorrect in place.

Keep memories atomic — one fact per call. Search works better on small,
focused records than on walls of text.
