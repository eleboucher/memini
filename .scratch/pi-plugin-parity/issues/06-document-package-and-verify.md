# Document, package, and verify the Pi integration

Status: resolved
Type: task
Blocked by: 01, 02, 03, 04, 05

## What to build

The published package and integration guide should accurately describe the implemented behavior and fail release validation when the source, lockfile, bundle, tests, or tool contract drift.

## Acceptance criteria

- [x] The guide lists the actual tools, lifecycle behavior, compact rendering, configuration precedence, and slash-command syntax.
- [x] Package and lockfile versions/dependencies are synchronized.
- [x] The standard verification path runs typechecking, focused tests, bundle build, and package-install validation.
- [x] The full Pi suite and relevant shared-client/Claude parity tests pass.
- [x] All completed issue files are marked resolved with validation evidence appended under Comments.
- [x] The completed work is committed on `fix/pi-plugin-parity` with no unrelated untracked artifacts added.

## Comments

Resolved on `fix/pi-plugin-parity`.

Implementation and validation evidence:

- The Pi guide now documents `pi install`, leading-slash command syntax, all eight always-on tools plus capability-gated `memory_answer`, lifecycle hooks, branch-aware dedupe, compact/expanded rendering, host-peer packaging, and the actual environment/server/default precedence.
- Package metadata targets Node 22/Pi 0.80.6, identifies the npm artifact as a Pi package, keeps Pi core modules as peers, and synchronizes both `package-lock.json` and the workspace `pnpm-lock.yaml`.
- `npm test` is now the standard gate: typecheck, clean bundle build, three packaging/bundle checks, 74 helper/lifecycle/tool-contract tests, and one clean-consumer pack/install/import test all passed (**78/78**).
- `npm pack --dry-run` passed after its `prepack` typecheck/build and contained only `dist/index.js` plus `package.json`; a stale declaration artifact can no longer leak into the tarball.
- `pnpm install --frozen-lockfile`, shared-client tests (**105/105**), Claude Code/Codex hook tests (**176/176**), and opencode parity tests (**82/82**) passed. The Claude migration fixture was made macOS-safe by comparing the canonical override key it actually writes.
- CI now uses frozen workspace installation and delegates the Pi job to the same standard package test path developers run locally.
- The pre-existing untracked `.pi-subagents/`, `integrations/pi/plugin/test/package.test.mjs`, `target/`, and `ui/dist/` paths were neither added nor modified by this slice.

Final integration validation:

- Pi standard gate passed: 3 bundle/package-metadata tests, 91 helper/lifecycle/contract tests, and 1 clean-consumer package-install/factory test (**95/95**), including typecheck and clean build.
- Packed dry-run passed with only `dist/index.js` and `package.json`; installed factory registration is invoked against functional host peer stubs.
- Pi host peers now use the required `"*"` ranges while Pi 0.80.6 remains the dev/test pin and documented minimum.
- Frozen workspace install passed; shared client passed **105/105** on confirmation rerun (the first run had one transient parent-process timing failure); Claude Code/Codex hooks passed **176/176**.
- `git diff --check` passed. `yamllint` and `ansible-lint` were unavailable; no Ansible files changed.
