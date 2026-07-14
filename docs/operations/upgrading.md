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
check is **not** in `config.Load()`. It lives in the two server-boot paths,
`runServer` and `runMCP` (`cmd/memini/root.go`, `cmd/memini/mcp.go`), which call
`config.FatalDeprecatedVars()` before loading config.

`memini migrate scopes` also calls `config.Load()`, and it separately reads
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

## The admin UI now requires signing in

Before this release, when `MEMINI_API_KEY` was set the server **injected** that
key into the UI shell (a `<meta name="memini-token">` tag) so the same-origin
browser authenticated with no interaction. That injection is **removed**. The
served shell is now credential-free (it never contains `MEMINI_API_KEY`), and the
UI is a real login: you paste an API key once per browser, it is verified against
`GET /v1/self`, and it persists in that browser's `localStorage`.

Two things drove the change. Serving the admin key inside HTML handed to anyone
who could so much as `GET /` was the wrong default. And admin is now a **per-key
attribute** (see [api-keys.md](../api-keys.md#the-admin-attribute)), so the
browser can hold a per-person, non-break-glass credential rather than the one
shared admin key.

**What this costs you:** every browser signs in once after the upgrade. There is
no config change to make it work; there is a paste. Below is what "sign in once"
means per deployment.

### Same-origin homelab (UI on the main port)

Nothing to configure. The first time each person opens the UI after upgrading,
they paste a key at the sign-in screen. If you were relying on the auto-embedded
admin key, this is the moment to stop sharing it: mint one **named admin key per
person** (`memini key add <name> --admin`, or the UI's create form) and hand each
person their own, keeping `MEMINI_API_KEY` as break-glass. The
[access control guide](../guides/access-control.md) walks that end to end. If you
would rather keep it simple, pasting the existing `MEMINI_API_KEY` once per
browser also works.

### Remote UI (the UI points at a remote memini)

Same paste-once, plus the base URL. Previously a remote target was configured in
Settings against an embedded token; now you set the API base URL in the sign-in
screen's collapsed **Advanced** section (Settings is unreachable until you are
signed in) and paste the key there.

### Compose

No config change. If your runbook said "open the UI and it just works", update it
to "open the UI and sign in with a key". Mounting a `MEMINI_API_KEYS_FILE` (see
the commented example in [`compose.yaml`](../../compose.yaml)) lets each person
sign in with their own named key rather than a shared token.

### Helm

Structurally unchanged: the chart already gives the UI its own listener
(`MEMINI_UI_ADDR`) and recommends routing it only to an internal gateway. Keep
that. Each admin signs in once per browser with their own named admin key; mint
them with `memini key add --admin` or a `MEMINI_API_KEYS_FILE` mounted from a
Secret (there is a commented mounting example in
[`charts/memini/values.yaml`](../../charts/memini/values.yaml) and a full
walkthrough in the [access control guide](../guides/access-control.md)).

### The 403 string changed (out-of-tree tooling)

The admin-gate rejection changed text, on purpose, so any tooling that
string-matched the old wording fails loudly instead of silently mis-reading a
response:

| Before               | After                                                                                                             |
| -------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `admin key required` | `admin credential required: this endpoint needs the admin env key (MEMINI_API_KEY) or an API key with admin=true` |

If you have a script or integration that matched `"admin key required"` on a
`403` from `/v1/keys` or `/v1/settings/defaults`, update it. Better, match the
`403` status rather than the string. The UI itself no longer probes this at all;
it reads `identity.admin` from `GET /v1/self`.

### New: admin is a per-key attribute

The flip side of the removal is a new capability. `MEMINI_API_KEY` is no longer
the only credential that can manage keys: any named or file key can carry
`admin: true` (`--admin` on the CLI, `admin: true` in the keys file, the admin
checkbox in the UI). The env key becomes **break-glass**, and named admins do the
day-to-day. It also gates two operator surfaces named admins do **not** unlock,
`/metrics` and verbose `/healthz`; see
[api-keys.md](../api-keys.md#the-metricshealthz-asymmetry).

## Client-side: the config-handshake redesign

Everything above is the server. This section is the client — the Claude Code
plugin, opencode, hermes, Open WebUI — which inverted from deriving its own
configuration to asking the server for it on every connect. See
[docs/reference/env-vars.md](../reference/env-vars.md) for the full model;
this is what changes for an existing install.

### Removed client variables

Four client-side variables are retired. None are fatal — each is silently
ignored everywhere, except `SessionStart`, which prints one combined stderr
line if any is set:

```
[memini] ignored removed env vars: MEMINI_URL, MEMINI_NAMESPACE_SCOPE (see docs/reference/env-vars.md)
```

| Removed                  | Now                                                                                                  |
| ------------------------ | ---------------------------------------------------------------------------------------------------- |
| `MEMINI_URL`             | Removed alias. Set `MEMINI_BASE_URL`.                                                                |
| `MEMINI_TOKEN`           | Removed alias. Set `MEMINI_API_KEY`.                                                                 |
| `MEMINI_MCP_URL`         | Removed. The MCP endpoint is always `${MEMINI_BASE_URL}/mcp`.                                        |
| `MEMINI_NAMESPACE_SCOPE` | Moved server-side, as the `namespace_scope` behavior setting — no client env override exists for it. |

Grep your shell profile, your MCP client config, and any wrapper scripts for
these four. Nothing crashes if you don't — the warning is the only signal,
same shape as the server's own removed-variable warnings above.

### Every remaining client env var is now a debug override, not the source of truth

Before this redesign, a client-side `MEMINI_*` variable like
`MEMINI_SESSION_DIGEST` or `MEMINI_INJECT_BRIEFING_FACTS` was read once,
locally, and that was the whole story. Now every one of those knobs is
**server data**, resolved fresh on each handshake (built-in default ← global
defaults ← per-key settings). The env var still works exactly as before — it
still wins if set — but it is now explicitly a **local debug override**: it
overrides what the server resolved for this one client, and does not change
what any other client sees or what is stored on the server. If you want a
setting to apply for everyone, set it once server-side
(`PUT /v1/settings/defaults`, `MEMINI_CLIENT_DEFAULTS`, or per-key via
`PUT /v1/self/settings`) instead of exporting it on every machine.

### Pins replace overrides.json

`~/.config/memini/overrides.json` — the per-machine namespace override file —
is retired in favor of server-side **pins** (`/v1/pins`,
`/memini:namespace <ns>`). A pin is the same idea (pin one project to one
namespace) but lives on the server, so it follows you across machines instead
of being stuck on whichever one you set it on.

Migration is automatic and needs nothing from you in the common case: the
first time a project's handshake succeeds and reports no pin, `SessionStart`
reads a matching `overrides.json` entry (read-only — it is never written or
cleared) and `PUT`s it to `/v1/pins`, printing one line on success:

```
[memini] migrated your local namespace override for this project to a server pin
```

The pin that creates is exactly what makes this idempotent: the next
handshake reports `namespace_source: "pin"`, so the migration never fires
twice for the same project. A failed `PUT` (server hiccup) is fail-soft — no
crash, no error surfaced beyond a stderr note — and is retried the same way
next session.

To migrate every project on a machine at once (e.g. right after upgrading,
before you have opened each one), run:

```
/memini:namespace --migrate
```

It prints a `key -> namespace -> status` table (`migrated` / `already-pinned`
/ `failed`) and, only on full success, renames the file to
`overrides.json.migrated`. A partial failure leaves the file in place so a
re-run retries just what did not land. It also checks
`~/.config/memini/config.json` for the older `tenantRoots`/`template` tenancy
config a couple of integrations still read — that one cannot be
auto-translated, so it is printed with instructions to recreate it by hand as
a `namespace_prefix` on the relevant API keys, or as per-project pins.

### The one capability regression: no offline pinning

Setting or clearing a pin needs the server reachable, because the pin itself
is server-side state. The old `overrides.json` was a local file, so it worked
offline. If you need to pin a namespace with no server available, the escape
hatch is the same variable that has always worked degraded:

```sh
export MEMINI_NAMESPACE=<namespace>
```

Degraded resolution honors it exactly as before. It just does not follow you
to another machine the way a real pin does — set the pin for real
(`/memini:namespace <ns>`) once the server is back.

### Windows: no `/proc`

The MCP `headersHelper` recovers the project directory by walking the process
tree (`/proc` on Linux, `lsof` on macOS) so it can resolve a namespace before
any hook has run. Windows has neither. On the **very first** MCP connect,
before any hook has populated the per-session cache the helper reads on every
connect after that, this means the helper emits **auth-only headers** — the
bearer token, no `X-Memini-Namespace` — and the server applies the
authenticating key's `default_namespace` (or the server default) for that one
connection. Once a hook has fired once (which happens on ordinary use, well
before most agents make their first tool call), the cache exists and every
later connect resolves the real per-project namespace normally. This is a
first-connect gap on Windows specifically, not a persistent behavior
difference.

### `MEMINI_CLIENT_DEFAULTS` for GitOps / Helm

If you manage global behavior defaults declaratively rather than through the
admin UI or `PUT /v1/settings/defaults`, set `MEMINI_CLIENT_DEFAULTS` as a
**server** env var — one JSON-encoded `ClientSettings` object. It becomes the
global-defaults layer and locks it read-only (`PUT /v1/settings/defaults`
returns 409 while it is set, so nobody can drift it back out from under your
GitOps repo). In `charts/memini/values.yaml`:

```yaml
env:
  MEMINI_CLIENT_DEFAULTS: '{"capture_turns":false,"recall_limit":5}'
```

See [`MEMINI_CLIENT_DEFAULTS`](../reference/configuration.md#memini_client_defaults)
for the full validation rules (fail-loud at boot, same as
`MEMINI_API_KEYS_FILE`) and the [homelab guide](../guides/homelab-team.md) for
more on managing a fleet this way.
