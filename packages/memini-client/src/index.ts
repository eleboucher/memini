/**
 * @memini/client
 *
 * The shared client-side core for memini integrations: namespace override,
 * effective-settings introspection with provenance, secret redaction, and
 * recovery of the project directory in harness processes that are not given one.
 *
 * Deliberately does NOT depend on @memini/namespace-resolver. The two are
 * siblings: that package is a namespace *resolution* chain, this one is
 * override + reporting. Keeping them apart matters because each harness already
 * has its own resolver (the Claude hooks consult a self-healing project map the
 * shared resolver knows nothing about), and `describeSettings` takes the
 * harness's resolver as a callback rather than imposing one. Bundling a second,
 * subtly-different resolver into a plugin that already has one is precisely the
 * drift this package exists to end.
 *
 * Consumed three ways:
 *   - pi / openclaw / opencode import the TypeScript directly
 *   - the Claude Code + Codex hooks import plugin/scripts/_client.gen.mjs, a
 *     committed, dependency-free bundle of this package (the plugin ships as raw
 *     files with no install-time build step, which is why its hooks are .mjs)
 *   - the Go CLI reads the same overrides.json, so `memini doctor` and the
 *     plugin can never disagree about which namespace is in force
 */

export {
  isSensitive,
  redactValue,
  redactByName,
} from "./redact.js";

export {
  MAX_NAMESPACE_BYTES,
  normalizeNamespace,
  validateNamespace,
} from "./namespace-validate.js";

export {
  OVERRIDES_VERSION,
  defaultOverridesPath,
  overrideKey,
  readOverrides,
  readOverride,
  writeOverride,
  clearOverride,
  type NamespaceOverride,
  type OverridesFile,
  type OverrideOptions,
} from "./override.js";

export {
  cacheDir,
  sessionCwdPath,
  writeSessionCwd,
  readSessionCwd,
  deleteSessionCwd,
  processCwd,
  looksLikePluginRoot,
  resolveHarnessCwd,
  SESSION_CWD_TTL_MS,
  type CwdSource,
  type HarnessCwd,
} from "./session.js";

export {
  CLIENT_KNOBS,
  describeSettings,
  type ClientSettings,
  type DescribeOptions,
  type HarnessResolver,
  type KnobKind,
  type KnobSpec,
  type NamespaceReport,
  type NamespaceSource,
  type ResolvedNamespace,
  type ResolveOpts,
  type SettingValue,
  type Warning,
  type WarningLevel,
} from "./settings.js";
