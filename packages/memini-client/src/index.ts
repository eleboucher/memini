/**
 * @memini/client
 *
 * The shared client-side core for the config-handshake redesign: project
 * facts (facts.ts), the handshake wire client (handshake.ts), what a caller
 * should do with a handshake result (resolve.ts), behavioral-settings
 * resolution with provenance (settings.ts), secret redaction, and recovery of
 * the project directory in harness processes that are not given one. The
 * namespace *override* file (override.ts) is now read-only here — every
 * TypeScript integration's namespace command writes a server-side pin
 * (POST/DELETE /v1/pins) instead; the read path survives only for Phase 9's
 * migration of any pre-existing override left by an older install.
 *
 * Consumed three ways:
 *   - pi / openclaw import the TypeScript directly
 *   - the Claude Code + Codex hooks import plugin/scripts/_client.gen.mjs, a
 *     committed, dependency-free bundle of this package (the plugin ships as raw
 *     files with no install-time build step, which is why its hooks are .mjs)
 *   - opencode/hermes/openwebui ship standalone (no install-time build step of
 *     their own either) and carry their own wire-shape-compatible copies of
 *     gatherFacts/performHandshake rather than importing this package
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
  TRUNCATION_MARKER,
  truncateForCapture,
  buildTurnCapture,
} from "./capture.js";

export {
  OVERRIDES_VERSION,
  defaultOverridesPath,
  overrideKey,
  readOverrides,
  readOverride,
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
  BEHAVIOR_KNOBS,
  effectiveSetting,
  type BehaviorKnob,
  type SettingSource,
} from "./settings.js";

export {
  readBootstrap,
  assertBearerTransportSafe,
  isPlaintextBearerUnsafe,
  envEnabled,
  envTimeoutMs,
  DEFAULT_TIMEOUT_MS,
  MIN_TIMEOUT_MS,
  type Bootstrap,
} from "./bootstrap.js";

export {
  gatherFacts,
  factsFingerprint,
  type ProjectFacts,
} from "./facts.js";

export {
  deriveLocalNamespace,
  resolveNamespace,
  repoNameFromRemote,
  repoSlugFromRemote,
  type LocalSource,
} from "./resolve.js";

export {
  performHandshake,
  handshakeCachePath,
  readCachedHandshake,
  writeCachedHandshake,
  deleteCachedHandshake,
  invalidateAllHandshakes,
  HANDSHAKE_TTL_MS,
  type HandshakeResult,
} from "./handshake.js";

export {
  isContentHash,
  injectedIdentity,
  pretoolFingerprint,
  briefingContentHash,
  MAX_INJECTED_IDS,
  normInjectedEntry,
  normalizeInjectedState,
  recordInjected,
  mergeInjectedStates,
  injectedSuppressed,
  cooldownIds,
  pretoolExcludeIds,
  approxTokens,
  fitByTokens,
  RECALL_DETAIL_HEADER,
  escapeMeminiTags,
  truncate,
  memoryHandle,
  formatRecallHit,
  recallHitTruncated,
  recallDropFooter,
  briefingDropFooter,
  injectedReport,
  type InjectedEntry,
  type InjectedState,
  type InjectedStateV2,
  type SuppressionWindow,
  type TokenFit,
  type InjectedReportBody,
} from "./enforce/index.js";
