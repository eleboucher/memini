# @memini/client

The shared client-side core for memini integrations. Everything here is about
what the _client_ does — what namespace it will use, what settings are actually
in force, and how it finds the project it is working in.

It is a sibling of [`@memini/namespace-resolver`](../namespace-resolver), not a
replacement. That package is a namespace resolution chain; this one is override,
introspection, and harness plumbing. They are deliberately not merged: each
harness already has its own resolver, and `describeSettings` takes that resolver
as a callback rather than imposing one.

## What it provides

| Module               | Purpose                                                           |
| -------------------- | ----------------------------------------------------------------- |
| `override`           | Per-project namespace override — read, write, clear               |
| `settings`           | `describeSettings()`: every client knob with its **provenance**   |
| `redact`             | Secret redaction, always on                                       |
| `namespace-validate` | Normalization, plus the header-safety rules                       |
| `session`            | Recover the project directory in processes the harness gives none |

## Provenance is the feature

A settings dump that only prints values is nearly useless. The case this package
was written for: `MEMINI_NAMESPACE` exported as a **fish universal variable**,
set once and forgotten, silently collapsing every repo on the machine into one
shared namespace. Nothing surfaced it, and memories from a dozen projects piled
into one pool.

A list of values would have shown `namespace: default` and looked fine. What
catches it is the provenance:

```
namespace   default        <- env:MEMINI_NAMESPACE
                              (git would give: memini)
```

So `describeSettings()` resolves the namespace three times, against progressively
stripped environments, and reports all three:

- **effective** — what the harness will actually use
- **withoutOverride** — what it would be with the override removed
- **derived** — what it would be with the override _and_ `MEMINI_NAMESPACE`
  removed, i.e. pure git/cwd derivation

## The override beats the environment

Ordering is deliberate:

```
1. project override      <- wins outright
2. MEMINI_NAMESPACE
3. config file / git / cwd
```

If the environment won, then on a machine with a globally exported
`MEMINI_NAMESPACE` — exactly the machine that needs an override most — setting one
would silently do nothing.

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
  make them uninstallable — their own bundle tests enforce that.
- **The Claude Code + Codex hooks** import `plugin/scripts/_client.gen.mjs`, a
  committed, dependency-free bundle of this package. The plugin ships as raw files
  and runs under a bare `node` with no install step — the same constraint that made
  those hooks `.mjs` rather than `.ts`. Regenerate with `mise run build-client`; CI
  fails on drift via `mise run client-check`.
- **The Go CLI** reads the same `overrides.json`, so `memini doctor` and the plugin
  can never disagree about which namespace is in force.
- **opencode does not consume it, on purpose.** It ships from npm as a single
  dependency-free file that opencode installs with Bun at startup. Pulling in a
  bundler to save ~50 lines of `JSON.parse` and `git rev-parse` would trade away the
  one property that makes it easy to install. It reimplements the override _reader_
  — but reads the same file, with the same precedence.
- **hermes and openwebui** are Python; the TS core is simply unreachable. They too
  reimplement the reader against the same file format.

The file format is therefore the real contract, not the package. It is deliberately
boring for that reason: JSON, one `git rev-parse` for the key, and every error path
degrades to "no override" rather than raising.
