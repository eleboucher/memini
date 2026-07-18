# Document, package, and verify the Pi integration

Status: ready-for-agent
Type: task
Blocked by: 01, 02, 03, 04, 05

## What to build

The published package and integration guide should accurately describe the implemented behavior and fail release validation when the source, lockfile, bundle, tests, or tool contract drift.

## Acceptance criteria

- [ ] The guide lists the actual tools, lifecycle behavior, compact rendering, configuration precedence, and slash-command syntax.
- [ ] Package and lockfile versions/dependencies are synchronized.
- [ ] The standard verification path runs typechecking, focused tests, bundle build, and package-install validation.
- [ ] The full Pi suite and relevant shared-client/Claude parity tests pass.
- [ ] All completed issue files are marked resolved with validation evidence appended under Comments.
- [ ] The completed work is committed on `fix/pi-plugin-parity` with no unrelated untracked artifacts added.

## Comments
