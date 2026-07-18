# PRD: Bring the Memini Pi plugin up to parity

Status: approved

## Problem

The Pi extension has drifted from the Claude Code/MCP integration. Automatic recall currently sends an invalid namespace header, lifecycle handling loses or suppresses memory around compaction, the native tool contract is incomplete, and raw JSON tool results flood the Pi transcript.

## Outcome

The Pi extension should preserve complete memory context for the model while presenting concise output to the user, use the server-authoritative namespace consistently, follow Pi's current session lifecycle, expose the current Memini tool contract, and honor the same behavioral settings and dedupe semantics as the maintained Claude integration where Pi provides an equivalent hook.

## Constraints

- Work lands on branch `fix/pi-plugin-parity`.
- Keep full structured tool content available to the model; compact rendering is a TUI concern only.
- Use current Pi extension APIs, including session lifecycle events and custom renderers.
- Do not modify or delete pre-existing untracked files or build artifacts.
- Add focused regression tests and finish with build, typecheck, package tests, and the full relevant test suite.

## Source evidence

- Claude plugin: `plugin/`
- Pi extension: `integrations/pi/plugin/`
- Generated MCP contract: `docs/reference/mcp-tools.md`
- Audit artifacts: `.pi-subagents/artifacts/outputs/2617ec5b-ef11-4354-a8d5-cb4f3ebf5d85/`
