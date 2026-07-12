# Server variables vs client variables

memini has two sides, and both read `MEMINI_*` environment variables.

The **server** is the `memini` process. It reads the settings in the
[configuration reference](configuration.md), which control storage, embeddings,
retrieval and the background lifecycle.

The **client** is the plugin that runs inside your agent (the Claude Code hooks,
the opencode plugin, and so on). It reads its own set, documented in
[`plugin/README.md`](../../plugin/README.md), which control where to find the
server and how much context to inject into a prompt.

Most of the time the distinction does not matter, because the two sets do not
overlap. Four names do overlap, and they mean different things on each side.
Those four are the reason this page exists.

## The four that mean two things

| Variable           | On the server                                                                                  | On the client                                                                                     |
| ------------------ | ---------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| `MEMINI_API_KEY`   | The admin credential the server **requires** from callers. Setting it turns authentication on. | The bearer token the client **sends**. Setting it does not turn anything on.                      |
| `MEMINI_NAMESPACE` | A fallback, used only when a request arrives with no `X-Memini-Namespace` header.              | The namespace the client **sends** on every call, overriding its own git and directory detection. |
| `MEMINI_HOME`      | Read only by `memini mcp` (stdio has no headers, so there is nowhere else to get it).          | The caller's personal namespace, sent as `X-Memini-Home` on every call.                           |
| `MEMINI_AGENT`     | Nothing. The server's own namespace fallback ignores it.                                       | Nests the client's resolved namespace under a per-agent segment.                                  |

`MEMINI_AGENT` is worth calling out. It is applied by the plugin's namespace
resolution, not by the server's header-less fallback, so setting it and then
running a bare `memini mcp` with no plugin does nothing at all. Set
`MEMINI_NAMESPACE` to the full path (`acme/phoenix/reviewer`) instead.
`memini doctor` resolves the namespace both ways and tells you when they differ.

The practical consequence of the overlap: exporting one of these in your shell
configures **both** the server you start from that shell and the agent you start
from it.

That is usually what you want on a laptop, where the two are the same machine
and the same person. It is usually wrong on a shared server, where
`MEMINI_API_KEY` on the server means "demand this token" and on a developer's
machine means "send this token". Setting the same value in both places is how
you end up sharing one credential across a team instead of issuing
[named keys](../api-keys.md).

## Which side am I configuring?

Ask where the process runs.

- Editing a Compose file, a Helm values file, a systemd unit, or a Dockerfile?
  That is the **server**. Use the
  [configuration reference](configuration.md).
- Editing your shell profile, an MCP client config, or an agent's settings?
  That is the **client**. Use [`plugin/README.md`](../../plugin/README.md).

In the guides, every shell block says which side it is configuring, for exactly
this reason.

## A note on aliases

The client accepts a few older names, which the server does not:

- `MEMINI_URL` is an alias for `MEMINI_BASE_URL`.
- `MEMINI_TOKEN` is an alias for `MEMINI_API_KEY`.

The server has no `MEMINI_BASE_URL`. It has `MEMINI_HTTP_ADDR`, which is the
address it binds rather than an address it calls. If you find yourself setting a
base URL on the server, you are configuring the wrong side.
