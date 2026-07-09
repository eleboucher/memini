# @memini/namespace-resolver

Shared namespace resolution for memini integrations. Resolves hierarchical namespaces
(`work/memini/orchestrator`) from a config file + git/cwd + agent identity, with a
configurable template engine that drops unresolvable segments.

## Install

```sh
pnpm add @memini/namespace-resolver
```

## Usage

```typescript
import { resolveNamespace } from "@memini/namespace-resolver";

const { namespace, segments, source } = resolveNamespace({
  cwd: process.cwd(),
  env: process.env,
  integration: "pi",
});
// namespace: "work/memini/orchestrator"
// segments: { tenant: "work", project: "memini", agent: "orchestrator" }
// source: "config"
```

## Config file

`~/.config/memini/config.json` (or `$XDG_CONFIG_HOME/memini/config.json`):

```json
{
  "tenantRoots": [
    { "path": "~/dev/work", "tenant": "work" },
    { "path": "~/dev/personal", "tenant": "personal" }
  ],
  "template": "{tenant}/{project}/{agent}",
  "overrides": {
    "openclaw": {
      "template": "{namespace}-{agent}",
      "namespace": "work/openclaw"
    }
  }
}
```

## Resolution chain

1. **`MEMINI_NAMESPACE` env** — wins immediately (backward compat)
2. **Config tenant roots** — match cwd against `tenantRoots[].path` → `{tenant}` segment
3. **Project** — `gitRemoteUrl` > `git remote get-url origin` > toplevel basename > cwd basename → `{project}`
4. **Agent** — `agentId` > `MEMINI_AGENT` env → `{agent}`
5. **Per-integration override** — `overrides[integration].namespace` → `{namespace}`
6. **Template** — substitute segments, drop unresolvable ones with slash collapse

No config file = today's exact behavior (env > git > cwd basename). Zero migration required.

## API

### `resolveNamespace(opts) → ResolveResult`

```typescript
interface ResolveOptions {
  cwd: string;
  env?: Record<string, string>;
  gitRemoteUrl?: string;
  agentId?: string;
  configPath?: string;
  integration?: string;
}

interface ResolveResult {
  namespace: string;
  segments: { tenant?: string; project?: string; agent?: string; namespace?: string };
  source: "env" | "config" | "git" | "cwd" | "default";
}
```

### `applyTemplate(template, segments) → string`

Substitutes `{tenant}`, `{project}`, `{agent}`, `{namespace}` and drops unresolvable
segments with slash collapse.

### `readConfig(configPath?) → NamespaceConfig`

Returns `{ tenantRoots, template, overrides }`. Empty config on any error.

## License

MIT
