/**
 * The shared injection-enforcement core: every PURE primitive that decides
 * what an integration injects, suppresses, trims, and reports.
 *
 * Semantics are pinned by vectors/enforcement.json (verified by
 * test/enforcement-vectors.test.ts): any port of these functions — TypeScript
 * or otherwise — must reproduce those vectors byte-for-byte. File I/O,
 * env reads, and network transports stay in each integration's adapter
 * (plugin/scripts/_shared.mjs for the Claude Code hooks); these modules take
 * plain objects and explicit timestamps/counters.
 */

export {
  isContentHash,
  injectedIdentity,
  pretoolFingerprint,
  briefingContentHash,
} from "./identity.js";

export {
  MAX_INJECTED_IDS,
  normInjectedEntry,
  normalizeInjectedState,
  recordInjected,
  mergeInjectedStates,
  injectedSuppressed,
  cooldownIds,
  type InjectedEntry,
  type InjectedState,
  type InjectedStateV2,
  type SuppressionWindow,
} from "./seen.js";

export {
  approxTokens,
  fitByTokens,
  type TokenFit,
} from "./budget.js";

export {
  RECALL_DETAIL_HEADER,
  escapeMeminiTags,
  truncate,
  memoryHandle,
  formatRecallHit,
  recallHitTruncated,
  recallDropFooter,
  briefingDropFooter,
} from "./render.js";

export {
  injectedReport,
  type InjectedReportBody,
} from "./telemetry.js";
