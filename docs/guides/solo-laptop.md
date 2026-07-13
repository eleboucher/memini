# Solo laptop

One developer, one machine, no cloud. memini runs as a local process on an
embedded SQLite file, talks to an embeddings endpoint you already have running,
and does not use a chat model at all. This is the smallest useful deployment and
it is a genuinely supported one, not a crippled demo mode.

## The server

memini starts with no configuration whatsoever, which is exactly why the first
thing to get right is the part it cannot invent: the embeddings endpoint.

```sh
# server (the memini process)
export MEMINI_SQLITE_PATH="$HOME/.local/share/memini/memini.db"
export MEMINI_EMBED_BASE_URL=http://127.0.0.1:8081/v1
export MEMINI_EMBED_MODEL=bge-m3
export MEMINI_EMBED_DIMS=1024
export MEMINI_UI_ENABLED=true          # the default; the admin UI at http://127.0.0.1:8080/
memini
```

Any OpenAI-compatible `/embeddings` endpoint works:
[text-embeddings-inference](https://github.com/huggingface/text-embeddings-inference),
llama.cpp, vLLM, LM Studio, or a hosted API. memini does not ship one.

Three things about that block deserve more than a glance.

**`MEMINI_EMBED_DIMS` must match the model, exactly.** The default (`1536`) suits
`text-embedding-3-small`. If you point at a 768- or 1024-dimension model and
leave the default in place, memini does not fail: it writes vectors that mean
nothing into a store shaped for a different model, and recall silently gets
worse. There is no error to catch, and unlike the model name, dimensionality
cannot be migrated in place afterwards (see
[`MEMINI_EMBED_DIMS`](../reference/configuration.md#memini_embed_dims)). Check the
model card, set the number, and only then start writing memories.

**`MEMINI_SQLITE_PATH` defaults to `memini.db` relative to the working
directory.** For a long-lived laptop install that is a trap: launch memini from a
different directory and it opens a different, empty database. Give it an absolute
path under `~/.local/share` (or wherever you keep application state) and forget
about it.

**The admin UI is on by default** at `/`, on the same port as the API and the MCP
endpoint. On a laptop with no `MEMINI_API_KEY` set, that is fine: the listener is
local, there is no token to leak, and the UI is the fastest way to see what your
agent has actually stored. (If you do set an admin key, read the security note in
the [homelab guide](homelab-team.md#the-ui-embeds-your-api-key), because the rules
change.)

## No LLM is a real mode, not a degraded one

`MEMINI_LLM_BASE_URL` is unset here, and that is the point. Without a chat model,
memini still runs the entire memory lifecycle on marker heuristics:

- **Write-time extraction.** A terse, unhedged decision, preference, or problem
  statement is pulled out of a raw capture into a durable typed fact.
- **Tier classification.** A write with no explicit `tier` is classified rather
  than dumped into `working`.
- **Promotion.** A short-term memory recalled enough times
  ([`MEMINI_PROMOTE_MIN_ACCESS`](../reference/configuration.md#memini_promote_min_access),
  default 3) is distilled into a durable fact by the same extractor.
- **Corroboration.** A write that restates an existing durable fact grows that
  fact's confidence instead of piling up beside it.
- **Contradiction.** A durable write that contradicts a stored fact invalidates
  the stale one, so current state wins recall.

See [tiers](../tiers.md) for the whole lifecycle. What you give up by having no
LLM is a short list: background consolidation, `POST /v1/answer`, the
`memory_answer` MCP tool (which is not even advertised to agents when no LLM is
configured, so nobody tries to call it), and `MEMINI_RERANK=llm`. Retrieval
itself, the part that determines whether recall is any good, does not involve a
chat model on any path.

## Wire up your agent

The Claude Code plugin is the shortest route. It resolves the namespace per
project, sends it on every call, and registers the MCP server for you.

```
/plugin marketplace add eleboucher/memini
/plugin install memini
```

Once it is installed, `/memini:status` tells you what the plugin is actually
doing — the namespace it resolved and _where that came from_, the server it is
talking to, and the read set a recall would draw on. Run it whenever memory is
behaving in a way you did not expect; it is read-only and it redacts secrets, so
the output is safe to paste into an issue.

Then, in the environment your agent runs in:

```sh
# client (your agent host)
export MEMINI_BASE_URL=http://127.0.0.1:8080
export MEMINI_HOME=personal/kit
```

`MEMINI_HOME` is the one that pays off over months. It names your personal
namespace, sent as `X-Memini-Home` on every call, and it makes memini merge that
namespace's durable memories into every recall no matter which project you are
in. Your conventions and preferences follow you across repos; project facts stay
in their project. It is also where a `visibility:"personal"` write lands, and
without it such a write errors. Use `personal/<your-name>`.

Note that `MEMINI_HOME` appears on both sides of the wire and means different
things. On the client it is what the plugin sends. On a server it is what the
stdio MCP server (`memini mcp`) reads directly, since stdio carries no headers.
Three other names, `MEMINI_API_KEY`, `MEMINI_NAMESPACE`, and
`MEMINI_DEFAULT_NAMESPACE`, have the same split personality.

**Without the plugin**, wire `memini mcp` into any MCP client as a stdio launch
command. That process is spawned by your agent, so its environment is your client
environment, and it reads `MEMINI_NAMESPACE` and `MEMINI_HOME` straight from
there rather than from headers.

## Verify it

```sh
memini doctor
```

`doctor` opens the store directly, so it needs no running server. It prints how
the namespace resolves for writes versus recall, the effective read set for the
current directory, and per-namespace memory counts:

```
Namespace resolution (cwd: /home/kit/src/phoenix)
  env override:    (unset)
  server default:  "phoenix" (git)
  plugin resolves: "phoenix" (git-remote)
  home namespace:  "personal/kit"

Effective read set for "phoenix" (source: local store)
  NAMESPACE     ORIGIN   TIERS
  phoenix       primary  all
  personal/kit  home     semantic,procedural

Store (sqlite): reachable, 2 namespace(s)
```

Then ask your agent to remember something and recall it back. If the round trip
works and `doctor` reports no warnings, you are done. If recall comes back empty,
the cause is almost always a namespace mismatch (writes landing somewhere recall
does not look), which `doctor` calls out explicitly. Start at
[tuning recall](tuning-recall.md) before touching any ranking setting.

Upgrading from an older install and `doctor` flags a leftover
`~/.config/memini/overrides.json`? Nothing reads it anymore — it retired in
favor of server-side pins. `/memini:namespace` picks up a matching entry for
the current project automatically on its next `SessionStart`; running
`/memini:namespace --migrate` once handles every project on the machine at
once. See [env-vars.md](../reference/env-vars.md#retired-local-state-overridesjson-and-configjsons-tenantroots).

## Changing the embedding model later

memini records which model produced a store's vectors and refuses to start when
`MEMINI_EMBED_MODEL` later disagrees, because vectors from two models are not
comparable and a silent swap degrades recall with no error at all. To move:

```sh
# server (the memini process)
export MEMINI_EMBED_MODEL=the-new-model
memini reembed          # re-embeds every memory, then records the new model
```

Or set
[`MEMINI_REEMBED_ON_MODEL_CHANGE=true`](../reference/configuration.md#memini_reembed_on_model_change)
to have the server do it at startup. That call blocks boot and hits the
embeddings endpoint once per memory, which is why it is opt-in.

Dimensionality is the exception: `memini reembed` cannot change it. A different
`MEMINI_EMBED_DIMS` needs a fresh store (`memini export`, then `memini import`).
Which is the second reason to get it right the first time.
