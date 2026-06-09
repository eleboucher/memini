<!-- Append to your project CLAUDE.md to make Claude use memini. -->

## Memory (memini)

You have a persistent memory via the `memini` MCP server.

- At the **start of a task**, call `memory_recall` with a query describing the
  task to load any relevant prior decisions, conventions, or gotchas.
- When you learn a **durable fact** about this project (an architectural
  decision, a convention, a non-obvious constraint), call `memory_remember`
  with `tier: "semantic"`.
- For transient task notes use `tier: "working"`; for "what happened this
  session" use `tier: "episodic"`.
- Prefer recalling before asking the user something you may have stored before.
