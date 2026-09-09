# Handoffs

A **handoff** is the full prompt one agent session writes for the next: what the next step is, what has to happen first, which constraints hold, which files to read. It exists because context does not survive a `/clear`, a new machine, or a switch to another agent harness — and re-deriving it costs more than writing it down did.

Everything else memini stores is a fact it retrieves for you. A handoff is the opposite: an instruction you retrieve deliberately, exactly once, at the start of the work it describes.

That inversion is the whole design. A handoff runs to 100-300 lines, so the rules that make a good fact make a terrible handoff:

|                   | a pinned memory              | a handoff                                    |
| ----------------- | ---------------------------- | -------------------------------------------- |
| Size              | one or two sentences         | 100-300 lines                                |
| In recall results | yes                          | never, unless you ask for it                 |
| In the briefing   | its content, up to 280 chars | a one-line pointer to fetch it               |
| How many are live | as many as you pin           | one per slot                                 |
| Replaced by       | editing it                   | writing the next one                         |
| Read as           | background context           | an instruction addressed to the next session |

## Slots

A handoff lives in a **slot**, named by `metadata.handoff_slot` and defaulting to `main`. One live handoff per slot per namespace: saving to a slot supersedes whatever that slot held, and the old prompt stays readable through the memory's history rather than being overwritten.

Slots exist so parallel lines of work do not overwrite each other. A worktree, a long-running feature branch, or a second agent working the same repo each take their own slot. Everything in one namespace, no separate namespace needed.

## Saving one

Tag the write `handoff` and pass the prompt as `content`. Nothing else is required.

```jsonc
{
  "content": "# Handoff: memini — execute stage 4\n\n## 1. Project & task\n...",
  "summary": "execute stage 4",
  "tags": ["handoff"],
  "metadata": {
    "handoff_slot": "wt-polygons", // omit for "main"
    "handoff_harness": "claude-code",
    "handoff_cwd": "/home/me/dev/memini",
    "handoff_lines": 212,
  },
}
```

Three things the server does for you, each of which would otherwise be a trap:

- **It picks the `procedural` tier.** A prompt is far past the 400-rune ceiling on [tier classification](how-it-works/write-path.md#how-the-tier-is-chosen), so an unclassified write would land in `working` and expire in 72 hours — losing precisely the memory that has to outlive the session that wrote it.
- **It supersedes the slot's previous handoff** once the new one is durably stored, so a slot always names one current prompt.
- **It stamps the slot and the memory type**, so both are queryable.

Two things not to pass. Do not pass an `id`: that upserts, which overwrites the previous prompt in place and destroys it, where supersession would have kept it. Do not summarize the prompt into the `content`, either — a fresh session needs what the last one actually wrote.

`summary` is worth care. It is all a fresh session sees before it decides whether to fetch, so it should name the next step, not the project.

## Resuming one

The session briefing carries a pointer per slot. The plugin renders it above every other section:

```
Handoff waiting (not loaded — fetch to resume):
- [main] (2026-09-09, from claude-code, 212 lines): "execute stage 4" — memory_get 1a2b3c4d5e6f7a8b
```

Fetch it with `memory_get` on that id, then follow it as the user's own instruction rather than weighing it as background. After acting on it, stamp `consumed_at` and `consumed_by` through `memory_update`.

Mind how that update handles lists. `tags` and `metadata` each **replace** the stored value wholesale when you pass them, and are left untouched when you omit them. So omit `tags`, and send the full existing `metadata` plus the two new keys — a partial list would strip the `handoff` tag or the slot, and orphan the record.

A consumed handoff keeps being listed, annotated with who resumed it and when. Hiding it would strand a session that was interrupted part-way through a resume, and a resumed prompt is still the truth about where the work stands. The annotation is a prompt to check before redoing the work, not a lock.

## Why it stays out of recall

Search does not return handoffs. Pass `include_handoffs: true` (REST `/v1/search`, or the `memory_recall` argument) to search across them.

The exclusion is not squeamishness about size. A handoff names the project, its files, its constraints and its vocabulary, all in one document — so it shares lexical and embedding surface with nearly every question anyone asks about that project. Left in the corpus it would rank plausibly for all of them and spend top-k slots that belong to real facts, on a document whose answer to any specific question is "read all 300 lines".

The same reasoning keeps handoffs out of the briefing's content sections. A 280-character fragment of a 300-line prompt is not usable as instruction and displaces a procedure that would have been.

Two consequences follow from the same principle:

- **The pointer is exempt from the briefing's token budget**, on both the server and the client. A pointer is one line, and it is the one item whose absence a fresh session has no way to detect: starve it and the session never learns that instructions were waiting.
- **Pointers never cross a namespace boundary.** A briefing draws facts from ancestors, your personal namespace and links, but handoffs come from the briefed namespace alone. An inherited handoff would tell this session to resume work in a different project.

## What a handoff is not

A handoff is not a fact, and memini treats it accordingly: it is exempt from [demotion](how-it-works/lifecycle.md#moving-down-demotion), and it is held out of corroboration and contradiction routing. Similarity machinery that reads two documents as "the same claim, restated" is right about facts and wrong about prompts — two consecutive handoffs for one project are near-identical by construction, and the lines that differ are the entire point.

So keep writing the ordinary durable memory too. One paragraph on where the work stands is what recall and the briefing's content sections can actually use; the handoff is what the next session executes.

## Cross-harness handoffs

The stored prompt is plain text, so any harness can fetch one written by another. `handoff_harness` records who wrote it, and the briefing pointer shows it, because that is the part a reader needs before trusting the prompt.

What does not travel is harness-specific instruction. A prompt that tells the next session to invoke a particular skill, or to read a rules directory that only one harness auto-loads, is not portable. Write the task, the references, the acceptance criteria and the constraints as harness-neutral prose, and keep the bootstrap steps in their own section that a different harness can skip.

## Reference

| What                                         | Where                                                                     |
| -------------------------------------------- | ------------------------------------------------------------------------- |
| Tag, slot resolution, supersession, pointers | `internal/service/handoff.go`                                             |
| Tier default, recall exclusion               | `internal/service/service.go` — `defaultTier`, `recallExcludeMetadata`    |
| Briefing index                               | `internal/service/query.go` — `Briefing`; budget exemption in `budget.go` |
| Demotion exemption                           | `internal/maintenance/demote.go` — `HandoffTag`                           |
| Wire shape                                   | `api/openapi.yaml` — `HandoffPointer`, `Briefing.handoffs`                |
| Briefing render                              | `plugin/scripts/session-start.mjs` — `formatHandoffs`                     |
| Agent-facing workflow                        | `plugin/skills/handoff/SKILL.md`, `plugin/commands/handoff.md`            |
