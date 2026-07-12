# Import, export, and store maintenance

Getting memories in, getting them out, and the handful of one-shot commands for
fixing a store afterwards.

All of these except `memini import --remote` open the configured store
**directly** (`MEMINI_SQLITE_PATH` or `MEMINI_POSTGRES_DSN`), not through a
running server. With the SQLite backend that means a single writer: stop the
server first, or accept that it will contend with you.

## `memini import`

Bulk-loads an export from another memory system, or memini's own, into the local
store or a remote server. Reads stdin when the path is `-` or omitted.

```sh
memini import --source agentmemory ./agentmemory-export.json
```

### Sources

| `--source`         | What it reads                                     |
| ------------------ | ------------------------------------------------- |
| `memini` (default) | memini's own portable JSON, from `memini export`. |
| `agentmemory`      | `rohitg00/agentmemory` export bundle.             |
| `mem0`             | `mem0ai/mem0` `get_all` / export output.          |
| `mnemory`          | `fpytloun/mnemory` export output.                 |
| `claude-code`      | Your Claude Code session transcripts.             |

Each source's fields map onto memini's tiers (agentmemory `workflow` becomes
procedural, mem0 facts become semantic, and so on) and onto a namespace
(`project` / `user_id`). Records whose source carries no recognized tier default
to **episodic**, with a 30-day TTL, so a bulk import of unknown quality ages out
unless recall reinforces it, rather than living forever as durable fact.

The `claude-code` source reconstructs verbatim user/assistant exchanges from
`~/.claude/projects/<project>/<session>.jsonl`, skipping tool-result noise,
sidechains, and slash-command wrappers. It takes a single transcript, a project
directory, or all projects. IDs are deterministic, so re-importing is idempotent.

```sh
memini import --source claude-code ~/.claude/projects
```

### Quality gates

Bulk exports are mostly junk. Two flags drop weak records before they are
written.

| Flag               | Default   | Effect                                                                                |
| ------------------ | --------- | ------------------------------------------------------------------------------------- |
| `--min-length`     | `20`      | Skip records whose trimmed content is shorter than this many bytes. `0` turns it off. |
| `--min-importance` | `0` (off) | Skip records below this importance.                                                   |

`--min-length` is **on by default at 20 bytes**. It drops stubs like "ok" without
you asking.

`--min-importance` is a trap on most sources: a source that carries no importance
reports `0`, so any positive threshold drops every record from it. Use it only
when your export carries real importance scores.

```sh
memini import --source mem0 --min-length 40 --min-importance 0.3 ./export.json
```

### Targeting a running server

`--remote` posts over REST instead of touching the local store, which is how you
import into a server you cannot stop (or a Postgres deployment you would rather
not reach into).

```sh
memini import --source mem0 --remote https://memini.example.com \
  --token "$MEMINI_API_KEY" --namespace acme/phoenix ./mem0-export.json
```

`--token` defaults to `MEMINI_API_KEY`. Over `--remote` the server sets its own
timestamps, so the source's created-at is preserved in
`metadata.imported_created_at` instead.

### The rest of the flags

| Flag                 | Default           | Effect                                                                                                                                                                   |
| -------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `--dry-run`          | off               | Parse and report where records would land. Writes nothing. Use it first.                                                                                                 |
| `--namespace`        | resolved default  | Namespace for records whose source carried none. A fallback, not an override.                                                                                            |
| `--merge-into`       | (none)            | Force _every_ record into one namespace, discarding source namespaces. The original is kept in metadata so `memini namespace split` can undo it. Prompts unless `--yes`. |
| `--importance`       | `0.25`            | Importance for records whose source carried none, so bulk imports rank below curated memories.                                                                           |
| `--confidence`       | (low import seed) | Seed confidence for durable imported facts. Below the default they must earn trust on recall.                                                                            |
| `--no-dedup`         | off               | Skip both the write-time exact-content skip and the post-import vector-cluster pass.                                                                                     |
| `--dedup-similarity` | `0.85`            | Threshold for the post-import dedup pass.                                                                                                                                |
| `--batch`            | backend default   | Records per batch.                                                                                                                                                       |
| `--extract`          | off               | Also distil decisions, preferences, and problems out of conversations into durable semantic memories. No LLM required.                                                   |

Empty records are skipped, and a per-record failure does not abort the run.

