# Multi-agent namespaces

Several agents, one memini. A coding agent, a reviewer subagent, a docs bot, a
nightly triage job: all working on the same project, all writing memories, and
none of them should have to read the others' scratch notes to find a shared
project fact.

The mechanism is the namespace tree plus the ancestor cascade, and it needs less
configuration than you would expect. [scopes.md](../scopes.md) is the full model;
this guide is how to apply it.

## Give each agent its own segment

```sh
# client (your agent host)
export MEMINI_AGENT=reviewer
```

`MEMINI_AGENT` nests the resolved namespace under a per-agent segment:
`acme/phoenix` becomes `acme/phoenix/reviewer`. It is resolved on the **client**
side (the plugin reads it and sends the nested namespace as `X-Memini-Namespace`
on every call), so you set it in the environment each agent runs in, not on the
memini server. Set it per agent and the tree lays itself out:

```
acme                          <- org-wide durable facts
acme/phoenix                  <- shared project knowledge
acme/phoenix/reviewer         <- the reviewer's own memories
acme/phoenix/docs             <- the docs bot's own memories
personal/kit                  <- your home namespace, if you have one
```

If you are running `memini mcp` over stdio without the plugin, no client is
resolving anything for you: set `MEMINI_NAMESPACE=acme/phoenix/reviewer`
explicitly, since the stdio server's own header-less fallback does not apply the
agent segment.

## Reads see the parent for free

This is the part that makes the whole design work. A recall in
`acme/phoenix/reviewer` does not just search `acme/phoenix/reviewer`. Under the
default `scope:"full"`, it also reads:

| namespace               | origin   | tiers searched                      |
| ----------------------- | -------- | ----------------------------------- |
| `acme/phoenix/reviewer` | primary  | all                                 |
| `acme/phoenix`          | ancestor | durable only (semantic, procedural) |
| `acme`                  | ancestor | durable only                        |
| `personal/kit`          | home     | durable only                        |
| any linked namespace    | link     | durable only                        |

So the reviewer inherits every durable fact the main coding agent established
about the project, with no duplication, no copying, and no configuration. Nobody
has to decide up front which facts are "shared", because sharing is the shape of
the tree rather than a flag on a memory.

The tier restriction is the isolation valve. Only `semantic` and `procedural`
cross a namespace boundary. The reviewer's raw session transcript stays exactly
where it was written, so a nested agent cannot flood the project namespace with
its own chatter simply by existing.

Ancestors are searched nearest-first, and that ordering is load-bearing: at equal
relevance a memory in `acme/phoenix` outranks one in `acme`. Specific beats
general.

Every result carries a `from` field naming where it came from (absent for the
agent's own namespace, the bare namespace for an ancestor or home hit,
`link:<ns>` for a linked one). Agents are expected to learn the topology by
reading `from` and the briefing's `Scope:` line, never by constructing a namespace
path. That is why the MCP tools do not accept a raw namespace at all.

## Reading wider or narrower: `scope`

Recall, briefing, and answer all take a `scope`:

- **`project`**: the agent's own namespace only. No ancestors, no home, no links.
  Use it when you specifically want to know what _this_ agent has seen, with no
  inherited context bleeding in.
- **`full`** (default): the cascade above.
- **`everywhere`**: the cascade plus the agent's own subtree, its descendant
  namespaces. Note that subtree members are treated as an extension of the primary
  namespace rather than a cascade leg, so they are searched at the caller's full
  tier filter, not clamped to durable. It is an explicit downward reach, never on
  by default. Run it from `acme/phoenix` and you see what every agent nested under
  the project has been up to, chatter included.

## Writing up the tree: `visibility`

A write always lands in exactly one namespace. By default that is the agent's own.
`visibility` moves it:

- **`project`** (default): stays put.
- **`personal`**: routes to the caller's home namespace (`MEMINI_HOME` /
  `X-Memini-Home`). Errors if no home is configured.
- **an ancestor's name**: `"acme/phoenix"` or the unambiguous last segment
  `"acme"`. Routes the write up to that ancestor, where every sibling agent will
  read it.

So the reviewer, having discovered a genuine project convention, writes it with
`visibility:"acme/phoenix"` and every other agent on the project picks it up on
its next recall. An invalid or ambiguous name errors with the valid chain
enumerated, so an agent can learn the topology from the error instead of guessing.

**The tier clamp is the guardrail.** Episodic and working writes **always** land
in the primary namespace, regardless of what `visibility` says. Only durable tiers
travel. This is silent and deliberate, and it is checked before `visibility` is
even validated. A session digest cannot pollute a shared ancestor, even if an
agent asks it to, and a clamped write does not require `MEMINI_HOME` to be set
just because it happened to say `personal`.

That single rule is what makes it safe to let an LLM choose `visibility` at all.
The worst case is a durable fact in the wrong place, which is a `memory_update`
away from fixed, rather than a thousand transcript fragments in the org namespace.

## Sideways sharing: links

The cascade only goes up. Two agents in unrelated parts of the tree that share a
convention (say a language style guide) need a **link**, which is explicit: one
command, one row.

```sh
memini link add acme/phoenix shared/golang --tiers semantic,procedural --note "shared Go conventions"
memini link ls acme/phoenix
```

`acme/phoenix` now reads `shared/golang`'s durable memories, and so does every
agent nested under it. Links are one hop only, never transitive, so provenance
stays answerable with a single lookup. They are durable-tier by default and can
narrow further, never widen.

## The escape hatch

```sh
# server (the memini process)
export MEMINI_CASCADE=false
```

This turns the ancestor, home, and link cascade off server-wide. A read then sees
only the request namespace (plus its subtree when asked), and `scope:"full"` stops
adding the cascade legs. It exists for operators upgrading from the pre-cascade
model who want the old isolation without setting `scope:"project"` on every call.
Per-call `scope` and explicit `namespaces` still work either way.

Reach for it if inherited context is actively hurting, which is rare. The usual
fix for "the reviewer sees too much" is a narrower `scope` on the calls that
matter, not switching off inheritance for the whole deployment.

## Checking what an agent actually sees

```sh
MEMINI_AGENT=reviewer memini doctor
```

`doctor` prints the effective read set for the namespace it resolves, one row per
namespace with its origin and the tiers it contributes. If an agent is not seeing
project knowledge, that table tells you whether the ancestor leg is present, and
therefore whether the problem is the tree or the ranking. `GET
/v1/namespaces/read-set` answers the same question against a running server.
