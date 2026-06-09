# memini + Hermes (NousResearch)

Hermes Agent is an MCP client: it discovers tools from configured MCP servers at
startup and supports per-server filtering. Add memini as a server in your Hermes
config (consult the Hermes docs for the exact file location/format; the entries
below follow the common MCP-server shape).

**Remote (Streamable HTTP):** see [`hermes.mcp.json`](hermes.mcp.json).

**Local (stdio):** see [`hermes.mcp.local.json`](hermes.mcp.local.json).

Restrict Hermes to memini's memory tools using its per-server tool filtering
(allow `memory_remember`, `memory_recall`, `memory_get`, `memory_forget`).

Set the namespace (`X-Memini-Namespace` header for remote, or
`MEMINI_DEFAULT_NAMESPACE` for stdio) to share memory with your other agents.
The `my-project` placeholder in the JSON files can be replaced with your real
namespace, or removed entirely — memini auto-resolves the namespace from the
git repo basename of its own working directory when no namespace is set.
