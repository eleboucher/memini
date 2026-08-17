# Examples

End-to-end worked scenarios: each one follows realistic data through the running system — a first deploy, a namespace's whole existence, one fact's life from birth to supersession — quoting the actual payloads sent and the responses that come back. They are the fastest way to see how the [how-it-works](../how-it-works/README.md) mechanisms behave together, in order, on something concrete.

## The "Validated by" convention

Every example ends with a **Validated by** footer naming Go tests. Each behavioral claim in an example — every quoted response field, every "the server does X" — is asserted by one of those tests, so the docs cannot silently drift from the code. The one exception is deploy shell steps (compose, curl to a live host), which are marked illustrative in place.

## The examples

| Example                                       | What it walks through                                                                            |
| --------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| [First memory](first-memory.md)               | Fresh deploy to a remembered fact: one write, the namespace materializes, recall finds it.       |
| [Namespace lifecycle](namespace-lifecycle.md) | A namespace from first write to disappearance: ancestors, visibility, subtree reads, pins.       |
| [A memory's life story](memory-life-story.md) | One fact from birth through classification, reinforcement, correction, time travel, and history. |
| [Recall in practice](recall-in-practice.md)   | One query at three scopes, plus what a session-start briefing does differently.                  |
| [Team sharing](team-sharing.md)               | Two people, one server: personal vs team memories, links, read-only agent keys.                  |

Start with [first memory](first-memory.md) if the server is new to you, or jump straight to [the life story](memory-life-story.md) for the write and read paths end to end.
