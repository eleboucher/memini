# Namespace Cascade: scope redesign

**Date:** 2026-07-10
**Status:** Draft for review
**Compat posture:** breaking (pre-1.0), with `memini migrate scopes` and doctor support

## Problem

Namespaces are n-depth slash-paths, but the scope semantics bolted onto them are
depth-1 and fragmented across four mechanisms, each with its own configuration
surface and tier rules:

| Mechanism | Configured via | Tiers |
|---|---|---|
| `MEMINI_GLOBAL_NAMESPACE` | server env var | durable only |
| tenant `_shared` merge | server env flag + naming convention in data | durable only |
| `scope=subtree` | per-call arg | request's filter |
| explicit `namespaces: [...]` | per-call arg | request's filter |

`tenantSharedNamespace` cuts at the **first** path segment only:
`work/clientA/projectX` merges `work/_shared` but never `work/clientA/_shared`.
Global and tenant-shared are the same idea at different tree depths, implemented
as two special cases.

Evidence from a live shared instance (memini.perfectra1n.com, 2026-07-10):

1. **Parent-as-shared is the organic convention.** The `atvik` tenant root holds
   org-wide facts (business pivot, GitHub org map, source-of-truth pointers) —
   users write shared context to the interior node directly, not to a `_shared`
   sibling. The `_shared` convention fights real usage.
2. **n-depth nesting is real.** `atvik/tyrfing/sdk` under `atvik/tyrfing` under
   `atvik` — depth-1 tenant sharing already fails it.
3. **The person dimension is missing.** One instance mixes personal trees
   (`homelab`, `nixos-configs`) with a team tree (`atvik/...`, multiple humans).
   An instance-global namespace makes "always remember this" cross people.
4. **Write-side placement fails visibly.** Per-person episodic session digests
   pollute the shared `atvik` root, and the newest memory in `atvik` is a stored
   procedural memory teaching agents where to write — the data layer documenting
   a missing parameter.

## Goals

- One scope rule, n-depth, zero server-side scope configuration.
- The LLM never constructs or sees raw namespace paths; it makes semantic
  choices (where should this be known?) and learns the topology from provenance.
- Per-person "everywhere" on shared instances.
- Sharing effort proportional to how unusual the sharing pattern is:
  universal → automatic; occasional → one command; rare → per-call.

## Non-goals

- **Access control.** Namespaces remain scoping, not authz: one trusted API key
  per instance (or per team). The model is designed so a permission boundary
  *could* later attach at a subtree root, but no enforcement ships here.
- **Transitive link traversal.** Links are one hop, permanently (provenance must
  stay answerable in one lookup).

## Core model

A **write** lands in exactly one namespace (unchanged invariant).

A **read** in namespace `N` resolves its read-set from, in order:

1. **Primary**: `N`, the request's tier filter. Always first, never clamped.
2. **Ancestors** (computed, nearest first): every path prefix of `N`, durable
   tiers only. `atvik/tyrfing/sdk` → `atvik/tyrfing`, `atvik`. The `_shared`
   leaf convention is removed; interior nodes ARE the shared layer.
3. **Home** (from the `X-Memini-Home` header, when present): the caller's
   personal namespace, durable tiers only. Replaces `MEMINI_GLOBAL_NAMESPACE`
   with a per-person equivalent carried by client config.
4. **Links** (stored): rows in `namespace_links` — directional read-edges
   `(src_ns, dst_ns, tiers, note, created_at)`, default durable-only,
   overridable per link. One hop; no traversal from linked namespaces.

Per-call `namespaces: [...]` still replaces the entire default read-set
(explicit means explicit); `scope=subtree` survives as the per-call downward
escape hatch. The resolved set keeps the existing cap-and-clamp behavior with
primary/home protected, and nearest-first ancestor ordering feeds the existing
first-seen RRF tie-break so closer context outranks distant on equal scores.

Deleted: `MEMINI_GLOBAL_NAMESPACE`, `MEMINI_TENANT_SHARED`, the `_shared`
convention, and the special-cased merge legs in `resolveDefaultReadSet`.

### Isolation contract

| Relationship | What crosses | Effort |
|---|---|---|
| Within a namespace | everything, all tiers | — |
| Child → ancestor (up) | durable, read-only | zero (always on) |
| Ancestor → descendant (down) | briefing rollup only; recall via per-call `scope=subtree` | zero / per-call |
| Sibling ↔ sibling | nothing unless linked | one command |
| Caller → own home | durable, read-only | zero (client config, once) |
| Caller → another person's home | nothing | not expressible |
| Any write | never crosses; explicit `visibility` moves durable writes up | one param |

