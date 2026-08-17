# How memini works

The machine on one page. Each doc in this directory explains one mechanism at
operator depth and ends with a source map for contributors; this page is the
index and the thirty-second architecture tour.

## The machine

**Write pipeline.** Every write — an explicit `memory_remember`, a captured
turn, a session digest — runs the same gauntlet: validate, auto-classify the
tier (pure heuristics, no LLM), resolve visibility to exactly one namespace,
scrub and sanitize, pass a value gate (junk is accepted but not stored),
dedup by content fingerprint (an exact repeat reinforces the original instead
of writing a copy), embed, dedup again by vector similarity (a near-duplicate
supersedes, hints, or coalesces), then store. If the embedder is slow or
absent the row lands vectorless and a background job repairs it later — a
write never fails because embeddings did. Details: [write-path](./write-path.md).

**Read pipeline.** A recall builds a read set from its scope — the primary
namespace first, then ancestors nearest-first, the caller's personal
namespace, and any linked namespaces, with the non-primary legs restricted to
durable tiers. Each leg runs vector and keyword search in parallel; results
are fused per-namespace, then across namespaces, ranked (relevance-dominated,
with a durable-tier reserve and a turn-echo guard), and cut to the token
budget. Recalling a memory reinforces it; briefings deliberately never do.
Details: [recall](./recall.md).

**Namespaces.** Never created, only materialized: a namespace exists exactly
as long as some memory row carries its string. The client proposes a name
(pin, env override, or derivation from the git remote); the server
arbitrates. Details: [namespaces](./namespaces.md).

**Background maintenance.** Four loops run behind every server:

| Loop                  | Default cadence                  | Job                                                                                               |
| --------------------- | -------------------------------- | ------------------------------------------------------------------------------------------------- |
| Sweeper               | hourly                           | Purge expired memories, evict over-cap short-term rows, collect tombstones, demote stale durables |
| Promoter              | daily                            | Distill repeatedly-accessed short-term memories into durable ones                                 |
| Dedup job             | daily                            | Merge near-duplicate rows that slipped past write-time dedup                                      |
| Importance assessment | hourly (needs an LLM configured) | Backfill importance scores on durable rows that lack one                                          |

Confidence, promotion, demotion, supersession, and time-travel are covered in
[lifecycle](./lifecycle.md).

**The plugin.** Seven Claude Code hooks plus an MCP server expose the whole
thing to an agent: a briefing injected at session start, per-prompt and
per-file recall, per-turn capture, and the nine `memory_*` tools
([reference](../reference/mcp-tools.md)). Details: [plugin](./plugin.md).

## One memory, end to end

You tell the agent "we're switching the job queue to Postgres — Redis kept
dropping jobs." The agent calls `memory_remember` with that fact. The MCP
call carries the namespace header the plugin's headers helper resolved for
this repo — say `acme/phoenix`, derived from the git remote during the
session-start handshake — and the server confirms that arbitration (a pin or
key default would have won instead).

The write pipeline takes over: the content classifies as a decision, so the
tier lands as `semantic` — durable — with a classification stamp in metadata.
It passes the value gate, matches no existing fingerprint, embeds, matches no
near-duplicate vector, and becomes a row in `acme/phoenix` with a seed
confidence and a validity interval starting now.

Tomorrow you open a new session in the same repo. The session-start hook runs
its handshake, resolves `acme/phoenix` again, and fetches the briefing — and
there it is, under "Decisions & conventions", injected into the agent's
context before you type a word. Every recall that surfaces it slides its
retention window and grows its confidence; if you later reverse the decision,
the correcting write supersedes it and history keeps both. That full arc is
walked with real payloads in the [examples](../examples/README.md).

## Where to look

| Question                                                 | Doc                                            |
| -------------------------------------------------------- | ---------------------------------------------- |
| How does it pick a tier for my memory?                   | [write-path](./write-path.md)                  |
| Why wasn't my write stored (`stored: false`)?            | [write-path](./write-path.md)                  |
| When do memories get pulled into context?                | [recall](./recall.md)                          |
| Why did recall search those namespaces?                  | [recall](./recall.md), [scoping](../scopes.md) |
| Where do memories live, and when is a namespace created? | [namespaces](./namespaces.md)                  |
| How do memories age, decay, and get promoted?            | [lifecycle](./lifecycle.md)                    |
| What happens when a fact is corrected?                   | [lifecycle](./lifecycle.md)                    |
| What do the hooks inject, and when?                      | [plugin](./plugin.md)                          |
| What do the tiers mean?                                  | [tiers](../tiers.md)                           |
| Who can read or write across namespaces?                 | [scoping](../scopes.md)                        |
| Show me a worked scenario                                | [examples](../examples/README.md)              |

## Source map

Each doc here ends with its own source map naming the files and functions
behind it — start from the doc that answers your question. The broad strokes:
the write and read pipelines live in `internal/service/`, ranking in
`internal/search/`, namespace resolution in `internal/nsresolve/`, the
background loops in `internal/maintenance/`, and the plugin in
`plugin/scripts/` over the shared client in `packages/memini-client/src/`.
