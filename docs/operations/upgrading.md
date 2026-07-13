# Upgrading

What breaks between versions, and what to do about it.

memini treats removed configuration in two ways. Most removed variables are
**ignored with a startup warning**: the server boots, but a value you were
relying on silently stops applying. Two are **fatal**: the server refuses to
boot at all, because booting as if they were never set would quietly change
what your agents can see.

Start here if the server will not start.

## "My server will not start"

If the boot fails with a message like:

```
fatal: MEMINI_GLOBAL_NAMESPACE is set; the scope model changed: namespaces are now
always merged via the ancestor cascade, replacing the old opt-in global namespace...
```

then one of these two variables is still in your environment:

| Variable                  | What it used to do                                                       |
| ------------------------- | ------------------------------------------------------------------------ |
| `MEMINI_GLOBAL_NAMESPACE` | Merged one namespace read-only into _every_ namespace, store-wide.       |
| `MEMINI_TENANT_SHARED`    | Merged `<tenant>/_shared` read-only into every sibling under `<tenant>`. |

Both named the old **opt-in shared-scope model**. That model is gone. Namespaces
now merge through an always-on **ancestor cascade**: a recall in `acme/phoenix/api`
already reads `acme/phoenix` and `acme`, because interior namespaces _are_ the
shared layer. See [scopes.md](../scopes.md), and
[scopes.md#knobs](../scopes.md#knobs) for the replacement knobs (the boot-refusal
message points there too).

The refusal is deliberate. Both variables expressed "these agents should also see
this shared pool". Silently ignoring them would drop that expectation with no
error, and your agents would quietly stop seeing memories they used to see. So
the boot fails instead.

### Runbook

**1. Back up the store.** Everything below moves and tombstones data.

```sh
# sqlite
cp /data/memini.db /data/memini.db.bak

# postgres
pg_dump "$MEMINI_POSTGRES_DSN" > memini-backup.sql
```

**2. Dry-run the migration.** `memini migrate scopes` folds every namespace
literally named `<prefix>/_shared` into `<prefix>`, moves link endpoints along
with it, then runs a dedup pass against the target to collapse facts the merge
just duplicated. It **defaults to a dry-run report**; there is no `--dry-run`
flag, only a `--yes` to apply.

```sh
memini migrate scopes
```

Read the report. It lists each `FROM → TO` merge with the number of memories
moved and the dedup clusters it would collapse. A bare `_shared` (no prefix) has
no parent to merge into and is left alone, reported as a note.

**3. Apply it.**

```sh
memini migrate scopes --yes
```

The apply path needs a working embeddings endpoint (`MEMINI_EMBED_BASE_URL`) up
front, because the post-merge dedup pass embeds as it goes. The command fails
before touching anything rather than moving data and erroring mid-dedup. It is
idempotent: once no `<prefix>/_shared` namespace holds memories, re-running finds
nothing to do.

**4. Adopt the old global namespace.** `migrate scopes` does _not_ rewrite
`MEMINI_GLOBAL_NAMESPACE` for you; it prints what to do and leaves the decision
to you, because the right answer depends on who reads that pool. Say your old
global was `shared/golang`:

- **Single operator.** Set `MEMINI_HOME=shared/golang` on the clients that read
  it. The home leg is merged read-only (durable tiers only) into every recall.

  ```sh
  export MEMINI_HOME=shared/golang
  ```

- **A team.** Add a read link per namespace that needs it. Links are stored in
  the store, so every client of that namespace inherits them with no client-side
  config.

  ```sh
  memini link add acme/phoenix shared/golang
  memini link add personal/kit  shared/golang
  ```

**5. Verify.**

```sh
memini doctor          # namespace mismatches and store health
memini namespace list  # namespaces and their memory counts
memini link ls         # every read link in the store
```

**6. Unset the variable** and restart.

```sh
unset MEMINI_GLOBAL_NAMESPACE MEMINI_TENANT_SHARED
```

### There is no deadlock

Worth saying explicitly, because it looks like there should be one: the fatal
check is **not** in ordinary configuration parsing. The Rust CLI checks
`fatal_deprecated_vars()` before starting either the HTTP or stdio MCP server.

`memini migrate scopes` separately reads
`MEMINI_GLOBAL_NAMESPACE` so it can print the adoption instructions in step 4.
If the refusal lived in `Load()`, the one command that fixes the problem could
never run while the variable that triggers it was set.

So: **you can run `memini migrate scopes` with the variable still set.** In fact
you should, so it can tell you what your old global was. Every one-shot CLI
command (`migrate`, `doctor`, `reembed`, `namespace`, `link`, `export`) is
unaffected. Only booting a long-running server (REST or MCP) refuses.

## The other removed variables

Nineteen more variables were removed without being fatal. They are **ignored**,
and each one logs a warning at startup:

```
WARN deprecated configuration detail="MEMINI_FUSION_ALPHA is removed and ignored; ..."
```

That warning is the only signal. If you tuned one of these and never read your
startup logs, your tuning stopped applying and retrieval behaviour changed under
you. Grep your environment, your Compose file, and your Helm values for all of
them.

### Renamed or collapsed

