# memini for opencode v2

Automatic cross-session memory for opencode's v2 plugin system (`opencode2`):
recall before each turn, capture after, plus a `memini_status` tool.

This is the v2 entrypoint. For opencode v1 use
[`@eleboucher/opencode-memini`](https://www.npmjs.com/package/@eleboucher/opencode-memini)
instead — v2 replaced the `{ id, server }` plugin contract with
`{ id, setup }`, so the two are not interchangeable.

## Install

```jsonc
{
  "plugins": ["@eleboucher/opencode-memini-v2"],
}
```

Or pin a version:

```jsonc
{
  "plugins": ["@eleboucher/opencode-memini-v2@0.7.19"],
}
```

A bare entry resolves to the `latest` dist-tag, so this package is always
published there. `opencode2 plugin add @eleboucher/opencode-memini-v2` writes
the entry for you, using whatever specifier you pass it.

## Configure

Use the object form to pass options:

```jsonc
{
  "plugins": [
    {
      "package": "@eleboucher/opencode-memini-v2",
      "options": { "namespace": "my-project" },
    },
  ],
}
```

Options and environment variables are documented in the
[integration README](https://git.erwanleboucher.dev/eleboucher/memini/src/branch/main/integrations/opencode/README.md).

## From a checkout

A path entry loads the plugin file directly. opencode does not install
dependencies for path-loaded plugins — this one has none, so it just works:

```jsonc
{
  "plugins": ["/absolute/path/to/memini/integrations/opencode/plugin/memini-v2.js"],
}
```

## Requirements

A running [memini](https://git.erwanleboucher.dev/eleboucher/memini) server;
point the plugin at it with `MEMINI_BASE_URL`.

This package has no runtime dependencies. It does not import
`@opencode-ai/plugin`: `Plugin.define` is an identity function upstream, so the
module's default export is the same plain `{ id, setup }` object either way,
and skipping the dependency means there is no SDK version to keep matched
against your opencode build.