Tiers are the isolation valve: working/episodic memory is first-person and
situational and never crosses a namespace boundary in either direction.

## Write model

`memory_remember` (and `POST /v1/memories`) gains `visibility`:

- `"project"` (default) → primary namespace.
- `"personal"` → the caller's home namespace (error if no home configured,
  message explains `MEMINI_HOME`).
- `"<ancestor name>"` → that ancestor, matched against the primary's chain by
  full path or unambiguous last segment (`"atvik"` or `"atvik/tyrfing"` from
  `atvik/tyrfing/sdk`). An invalid or ambiguous name errors with the valid
  chain listed — the error teaches the topology.

**Tier clamp:** episodic and working writes always land in the primary
namespace regardless of `visibility`; only durable tiers travel up. This
mechanically prevents session-digest pollution of shared interior nodes.

Raw `namespace` stays on the REST API for scripts and the admin UI; it is
dropped from the MCP tool schema the LLM sees.

## Read/LLM surface

- `memory_recall` gains `scope`: `"project"` (primary only) | `"full"`
  (default: primary + ancestors + home + links) | `"everywhere"` (full +
  subtree).
- **Provenance on every result**: `from: project | ancestor:<ns> | personal |
  link:<ns>`, rendered compactly in MCP output (`(from: atvik)`). The LLM
  learns scope behavior by observing it, not from schema prose.
- **Briefing scope header**: one line showing the chain and merge counts, e.g.
  `Scope: atvik/tyrfing/sdk ← atvik/tyrfing(3) ← atvik(4) ← personal(2), +1 link`.
- **Briefing-only downward visibility**: at an interior node, briefing appends
  a compact child rollup (per child: name, memory count, pinned + most recent
  durable titles). Recall precision is untouched; orientation is not.

## API / CLI / config

- New: `GET/POST/DELETE /v1/links`; `memini link add|rm|ls`.
- New: `GET /v1/namespaces/{ns}/read-set` → resolved entries each tagged
  `origin: primary|ancestor|home|link|call` + tier restriction. Feeds doctor,
  the admin UI (render the tree + effective read-set), and debugging.
- New client env: `MEMINI_HOME` (recommended value `personal/<name>`), resolved
  by plugins exactly like `MEMINI_NAMESPACE` and sent as `X-Memini-Home`.
- `tenantRoots` config.json survives unchanged (path → prefix mapping only).
- `memini doctor` additions: warn when `MEMINI_NAMESPACE` is pinned globally
  (catch-all namespace trap); warn when `X-Memini-Home` is absent but
  `visibility:"personal"` was attempted recently; show the effective read-set.

## Migration — `memini migrate scopes`

1. Every `<t>/_shared` namespace: merged into `<t>` via the existing
   renamespace/move machinery (dedup gate already handles collisions).
2. If `MEMINI_GLOBAL_NAMESPACE` was set: print instructions to adopt it as a
   home namespace (single-operator instances: `MEMINI_HOME=<old global>`), or
   to `memini link` it where team-wide. No silent rewrite.
3. Server refuses to start with the deleted env vars set, with a message
   pointing at this command (fail loud, pre-1.0).
4. Existing default read behavior is a strict subset of the new cascade except
   the removed opt-out (`MEMINI_TENANT_SHARED=false`); release notes call this
   out.

## Testing

- Cascade resolution unit tests in the existing `readset_test.go` table style:
  depth 0/1/3, home present/absent, links, clamp interaction, ordering.
- Store conformance suite additions for `namespace_links` CRUD (both backends);
  Postgres integration job already exercises real VectorChord.
- E2E: plugin sends both headers → resolved read-set with origins asserted via
  `/v1/namespaces/{ns}/read-set`; visibility mapping including tier clamp and
  ambiguous-ancestor errors.
- Plugin JS tests for `MEMINI_HOME` resolution across Claude Code / Codex /
  opencode / OpenClaw.
- Bench: one LongMemEval-S run before/after — ancestor legs change RRF
  candidate pools, and ranking regressions won't surface in unit tests.
- Migration tests: `_shared` merge, env-var refusal, idempotent re-run.

## Future (explicitly out of scope)

- Reimplementing cascade legs as implicit links (unifying resolution fully)
  is compatible later without another breaking change.
- Attaching authz at a subtree root (per-tenant keys) fits the model when
  needed.
