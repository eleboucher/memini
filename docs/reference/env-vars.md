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

## The namespace override outranks the environment

There is one input that beats `MEMINI_NAMESPACE`: the **per-project override**,
stored in `~/.config/memini/overrides.json` and set with `/memini:namespace` (or
the equivalent command in your harness).

```
1. project override      <- ~/.config/memini/overrides.json
2. MEMINI_NAMESPACE
3. config file / git remote / git toplevel / cwd
```

That ordering is deliberate. `MEMINI_NAMESPACE` is a _global_ variable: exported
from a shell profile — or worse, set once as a fish **universal** variable — it
pins every repository on the machine to a single namespace, forever, and nothing
about the symptom points at the cause. Every project's memories pile into one
pool, recall gets noisier, and the agent appears to "remember" things from
unrelated work.

If the environment beat the override, then the command for fixing this would
silently do nothing on precisely the machines that have the problem. So it does
not.

`memini doctor` and `/memini:status` both read the same file and both report the
override when one is in force — they cannot disagree about which namespace is
actually being used, which is the entire point of having a diagnostic.

To see what an override is masking, `/memini:status` prints the counterfactuals:

```
NAMESPACE
  effective              acme/api        <- override
  without the override   default         <- env      (the global pin)
  git/cwd would give     memini          <- git-remote
```

### The overrides file

Every client reads the same file, and three of them (opencode, hermes, openwebui)
reimplement the reader because they cannot import the shared TypeScript. So the
**format is the contract**, and it is deliberately boring:

```json
{
  "version": 1,
  "overrides": {
    "/home/kit/src/phoenix": {
      "namespace": "acme/api",
      "setAt": "2026-07-12T20:30:00Z"
    }
  }
}
```

The key is the **git toplevel** (`git rev-parse --show-toplevel`, absolute),
falling back to the resolved working directory when the project is not a git
repo. Keying on the repository root rather than the raw cwd is what makes an
override set at the top of a repo still apply three directories down.

Two rules any reader must follow:

- Read the file **before** computing the key. Computing the key costs a
  `git rev-parse`, and nobody should pay for one just to discover they have no
  overrides at all, on a path that runs on every hook invocation.
- Every error degrades to "no override", never an exception. A corrupt overrides
  file must not break the agent, and it must not break `memini doctor` either,
  since that is the tool you would reach for to diagnose it.

Namespace values are validated more strictly on the client than on the server:
the namespace travels as the `X-Memini-Namespace` **HTTP header**, so a value
containing CR or LF would split it and let a caller inject arbitrary headers.
Those are rejected rather than normalized away.

## Turning off session digests

`MEMINI_SESSION_DIGEST=0` (client-side) stops the lifecycle hooks recording
**activity**: "edited `auth.go` (3), ran `go test ./...`". Those digests answer
"what was I doing in this repo last week", which some people want and some
emphatically do not. If you only want memini to hold durable facts, every session
otherwise adds a memory that will never answer a question and dilutes recall.

One switch covers all four write sites, because they are the same distilled
buffer: the `SessionEnd` digest, the `Stop` checkpoint, the `PreCompact` rescue
copy, and the `PostToolUse` buffering that feeds them.

It is easy to confuse with the two knobs next to it, so:

| Knob                    | What it turns off                                           |
| ----------------------- | ----------------------------------------------------------- |
| `MEMINI_SESSION_DIGEST` | Activity records: what you edited and ran                   |
| `MEMINI_CAPTURE_TURNS`  | Each user/assistant turn, stored as episodic memory         |
| `MEMINI_INLINE_EXTRACT` | The directive asking the agent to save durable facts itself |

They are independent. Turning digests off leaves the agent saving decisions and
conventions through `memory_remember` exactly as before.

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
