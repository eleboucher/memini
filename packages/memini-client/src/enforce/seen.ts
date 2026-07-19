/**
 * Cross-surface injected-memory ("seen") state: the windowed suppression
 * predicate, the cooldown-id lister, the v1→v2 state normalization, and the
 * merge-on-write algorithm.
 *
 * Ported VERBATIM from plugin/scripts/_shared.mjs; semantics are pinned by
 * vectors/enforcement.json. Pure: no fs, no env — file I/O stays in the Node
 * adapter (_shared.mjs), which passes plain objects in and out. Timestamps
 * are parameters; the Date.now() defaults exist only so call sites that mean
 * "now" keep their historical signature.
 *
 * State v2 shape (`<sessionId>.injected.json`):
 *   { "v": 2, "n": <prompt-counter>, "ids": { "<memId>": { "h", "at", "n" } } }
 *   - top-level `n`: a monotonic per-session prompt counter (bumped once per
 *     user prompt by the prompt surface). Persisted even when nothing is
 *     injected, so the counter can't slide.
 *   - per-entry: `h` = content-identity hash (sentinel "" for MCP tool-reads,
 *     whose concise responses may truncate — content identity is unknowable),
 *     `at` = last-injected epoch ms, `n` = the counter value at injection.
 */

/** One recorded injection: content-identity hash, epoch ms, prompt counter. */
export interface InjectedEntry {
  h: string;
  at: number;
  n: number;
}

/** The in-memory injected state: prompt counter + id → entry map. */
export interface InjectedState {
  n: number;
  ids: Record<string, InjectedEntry>;
}

/** The persisted (v2) shape of the injected state. */
export interface InjectedStateV2 extends InjectedState {
  v: 2;
}

// Bounded to the server's exclude_ids cap: keeping more than we can send is
// pure growth with no dedupe value.
export const MAX_INJECTED_IDS = 512;

/** Coerce a raw v2 ids-map entry into a well-formed { h, at, n } or null. */
export function normInjectedEntry(e: any, fallbackAt: number): InjectedEntry | null {
  if (!e || typeof e !== "object" || Array.isArray(e) || typeof e.h !== "string") return null;
  return {
    h: e.h,
    at: Number.isFinite(e.at) ? e.at : fallbackAt,
    n: Number.isFinite(e.n) ? e.n : 0,
  };
}

/**
 * Normalize a raw parsed state file into { n, ids }. Migrates a legacy v1
 * flat file (id → hash string) in-memory to { h, at: now, n: 0 } entries.
 * Junk values are skipped; malformed input yields an empty state — never a
 * crash. (The fs read wrapper lives in the Node adapter.)
 */
export function normalizeInjectedState(raw: any, now: number = Date.now()): InjectedState {
  const empty: InjectedState = { n: 0, ids: {} };
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return empty;
  const ids: Record<string, InjectedEntry> = {};
  if (raw.v === 2) {
    const rawIds = raw.ids && typeof raw.ids === "object" && !Array.isArray(raw.ids) ? raw.ids : {};
    for (const [id, e] of Object.entries(rawIds)) {
      const norm = normInjectedEntry(e, now);
      if (id && norm) ids[id] = norm;
    }
    return { n: Number.isFinite(raw.n) ? raw.n : 0, ids };
  }
  // v1 flat migration: { id: "hash" } → { h, at: now, n: 0 }.
  for (const [id, h] of Object.entries(raw)) {
    if (id && typeof h === "string") ids[id] = { h, at: now, n: 0 };
  }
  return { n: 0, ids };
}

/**
 * Record an injection into an in-memory state: ids[id] = { h, at: now, n } with
 * n stamped from the state's current prompt counter. Mutates and returns state.
 */
export function recordInjected(state: any, id: string, h: string, now: number = Date.now()): any {
  if (!state.ids) state.ids = {};
  state.ids[id] = { h, at: now, n: Number.isFinite(state.n) ? state.n : 0 };
  return state;
}

/**
 * Merge an in-memory state over the state read back from disk, bounding ids to
 * MAX_INJECTED_IDS by NEWEST `at` (evict oldest). Merge-on-write for
 * concurrent RMW safety: n = max(disk.n, mem.n) and, per id, keep the entry
 * with the larger `at` (residual last-write-wins on ties is accepted —
 * bounded to one extra suppression or injection). Returns the v2 shape the
 * Node adapter persists verbatim.
 */
