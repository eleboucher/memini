# memini + OpenClaw

OpenClaw is a self-hosted gateway that drives coding agents (it runs Claude Code
and Codex as managed sessions) and supports skills.

Two complementary ways to wire memini:

1. **Through the managed agent.** OpenClaw runs Claude Code / Codex — configure
   memini there using the [`claude-code/`](../claude-code/) or [`codex/`](../codex/)
   recipes, and the managed sessions inherit memory.

2. **As an OpenClaw skill.** Install [`skills/memory/SKILL.md`](skills/memory/SKILL.md)
   so OpenClaw itself knows to remember/recall via memini's REST API across its
   wake/goal loops. Set `MEMINI_URL` and any bearer token in the gateway
   environment. The namespace comes from `MEMINI_NS` or the `X-Memini-Namespace`
   header in the skill's curl calls.

Use the same namespace everywhere to share one memory across the gateway and its
managed coding agents.
