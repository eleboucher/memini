# memini + opencode

opencode reads MCP servers from `opencode.json` under `mcp`.

**Remote (recommended):** merge [`opencode.json`](opencode.json) into your config.

**Local (stdio):** see [`opencode.local.json`](opencode.local.json) — opencode
spawns `memini mcp`.

Then in opencode the `memory_*` tools are available to the model. Mind the
context budget: memini adds a handful of tools, which is light, but disable
other large MCP servers you aren't using.

Use the same `X-Memini-Namespace` as your other agents to share memory.
`my-project` is a placeholder — replace it with your real project name, or
remove the header to let memini auto-resolve the namespace from the git repo
basename of its own working directory.
