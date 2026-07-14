# Recipes

Five guides, worked end to end. Each one is a real, complete configuration
rather than a menu of options: pick the closest, get it running, then diverge.

| Recipe                                              | Shape                                                                                                          |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| [Solo laptop](solo-laptop.md)                       | One developer, one machine. SQLite, a local embeddings endpoint, no LLM.                                       |
| [Homelab team](homelab-team.md)                     | Three to five developers, self-hosted. Postgres and VectorChord, remote MCP over HTTP, a named key per person. |
| [Access control](access-control.md)                 | A team's keys end to end: break-glass env key, one admin key per human, non-admin keys per agent/CI, rotation. |
| [Tuning recall](tuning-recall.md)                   | Recall is bad and you want to know which knob to turn. Symptom, cause, setting.                                |
| [Multi-agent namespaces](multi-agent-namespaces.md) | Several agents on one memini, sharing project knowledge without stepping on each other.                        |

Every setting these guides mention is documented in full in the
[configuration reference](../reference/configuration.md), which is generated from
the code. The guides tell you which settings matter together and why; the
reference is the authority on what each one does.

Background reading, in the order it tends to become relevant:
[tiers](../tiers.md) (how durable a memory is), [scopes](../scopes.md) (which
namespaces a read and a write touch), [API keys](../api-keys.md) (who the caller
is), [categories](../categories.md) (what a memory is about).