The three write-dedup score thresholds were three knobs for one decision. They
collapsed into a single score plus an action:

| Removed                           | Replacement                                                                    |
| --------------------------------- | ------------------------------------------------------------------------------ |
| `MEMINI_WRITE_DEDUP_MIN_SCORE`    | `MEMINI_WRITE_DEDUP_SCORE` with `MEMINI_WRITE_DEDUP_ACTION=coalesce`           |
| `MEMINI_MERGE_HINT_MIN_SCORE`     | `MEMINI_WRITE_DEDUP_SCORE` with `MEMINI_WRITE_DEDUP_ACTION=hint` (the default) |
| `MEMINI_AUTO_SUPERSEDE_MIN_SCORE` | `MEMINI_WRITE_DEDUP_SCORE` with `MEMINI_WRITE_DEDUP_ACTION=supersede`          |

One threshold now decides _whether_ a write is a near-duplicate;
`MEMINI_WRITE_DEDUP_ACTION` decides what happens when it is (`hint`, `coalesce`,
`supersede`, or `off`).

### Now a fixed internal value

These are no longer configurable. The value in parentheses is what the code now
uses unconditionally.

| Removed                            | Now fixed at                                                                                    |
| ---------------------------------- | ----------------------------------------------------------------------------------------------- |
| `MEMINI_FUSION_ALPHA`              | `0.5` (a baked retrieval default; tune via the benchmark harness, not env)                      |
| `MEMINI_TEMPORAL_BOOST`            | `0.40`                                                                                          |
| `MEMINI_RECALL_MIN_SEMANTIC_SCORE` | `0` (off)                                                                                       |
| `MEMINI_DEDUP_NEIGHBOURS`          | `20`                                                                                            |
| `MEMINI_DEDUP_MIN_CLUSTER_SIZE`    | `2`                                                                                             |
| `MEMINI_EMBED_MAX_ITEM_CHARS`      | `8000` (batch-char budgets stay configurable)                                                   |
| `MEMINI_RERANK_MAX_DOC_CHARS`      | `2048` (`MEMINI_RERANK_MAX_BATCH_CHARS` remains configurable)                                   |
| `MEMINI_CONSOLIDATE_QUEUE_CAP`     | `1024`                                                                                          |
| `MEMINI_NAMESPACE_HEADER`          | `X-Memini-Namespace` (the header name is fixed; clients and plugins all send this exact header) |

The three retrieval ones are the ones to care about. If you had moved
`MEMINI_FUSION_ALPHA` or `MEMINI_TEMPORAL_BOOST` off their defaults, your ranking
has changed. There is no env knob to put it back; the way to influence these now
is the benchmark harness under [`bench/`](../../bench).

### Now always on

Behaviour that used to be opt-in (or opt-out) is now unconditional. Setting these
does nothing.

| Removed                          | Now                                                                                         |
| -------------------------------- | ------------------------------------------------------------------------------------------- |
| `MEMINI_REDACT_SECRETS`          | Secret redaction is always on.                                                              |
| `MEMINI_WRITE_DEDUP_FINGERPRINT` | Exact-restatement dedup is always on.                                                       |
| `MEMINI_REINFORCE_SKIP_MARKERS`  | Always on.                                                                                  |
| `MEMINI_DISTILL_ON_WRITE`        | Write-time fact building is automatic (LLM when configured, heuristic extractor otherwise). |
| `MEMINI_EXTRACT_ON_WRITE`        | Same as above.                                                                              |
| `MEMINI_DISTILL_DROP_NO_FACT`    | Removed; episodic captures are always kept.                                                 |
| `MEMINI_QUARANTINE_GARBLED`      | Removed; garbled-content downranking is no longer configurable.                             |

If you had _disabled_ one of these (say `MEMINI_REDACT_SECRETS=false`), it is now
on and you cannot turn it off.

The full generated table lives in
[configuration.md](../reference/configuration.md#removed-settings), which is
generated from the code and is always current.

## Behaviour changes that are not variables

Not every breaking change shows up as a removed variable. These change what the
server does without changing what you configure.

### Durable memories now demote after 7 days

`MEMINI_DEMOTE_AFTER` shipped opt-in, defaulting to `0` (disabled). **Its default
is now `168h` (7 days).** Older documentation, including the README, said `0`.
It does not.

A durable (semantic or procedural) memory is demoted back to the episodic tier,
inheriting the episodic TTL, when _all_ of these hold:

- it has never been recalled (`AccessCount == 0`),
- its importance is below `0.5`,
- its effective confidence is below the demotion floor (uncorroborated),
- it is not pinned,
- it was last updated more than `MEMINI_DEMOTE_AFTER` ago.

The intent is that low-quality durable debris (a bulk import, mostly) ages out on
its own, while anything an agent actually recalled, marked important, or
corroborated is kept. But if you were relying on durable meaning _permanent_,
that changed. A never-recalled fact written 8 days ago is now episodic and on a
30-day clock.

Set `MEMINI_DEMOTE_AFTER=0` to restore the old behaviour, or raise it (for
example `1440h`, 60 days) if you want the sweep but on a longer horizon.