export function mergeInjectedStates(disk: InjectedState, mem: any, now: number = Date.now()): InjectedStateV2 {
  const m = mem && typeof mem === "object" ? mem : {};
  const memIds = m.ids && typeof m.ids === "object" ? m.ids : {};
  const memN = Number.isFinite(m.n) ? m.n : 0;

  const merged: Record<string, InjectedEntry> = { ...disk.ids };
  for (const [id, e] of Object.entries(memIds)) {
    const norm = normInjectedEntry(e, now);
    if (!id || !norm) continue;
    const prev = merged[id];
    if (!prev || norm.at >= prev.at) merged[id] = norm;
  }
  const n = Math.max(memN, disk.n);

  let entries = Object.entries(merged);
  if (entries.length > MAX_INJECTED_IDS) {
    entries.sort((a, b) => (b[1]?.at || 0) - (a[1]?.at || 0));
    entries = entries.slice(0, MAX_INJECTED_IDS);
  }
  return { v: 2, n, ids: Object.fromEntries(entries) };
}

/** The windowed-suppression knobs every predicate call carries. */
export interface SuppressionWindow {
  now: number;
  counter: number;
  cooldownMs: number;
  cooldownPrompts: number;
}

/**
 * Shared cooldown predicate: is this recorded entry still suppressed?
 *
 *   suppressed(entry, identity, { now, counter, cooldownMs, cooldownPrompts }):
 *     entry.h === ""                       → true   sentinel/tool-read: FOREVER
 *                                                   (the model pulled it; hooks
 *                                                   never re-push it)
 *     identity && entry.h !== identity     → false  content changed → re-inject
 *     cooldownMs == 0 && prompts == 0      → true   legacy forever-dedupe (#134)
 *     promptDim = prompts > 0 && counter > 0 && counter - entry.n < prompts
 *                  (counter == 0 ⇒ prompt dimension inert: a host that never
 *                   fires a prompt hook degrades to time-only, not forever)
 *     timeDim   = cooldownMs > 0 && now - entry.at < cooldownMs
 *                  (negative deltas — clock skew / counter regression — clamp
 *                   to suppressed)
 *     → promptDim || timeDim                        suppress within EITHER
 *                                                   window; re-admit once BOTH
 *                                                   lapse
 *
 * `identity` null means "id-only check" (used by cooldownIds): the content-
 * change bypass is skipped, so a real-hash entry is judged on the windows
 * alone and a sentinel stays suppressed.
 */
export function injectedSuppressed(
  entry: any,
  identity: string | null | undefined,
  { now, counter, cooldownMs, cooldownPrompts }: SuppressionWindow,
): boolean {
  if (!entry || typeof entry !== "object") return false;
  if (entry.h === "") return true; // sentinel / tool-read: forever
  if (identity && entry.h !== identity) return false; // content changed: re-inject
  if (cooldownMs === 0 && cooldownPrompts === 0) return true; // legacy forever-dedupe
  const promptDim = cooldownPrompts > 0 && counter > 0 && counter - entry.n < cooldownPrompts;
  const timeDim = cooldownMs > 0 && now - entry.at < cooldownMs;
  return promptDim || timeDim;
}

/**
 * The ids currently in cooldown for exclude_ids senders: every recorded entry
 * the predicate suppresses judged by id alone (identity=null) against the
 * state's own counter. Sentinel entries are always in cooldown; real-hash
 * entries ride the time/prompt windows.
 */
export function cooldownIds(
  state: any,
  { now, cooldownMs, cooldownPrompts }: { now: number; cooldownMs: number; cooldownPrompts: number },
): string[] {
  const ids = state && state.ids && typeof state.ids === "object" ? state.ids : {};
  const counter = Number.isFinite(state?.n) ? state.n : 0;
  const out: string[] = [];
  for (const [id, entry] of Object.entries(ids)) {
    if (injectedSuppressed(entry, null, { now, counter, cooldownMs, cooldownPrompts })) out.push(id);
  }
  return out;
}
