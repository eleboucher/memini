# Contributing to memini

## Quick start

[mise](https://mise.jdx.dev/) is the single source of truth for tool versions
and dev tasks (`.mise/config.toml`). CI runs the same tasks, so local green
means CI green.

```sh
mise install     # toolchain + git hooks (lefthook, via the postinstall hook)
mise run test    # gofmt + go vet + unit tests with coverage
mise run build   # build ./bin/memini with the embedded UI
mise run run     # run the server from source
mise tasks       # everything else
```

A devcontainer exists (`.devcontainer/devcontainer.json`: Debian base, mise,
docker-in-docker; `postCreateCommand` runs `mise trust && mise install`), so
"open in container" lands you in a ready environment.

The dev SQLite database is kept out of your way: `.mise/config.toml` sets
`MEMINI_SQLITE_PATH` to `bin/memini.dev.db` (gitignored).

Note on the CLI: the server is bare `memini` — there is no `serve` subcommand.

## Pre-commit gates (lefthook)

`mise install` installs the git hooks (`.lefthook.toml`). On commit:

| Hook            | Runs on staged          | Does                                                                    |
| --------------- | ----------------------- | ----------------------------------------------------------------------- |
| `gofmt`         | `*.go`                  | `gofmt -w`, re-stages the fixes                                         |
| `golangci-lint` | `*.go`                  | `golangci-lint run`                                                     |
| `format`        | `*.{md,json,yaml,yml}`  | `oxfmt --write` (honours `.prettierignore`), re-stages                  |
| `tidy`          | `go.{mod,sum}`          | `go mod tidy`, re-stages                                                |
| drift gates     | each generator's inputs | the matching `mise run *-check` task from the table below (glob-scoped) |

On push: `go test ./...`, plus a compile-only check of the bench harnesses
(`go test -tags bench -run=^$ ./bench/` — builds them, runs nothing, so the
minutes-long live-embedder eval never triggers accidentally).

## Generated files and drift gates

Several committed files are generated. **Never hand-edit generated output** —
change the generator's input and regenerate. CI (and the pre-commit hooks)
fail on drift by regenerating and diffing.

When you touch X, run Y:

| When you touch                                                                    | Run                     | Regenerates                                                                                                                   | Gate              |
| --------------------------------------------------------------------------------- | ----------------------- | ----------------------------------------------------------------------------------------------------------------------------- | ----------------- |
| `internal/config/config.go` (env tags, doc comments), `api/openapi.yaml`          | `mise run docs`         | `docs/reference/` — `configuration.md`, `cli.md`, `mcp-tools.md`, `rest-api.md` (`env-vars.md` is the hand-written exception) | `docs-check`      |
| `packages/memini-client/src/**/*.ts`, `packages/memini-client/scripts/bundle.mjs` | `mise run build-client` | `plugin/scripts/_client.gen.mjs` (the plugin ships as raw files run under bare `node`, so the bundle is committed)            | `client-check`    |
| `api/openapi.yaml`, `internal/api/rest/oapi-codegen.yaml`                         | `mise run rest-check`   | `internal/api/rest/api.gen.go`                                                                                                | `rest-check`      |
| `api/openapi.yaml`, `ui/scripts/gen-catalog.mjs`                                  | `mise run gen-api`      | `ui/src/settings-catalog.gen.ts`, `ui/src/api-schema.gen.ts`                                                                  | `gen-api-check`   |
| `charts/memini/**` (values comments, `Chart.yaml`, `README.md.gotmpl`)            | `mise run helm-docs`    | `charts/memini/README.md`, `charts/memini/values.schema.json`                                                                 | `helm-docs-check` |

**Warning — vendor the chart dependencies before `mise run helm-docs`.** The
dependency archive under `charts/memini/charts/` is gitignored, so a fresh
checkout does not have it. Run:

```sh
helm repo add bjw-s https://bjw-s-labs.github.io/helm-charts
helm dependency build charts/memini
```

first. Without the vendored dependency, `helm-schema` silently truncates
`values.schema.json` instead of failing — you commit a broken schema and only
the CI drift gate (which vendors deps first) catches it.

Each `*-check` task regenerates and then `git diff --exit-code`s the output,
so running the check also fixes your working tree.

## Tests

`mise run test` runs the unit suite (SQLite-backed, no external services).
`mise run test-hooks` and `mise run test-client` cover the Node plugin scripts
and the shared TypeScript client.

### Build tags

| Tag           | What lives behind it                                                                                                                                                                                                                     |
| ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `integration` | Suites needing external services or a full server boot: the Postgres store conformance tests (`internal/store/postgres`) and the end-to-end server tests (`cmd/memini`). Run with `mise run test-integration`.                           |
| `gendocs`     | `cmd/memini/gendocs.go` — the `gen-docs` subcommand that renders `docs/reference/cli.md`. Behind a tag so the doc generator stays out of the shipped binary; invoked as `go run -tags gendocs ./cmd/memini gen-docs` by `mise run docs`. |
| `bench`       | The retrieval-eval harnesses in `bench/` (need a live embedder, take minutes). Compile-checked in CI and on push; run via `mise run bench`.                                                                                              |

### The Postgres conformance suite

The Postgres-backed tests are enabled by `MEMINI_TEST_POSTGRES_DSN`; without
it they `t.Skip`. The database must ship VectorChord — the easiest local one
is the compose service:

```sh
docker compose up -d db
MEMINI_TEST_POSTGRES_DSN="postgres://postgres:memini@localhost:5432/memini?sslmode=disable" \
  go test -tags integration ./internal/store/postgres/ ./cmd/memini/
```

CI runs exactly this (against `ghcr.io/tensorchord/vchord-postgres`) so the
Postgres backend cannot rot untested; the SQLite paths of the e2e suite run
with the tag even without the DSN.

## Documentation conventions

- **Generated files are never hand-edited** (see the drift-gate table). Any
  _generated markdown_ must also be listed in `.prettierignore`: the
  pre-commit `oxfmt` hook reformats markdown, and a reformatted generated page
  fails its drift gate on an otherwise clean tree.
- **No emojis**, in docs or in user-facing output.
- Prose style follows the existing docs: sentence-case headings, concrete
  commands with realistic values, American English, tables where they beat
  prose.
- Worked examples in `docs/examples/` end with a `Validated by:` footer naming
  the Go test file that pins the example's behavioral claims (the quoted
  outputs are shapes those tests assert). If you change an example, change its
  test; if you add one, add a test.
- **`docs/scopes.md#knobs` is load-bearing.** The boot-fatal messages for
  removed scope variables in `internal/config/config.go` cite that anchor, and
  `internal/config/config_test.go` (`assertFatalMessageComplete`) pins the
  citation. Renaming or moving the `## Knobs` heading requires changing the
  code and the test in lockstep — treat `scopes.md` as append-only around it.
