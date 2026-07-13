# @memini/client

The shared client-side core for the config-handshake redesign: project facts,
the handshake wire client, what a caller should do with a handshake result,
behavioral-settings resolution with provenance, secret redaction, and how it
finds the project it is working in.

`@memini/namespace-resolver`, formerly a sibling package (a separate namespace
resolution chain for pi/openclaw's config-file/tenant-root feature), has been
deleted: both of its consumers moved onto this package's
gatherFacts/performHandshake/resolveNamespace instead, and its one
still-in-use helper (a small `{namespace}`/`{agent}` template substitution)
was inlined into openclaw, its last consumer.

## What it provides

| Module               | Purpose                                                                   |
| -------------------- | ------------------------------------------------------------------------- |
| `facts`              | `gatherFacts()`: the project facts sent as `POST /v1/handshake`'s body    |
| `handshake`          | `performHandshake()` plus the per-session handshake cache                 |
| `resolve`            | `resolveNamespace()`: what to do with a handshake result (or its absence) |
| `settings`           | `effectiveSetting()`/`BEHAVIOR_KNOBS`: behavioral knobs with provenance   |
| `override`           | Per-project namespace override — **read-only**; see below                 |
| `redact`             | Secret redaction, always on                                               |
| `namespace-validate` | Normalization, plus the header-safety rules                               |
| `session`            | Recover the project directory in processes the harness gives none         |

## Provenance is the feature

A settings dump that only prints values is nearly useless. The case this
matters for: `MEMINI_NAMESPACE` exported as a **fish universal variable**, set
once and forgotten, silently collapsing every repo on the machine into one
shared namespace. A list of values would show `namespace: default` and look
fine; only the provenance — _where_ that value came from — catches it.

`resolveNamespace(boot, facts, hs)` is what every caller composes its own
provenance report around: a successful handshake (`hs`) wins outright and is
never `degraded`; absent one, `boot.namespaceEnv` (`MEMINI_NAMESPACE`) is the
next fallback, then local git/cwd derivation — every non-handshake path comes
back `degraded: true`, because it is a guess the server hasn't confirmed. Each
integration's own `/memini:status` (or equivalent) renders this with the
knobs from `effectiveSetting`, so the report always reflects what that
specific harness actually does, not a shape this package imposes on all of
them.

This package's own namespace _override_ (below) predates the handshake and no
longer participates in this precedence at all — pins (server-side, via
`POST`/`DELETE /v1/pins`) replaced it as the thing a user sets deliberately.

## Namespace validation is stricter than the server's

The server accepts anything 1–256 bytes without a NUL. The client must be
stricter, because a namespace does not reach the server as a body field: it rides
on the `X-Memini-Namespace` **HTTP header**. A value containing CR or LF would
split that header and let a caller inject arbitrary ones, so those are rejected
rather than normalized away.

## Finding the project directory

Claude Code's MCP `headersHelper` sets `X-Memini-Namespace` for every MCP tool
call, and is handed almost nothing to work with. Measured in a live session:

```
cwd                : <plugin install root>
PWD                : <plugin install root>   <- rewritten; NOT the project
CLAUDE_PROJECT_DIR : unset
process.ppid       : the session's `claude` process, cwd = the project dir
```

Historically the helper fell back to a single global cache file that the
SessionStart hook wrote — which, with two sessions open in two repos, is
last-writer-wins. Both sessions' MCP calls then target one namespace while their
hooks write to another: the "writes land where recall doesn't look" failure
`memini doctor` exists to diagnose.

`resolveHarnessCwd()` fixes it by walking the process tree instead:

```
1. CLAUDE_PROJECT_DIR       if the harness ever provides it
2. the parent process's cwd  Linux /proc, macOS lsof — always fresh, and works
                             on the first connect before any hook has run
3. the session file          portable fallback (Windows), written by the hooks
                             under the same ppid both sides observe
```

Every step yields a **directory**, never a namespace. The caller re-resolves the
namespace from it on each use — which is what keeps an override authoritative,
where a cached namespace would go stale the moment one was set.

## Who consumes it, and who deliberately does not

- **pi and openclaw** import the TypeScript, inlined at build time by esbuild
  (`--alias:@memini/client=...`). They cannot take it as a normal dependency: both
  are `npm install`-ed by their hosts, and an unpublished workspace package would
  make them uninstallable — their own bundle tests enforce that. Both compose
  `gatherFacts`/`performHandshake`/`resolveNamespace`/`effectiveSetting`
  directly rather than reimplementing any of it.
- **The Claude Code + Codex hooks** import `plugin/scripts/_client.gen.mjs`, a
  committed, dependency-free bundle of this package. The plugin ships as raw files
  and runs under a bare `node` with no install step — the same constraint that made
  those hooks `.mjs` rather than `.ts`. Regenerate with `mise run build-client`; CI
  fails on drift via `mise run client-check`.
- **opencode, hermes, and openwebui do not consume it, on purpose.** Each ships
  standalone (opencode from npm via Bun, hermes/openwebui as Python) with no
  install-time build step of its own, so pulling in this package (or a bundler,
  for opencode) isn't an option. Each carries its own wire-shape-compatible copy
  of `gatherFacts`/`performHandshake` instead, POSTing the same
  `HandshakeRequest`/reading the same `HandshakeResponse` — the wire contract
  (`api/openapi.yaml`) is the real cross-language contract, not this package.
- **The Go CLI** talks to the server directly (`memini doctor`, `memini
namespace`); it does not read this package's override file.

The namespace override (`override.ts`) is now **read-only**: every TypeScript
integration's namespace command writes a server-side pin instead of a local
file. `readOverride` survives only so a future migration can detect and
carry forward an override left behind by an older install.
