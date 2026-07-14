# memini documentation

Start with the recipe closest to what you are doing, then reach for the
reference when you need a specific setting.

## I want to...

|                                                |                                                                                             |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------- |
| Run it on my laptop                            | [Solo laptop](guides/solo-laptop.md)                                                        |
| Wire it into my coding agent                   | [Integrations](../integrations/README.md)                                                   |
| Self-host it for my team                       | [Homelab and team](guides/homelab-team.md)                                                  |
| **Fix bad recall**                             | [Tuning recall](guides/tuning-recall.md)                                                    |
| Lay out namespaces for several agents          | [Multi-agent namespaces](guides/multi-agent-namespaces.md)                                  |
| **Upgrade, and my server will not start**      | [Upgrading](operations/upgrading.md)                                                        |
| Look up a setting                              | [Configuration](reference/configuration.md)                                                 |
| Look up an MCP tool, CLI command or endpoint   | [MCP tools](reference/mcp-tools.md), [CLI](reference/cli.md), [REST](reference/rest-api.md) |
| Understand how memini decides what to remember | [Tiers](tiers.md), [Categories](categories.md)                                              |
| Understand which namespaces a search reads     | [Scopes](scopes.md)                                                                         |
| Check what a word means exactly                | [Glossary](glossary.md)                                                                     |
| Give each person their own credential          | [API keys](api-keys.md)                                                                     |
| See the retrieval numbers                      | [Benchmarks](../bench/README.md)                                                            |

## Concepts

Read these when you want to know why memini behaves the way it does.

- [**Glossary**](glossary.md). One meaning per word: namespace, project, pin,
  scope, and the overloads they replace.
- [**Tiers**](tiers.md). Every memory is `working`, `episodic`, `semantic` or
  `procedural`. The tier decides how long it survives and whether it is allowed
  to cross a namespace boundary.
- [**Scopes**](scopes.md). A write lands in exactly one namespace. A read can
  see more than one. This explains how the read set is composed, and it is the
  page to read if recall is returning too much or too little.
- [**Categories**](categories.md). An orthogonal topic axis, for filtering by
  what a memory is about rather than how durable it is.
- [**API keys**](api-keys.md). Keys are identity, not authorization. Each key can
  carry its own home and default namespace.

## Guides

Worked setups. Pick the closest one and diverge from it.

- [Solo laptop](guides/solo-laptop.md). SQLite, a local embeddings endpoint, no LLM.
- [Homelab and team](guides/homelab-team.md). Postgres, named keys, a shared server.
- [Tuning recall](guides/tuning-recall.md). Symptom, cause, and the setting that fixes it.
- [Multi-agent namespaces](guides/multi-agent-namespaces.md). Several agents, one store.

## Reference

Generated from the code, so it cannot drift.

- [Configuration](reference/configuration.md). Every server setting, its default,
  and the removed ones.
- [MCP tools](reference/mcp-tools.md). The nine tools an agent sees.
- [CLI](reference/cli.md). Every command the binary accepts.
- [REST API](reference/rest-api.md). Every endpoint, indexed from the OpenAPI spec.
- [Server vs client variables](reference/env-vars.md). Four names mean different
  things on each side. Read this if a setting seems to have no effect.

## Operations

- [Deployment](operations/deployment.md). Docker, Compose, Kubernetes.
- [Upgrading](operations/upgrading.md). Removed settings, and the two that refuse
  the boot.
- [Web UI](operations/web-ui.md). What each view is for, and the one security
  caveat that matters.
- [Import and export](operations/import-export.md). Moving memories in, out, and
  between embedding models.