Every import stamps its records with a tag: `import:<source>:<date>`. That is
your undo handle, see [`memini forget`](#memini-forget).

## `memini export`

Writes memini's portable JSON. It round-trips: `memini export` then
`memini import` into a fresh store is the supported backup and migration path
(and the only way to change embedding dimensionality, see below).

```sh
memini export --all-namespaces -o backup.json
memini export --namespace acme/phoenix --tier semantic --tier procedural --pretty
```

| Flag                   | Effect                                                      |
| ---------------------- | ----------------------------------------------------------- |
| `--namespace`          | Export one namespace. Defaults to the resolved default.     |
| `--all-namespaces`     | Export every namespace. Each record keeps its own.          |
| `--tier`               | Restrict to these tiers. Repeatable.                        |
| `--tag`                | Only memories carrying _every_ listed tag. Repeatable, AND. |
| `--meta`               | Restrict by metadata. Repeatable.                           |
| `--include-expired`    | Include memories past their TTL.                            |
| `--include-superseded` | Include contradiction-tombstoned memories.                  |
| `-o`, `--output`       | Write to a file instead of stdout.                          |
| `--pretty`             | Indent the JSON.                                            |

By default expired and superseded memories are left out, which is what you want
for a migration and not what you want for a forensic dump. For a true full copy,
pass `--all-namespaces --include-expired --include-superseded`.

## `memini reembed`

### Why the server refuses to start

Vectors from different embedding models are not comparable. A same-dimension
model swap would not error anywhere: it would just quietly return worse results
forever.

So memini records which model produced a store's vectors, and **refuses to boot**
when `MEMINI_EMBED_MODEL` later disagrees:

```
store was created with embedding model "bge-small-en-v1.5" but is configured for
"text-embedding-3-small"; vectors from different models are not comparable, so
recall would silently degrade.
```

You have three ways out, and the error names all three:

1. Put `MEMINI_EMBED_MODEL` back to what the data was built with.
2. Run `memini reembed` to rewrite every vector under the new model.
3. Set `MEMINI_REEMBED_ON_MODEL_CHANGE=true` to have the server do that
   automatically at startup.

Option 3 is off by default for a reason: it hits the embeddings endpoint once per
memory and blocks startup while it does. On a large store that is a long outage,
in a Kubernetes deployment it is a failing readiness probe. Prefer option 2.

### Migrating a model

```sh
# dry-run: how many memories would be re-embedded
MEMINI_EMBED_MODEL=new-model memini reembed

# apply
MEMINI_EMBED_MODEL=new-model memini reembed --yes
```

Dry-run is the default; `--yes` applies. `--namespace` limits the run to one
namespace, `--batch` sets memories per request.

`reembed` deliberately bypasses the boot guard (it is the command that exists to
resolve it). The store's recorded model is updated **only after every vector has
been rewritten**, so an interrupted run leaves the guard pointing at the old
model and you can simply re-run.

### Dimensionality cannot be migrated in place

`reembed` keeps the store's width: the store is fixed at whatever
`MEMINI_EMBED_DIMS` it was created with. Changing dimensions (say 1536 to 1024)
needs a fresh store:

```sh
memini export --all-namespaces -o all.json
# create a new store with the new MEMINI_EMBED_DIMS, then:
memini import all.json
```

## `memini backfill`

Recovery for stores that predate the confidence field (pre-0.0.11). It walks
every namespace and seeds a confidence on durable (semantic and procedural)
memories that have none, so they enter the decay and demotion lifecycle instead
of sitting outside it forever.

```sh
memini backfill        # preview
memini backfill --yes  # apply
```

Dry-run by default. Idempotent: memories that already have a confidence are
skipped, and it reports how many of each.

You need this only if the store is old. On a store created after 0.0.11 it
reports zero seeded.

## `memini forget`

Bulk-deletes every memory in a namespace carrying an exact tag. Its reason to
exist is undoing a bad import, which is why `memini import` tags everything it
writes.

```sh
memini forget --tag import:mem0:2026-06-12
memini forget --tag import:mem0:2026-06-12 --namespace acme/phoenix
```

`--tag` is required. `--namespace` defaults to the resolved default namespace.

There is no dry-run and no confirmation prompt: it deletes. Check what you are
about to remove first:

```sh
memini export --tag import:mem0:2026-06-12 --pretty
```
