/**
 * memini memory plugin for opencode.
 *
 * Hooks opencode's plugin API so memory is automatic — the model never has to
 * call a tool:
 *   - chat.message: recall memories relevant to the incoming user message and
 *     inject them as a synthetic context part before the turn runs.
 *   - event (session.idle): capture the completed user/assistant turn into
 *     memini as episodic memory once the session goes idle.
 *
 * Talks to memini over REST (/v1/search, /v1/memories), scoped by the
 * X-Memini-Namespace header. Default endpoint http://localhost:8080.
 *
 * Config comes from the plugin options (the [name, options] form in
 * opencode.json), with env-var fallbacks; secrets like MEMINI_API_KEY come
 * from the environment. Namespace and behavioral settings (recall, capture,
 * recall_limit, ...) are additionally cross-checked against the server via
 * POST /v1/handshake — see effectiveConfig() below for the precedence. See
 * the options/env table in ../README.md.
 */

import { execSync, spawnSync } from "node:child_process";
import { readFileSync, existsSync, rmSync, writeFileSync, statSync } from "node:fs";
import { resolve, join, dirname } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 30000;
const DEFAULT_RECALL_BUDGET_MS = 2000;
const DEFAULT_RECALL_LIMIT = 3;
const DEFAULT_NAMESPACE = "opencode";
const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);
// Race sentinel: distinguishes "the recall budget expired" from any value the
// search itself could resolve to (including null on a degraded failure).
const BUDGET_EXPIRED = Symbol("memini-recall-budget-expired");

// The client identifies itself to /v1/handshake for logging/diagnostics only
// (api/openapi.yaml's HandshakeRequest.client). Version is read from this
// package's own package.json (always shipped alongside memini.js — npm
// includes it regardless of the "files" allowlist) so it never has to be kept
// in sync by hand; "0.0.0" degrades gracefully when running from a checkout
// that lacks one for some reason.
const CLIENT_NAME = "opencode-memini";
function readPluginVersion() {
  try {
    const pkg = JSON.parse(readFileSync(new URL("./package.json", import.meta.url), "utf8"));
    return typeof pkg.version === "string" && pkg.version ? pkg.version : "0.0.0";
  } catch {
    return "0.0.0";
  }
}
const CLIENT_VERSION = readPluginVersion();

// Auto-update: opencode never re-fetches cached npm plugins, so the plugin
// checks npm dist-tags once per process and self-updates (same major version
// only) so the running copy stays current.
const PACKAGE_NAME = "@eleboucher/opencode-memini";
const NPM_REGISTRY_URL = `https://registry.npmjs.org/-/package/${PACKAGE_NAME}/dist-tags`;
const NPM_FETCH_TIMEOUT = 5000;
const BUN_INSTALL_TIMEOUT_MS = 60000;
let autoUpdateChecked = false;

/**
 * parseVersion extracts major.minor.patch from a semver string. Exported for
 * testing.
 */
export function parseVersion(version) {
  const normalized = String(version).trim().replace(/^[~^=<>\s]+/, "");
  const match = normalized.match(/^(\d+)\.(\d+)\.(\d+)/);
  if (!match) return null;
  return { major: Number(match[1]), minor: Number(match[2]), patch: Number(match[3]) };
}

/**
 * compareVersions returns -1, 0, or 1. Exported for testing.
 */
export function compareVersions(a, b) {
  const va = parseVersion(a);
  const vb = parseVersion(b);
  if (!va || !vb) return 0;
  if (va.major !== vb.major) return va.major < vb.major ? -1 : 1;
  if (va.minor !== vb.minor) return va.minor < vb.minor ? -1 : 1;
  if (va.patch !== vb.patch) return va.patch < vb.patch ? -1 : 1;
  return 0;
}

/**
 * resolveInstallContext finds the opencode plugin cache directory that holds
 * this running plugin instance, by walking up from import.meta.url. Returns
 * { installDir, packageJsonPath } or null.
 */
function resolveInstallContext() {
  try {
    const pluginDir = dirname(fileURLToPath(import.meta.url));
    const nodeModulesDir = dirname(pluginDir);
    const installDir = dirname(nodeModulesDir);
    const packageJsonPath = join(installDir, "package.json");
    if (existsSync(packageJsonPath)) {
      return { installDir, packageJsonPath };
    }
  } catch {}
  return null;
}

/**
 * fetchLatestVersion queries npm dist-tags with a timeout. Returns the version
 * string or null on failure.
 */
async function fetchLatestVersion() {
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), NPM_FETCH_TIMEOUT);
    try {
      const resp = await fetch(NPM_REGISTRY_URL, {
        signal: controller.signal,
        headers: { Accept: "application/json" },
      });
      if (!resp.ok) return null;
      const data = await resp.json();
      return data?.latest ?? null;
    } finally {
      clearTimeout(timer);
    }
  } catch {
    return null;
  }
}

/**
 * prepareCacheUpdate rewrites the cache package.json to pin the new version,
 * removes the installed node_modules package, and cleans bun.lock. Returns
 * the installDir on success, null on failure.
 */
function prepareCacheUpdate(newVersion, log) {
  const ctx = resolveInstallContext();
  if (!ctx) {
    log.warn("auto-update: could not resolve install context");
    return null;
  }
  // Rewrite package.json with the new version pin
  try {
    const pkg = JSON.parse(readFileSync(ctx.packageJsonPath, "utf8"));
    if (pkg.dependencies && pkg.dependencies[PACKAGE_NAME] === newVersion) {
      return ctx.installDir; // already updated
    }
    pkg.dependencies = { ...pkg.dependencies, [PACKAGE_NAME]: newVersion };
    writeFileSync(ctx.packageJsonPath, JSON.stringify(pkg, null, 2));
  } catch (err) {
    log.warn(`auto-update: failed to rewrite cache package.json: ${String(err)}`);
    return null;
  }
  // Remove installed node_modules so bun install re-fetches
  try {
    const pkgDir = join(ctx.installDir, "node_modules", "@eleboucher", "opencode-memini");
    if (existsSync(pkgDir)) rmSync(pkgDir, { recursive: true, force: true });
  } catch (err) {
    log.warn(`auto-update: failed to remove cached node_modules: ${String(err)}`);
    return null;
  }
  // Clean bun.lock entry if it exists
  const lockPath = join(ctx.installDir, "bun.lock");
  if (existsSync(lockPath)) {
    try {
      const lock = JSON.parse(readFileSync(lockPath, "utf8"));
      let modified = false;
      if (lock.workspaces?.[""]?.dependencies?.[PACKAGE_NAME]) {
        delete lock.workspaces[""].dependencies[PACKAGE_NAME];
        modified = true;
      }
      if (lock.packages?.[PACKAGE_NAME]) {
        delete lock.packages[PACKAGE_NAME];
        modified = true;
      }
      if (modified) writeFileSync(lockPath, JSON.stringify(lock, null, 2));
    } catch {
      // bun.lock format varies; if we can't parse it, leave it — bun install
      // will reconcile.
    }
  }
  return ctx.installDir;
}

/**
 * runBunInstall runs `bun install` in the given directory with a timeout.
 * Returns true on success (exit code 0), false otherwise.
 */
function runBunInstall(installDir) {
  try {
    const result = spawnSync(process.execPath, ["install"], {
      cwd: installDir,
      stdio: ["ignore", "pipe", "pipe"],
      timeout: BUN_INSTALL_TIMEOUT_MS,
    });
    return result.status === 0;
  } catch {
    return false;
  }
}

// How long a memoized handshake stays trustworthy on a live plugin instance,
// and how long a single handshake call is allowed to block before falling
// back. Mirrors packages/memini-client's HANDSHAKE_TTL_MS / default timeout;
// this plugin ships standalone so it stays a copy, not an import.
export const HANDSHAKE_TTL_MS = 10 * 60 * 1000;
export const HANDSHAKE_TIMEOUT_MS = 2500;

function envBool(value, fallback) {
  if (value === undefined || value === null || value === "") return fallback;
  return !/^(0|false|no|off)$/i.test(String(value).trim());
}

function isSet(value) {
  return value !== undefined && value !== null && String(value).trim() !== "";
}

// sanitizeNamespace keeps the X-Memini-Namespace value header-safe (the server
// sanitizes too, but the header should be clean): alnum, dot, dash, underscore;
// collapse the rest to dashes and trim.
function sanitizeNamespace(s) {
  return String(s).trim().replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
}

// basenameOf is the raw (unsanitized) basename of a path — used for the
// cwd_basename fact sent to the server, which does its own sanitizing.
// deriveNamespace (below) is the sanitized, LOCAL-fallback-only variant.
function basenameOf(p) {
  return String(p).replace(/[\\/]+$/, "").split(/[\\/]/).pop() || "";
}

// deriveNamespace scopes memory to the project: the basename of the git
// worktree (the repo dir name). This is the LOCAL fallback only — used when
// neither an explicit namespace (option/MEMINI_NAMESPACE) nor a handshake
// result is available, see effectiveConfig(). Returns "" when no path is given.
export function deriveNamespace(worktree) {
  if (typeof worktree !== "string" || !worktree.trim()) return "";
  return sanitizeNamespace(basenameOf(worktree));
}

// gitFacts best-effort gathers the git remote and toplevel for the handshake
// request body only — never for local namespace derivation, which stays the
// plain worktree basename (deriveNamespace). Mirrors
// packages/memini-client's gatherFacts so every memini client sends the same
// shape of project facts; this plugin ships standalone so it stays a copy
// rather than an import. Never throws: no git, no repo, or a slow git all
// degrade to omitting the field.
function gitFacts(cwd) {
  const gitOut = (args) => {
    try {
      return execSync(`git ${args}`, { cwd, stdio: ["ignore", "pipe", "ignore"], timeout: 500 })
        .toString()
        .trim();
    } catch {
      return "";
    }
  };
  const facts = {};
  const remote = gitOut("remote get-url origin");
  if (remote) facts.remote_url = remote;
  const toplevel = gitOut("rev-parse --show-toplevel");
  if (toplevel) {
    facts.toplevel_path = toplevel;
    facts.toplevel_basename = basenameOf(toplevel);
  }
  return facts;
}

// buildFacts assembles HandshakeRequest.project (api/openapi.yaml): the
// worktree basename (always present, the last-resort fallback), git
// remote/toplevel (best-effort), and MEMINI_NAMESPACE as env_namespace — sent
// so a server-side pin can still beat it (the client cannot make that call
// itself without knowing whether a pin exists). Exported for testing.
export function buildFacts(dir, env) {
  const e = env || {};
  const facts = { cwd_basename: basenameOf(dir || process.cwd()), ...gitFacts(dir) };
  const ns = String(e.MEMINI_NAMESPACE || "").trim();
  if (ns) facts.env_namespace = ns;
  return facts;
}

// resolveConfig merges env vars with the options object (options win), filling
// in defaults. Exported for testing.
//
// Namespace here is LOCAL ONLY: the `namespace` option / MEMINI_NAMESPACE
// (raw-trimmed — the server validates the header, and flattening "/" here
// would split a tenant path like work/memini in two), else the git worktree
// basename, else the built-in default. See effectiveConfig() below for how a
// handshake result is layered on top of this: a handshake can win the
// worktree/default tail, but never an explicit option or env value.
//
// Each recall/capture knob also carries an `explicit` flag alongside its
// locally-resolved (option > env > built-in default) value, so
// effectiveConfig() knows whether a handshake's server settings are allowed
// to fill it in.
export function resolveConfig(env, options, worktree) {
  const e = env || {};
  const o = options || {};

  let namespace;
  let namespace_source;
  if (o.namespace && String(o.namespace).trim()) {
    namespace = String(o.namespace).trim();
    namespace_source = "option";
  } else if (e.MEMINI_NAMESPACE && String(e.MEMINI_NAMESPACE).trim()) {
    namespace = String(e.MEMINI_NAMESPACE).trim();
    namespace_source = "env";
  } else {
    const derived = deriveNamespace(worktree);
    namespace = derived || DEFAULT_NAMESPACE;
    namespace_source = derived ? "local-worktree" : "local-default";
  }

  // Number.isFinite guard: malformed env / option falls through to the next
  // source instead of NaN flowing into the request body.
  const recall_limit = (() => {
    for (const v of [o.recall_limit, e.MEMINI_RECALL_LIMIT, DEFAULT_RECALL_LIMIT]) {
      const n = Number(v);
      if (Number.isFinite(n) && n >= 0) return n;
    }
    return DEFAULT_RECALL_LIMIT;
  })();
  // How long chat.message waits for recall before letting the turn proceed
  // without it; 0 disables the race (fully blocking recall). The ""-skip
  // matters: Number("") === 0, so an empty env var would silently go blocking.
  const recall_budget_ms = (() => {
    for (const v of [o.recall_budget_ms, e.MEMINI_RECALL_BUDGET_MS, DEFAULT_RECALL_BUDGET_MS]) {
      if (v === undefined || v === null || v === "") continue;
      const n = Number(v);
      if (Number.isFinite(n) && n >= 0) return n;
    }
    return DEFAULT_RECALL_BUDGET_MS;
  })();
  // home: the caller's personal namespace, sent as X-Memini-Home. Same
  // env-only resolution style as namespace's MEMINI_NAMESPACE (option wins
  // over env), but no derivation fallback — unset means "no home leg", not a
  // guess. Not layered from the server: it is a purely local, per-caller knob.
  const homeRaw = o.home !== undefined ? o.home : e.MEMINI_HOME;
  const home = homeRaw && String(homeRaw).trim() ? String(homeRaw).trim() : undefined;

  return {
    base_url: o.base_url || e.MEMINI_BASE_URL || DEFAULT_BASE_URL,
    // namespace is already resolved above (explicit raw-trimmed, or the
    // sanitized worktree/default fallback); re-sanitizing here would flatten
    // a tenant "/" separator.
    namespace,
    namespace_source,
    home,
    recall: o.recall !== undefined ? o.recall !== false : envBool(e.MEMINI_RECALL, true),
    capture: o.capture !== undefined ? o.capture !== false : envBool(e.MEMINI_CAPTURE, true),
    recall_limit,
    recall_max_tokens:
      o.recall_max_tokens !== undefined
        ? Number(o.recall_max_tokens) || 0
        : intEnv("MEMINI_INJECT_RECALL_MAX_TOK", 0),
    recall_min_score:
      o.recall_min_score !== undefined
        ? Number(o.recall_min_score) || 0
        : floatEnv("MEMINI_INJECT_RECALL_MIN_SCORE", 0),
    recall_budget_ms,
    timeout_ms: Number(o.timeout_ms || e.MEMINI_TIMEOUT_MS || DEFAULT_TIMEOUT_MS),
    fallback_on_error:
      o.fallback_on_error !== undefined
        ? o.fallback_on_error !== false
        : envBool(e.MEMINI_FALLBACK, true),
    auto_update: o.auto_update !== undefined ? o.auto_update !== false : envBool(e.MEMINI_AUTO_UPDATE, true),
    // Recorded so effectiveConfig() can tell "explicitly set to the built-in
    // default" apart from "not set at all" — only the latter may be filled in
    // from the server.
    explicit: {
      recall: o.recall !== undefined || isSet(e.MEMINI_RECALL),
      capture: o.capture !== undefined || isSet(e.MEMINI_CAPTURE),
      recall_limit: o.recall_limit !== undefined || isSet(e.MEMINI_RECALL_LIMIT),
      recall_max_tokens: o.recall_max_tokens !== undefined || isSet(process.env.MEMINI_INJECT_RECALL_MAX_TOK),
      recall_min_score: o.recall_min_score !== undefined || isSet(process.env.MEMINI_INJECT_RECALL_MIN_SCORE),
    },
  };
}

// effectiveConfig merges a (possibly null — handshake is fail-soft) handshake
// result into the locally-resolved `cfg` from resolveConfig():
//
//   namespace: option/env (cfg.namespace_source already "option"/"env") beats
//     the handshake's resolved namespace beats cfg's own local
//     worktree/default fallback.
//   recall/capture/recall_limit/recall_max_tokens/recall_min_score: option
//     beats env (both already baked into cfg, tracked by cfg.explicit) beats
//     the handshake's `settings` (ClientSettings — api/openapi.yaml) beats
//     the built-in default already baked into cfg.
//
// hs may be null/undefined (network error, non-2xx, timeout — see
// createClient's handshake()) or shaped without the fields this cares about;
// every read below tolerates that and falls back to cfg. Exported for testing.
export function effectiveConfig(cfg, hs) {
  let namespace = cfg.namespace;
  let namespace_source = cfg.namespace_source;
  if (namespace_source !== "option" && namespace_source !== "env" && hs && hs.namespace) {
    namespace = hs.namespace;
    namespace_source = `server:${hs.namespace_source}`;
  }

  const s = (hs && hs.settings) || {};
  const explicit = cfg.explicit || {};

  return {
    ...cfg,
    namespace,
    namespace_source,
    recall: explicit.recall || typeof s.recall !== "boolean" ? cfg.recall : s.recall,
    capture: explicit.capture || typeof s.capture !== "boolean" ? cfg.capture : s.capture,
    recall_limit:
      explicit.recall_limit || !Number.isFinite(s.recall_limit) ? cfg.recall_limit : s.recall_limit,
    recall_max_tokens:
      explicit.recall_max_tokens || !Number.isFinite(s.inject_recall_max_tok)
        ? cfg.recall_max_tokens
        : s.inject_recall_max_tok,
    recall_min_score:
      explicit.recall_min_score || !Number.isFinite(s.inject_recall_min_score)
        ? cfg.recall_min_score
        : s.inject_recall_min_score,
  };
}

// memoizeAsync wraps a zero-arg async fn so a long-lived plugin instance calls
// it at most once per ttlMs, returning the cached value in between — the
// shape MeminiPlugin uses to memoize the handshake per session. `now` is
// injectable so tests can drive expiry without a real 10-minute sleep.
// Exported for testing.
export function memoizeAsync(fn, ttlMs, now = Date.now) {
  let cached = null; // { value, expiresAt }
  return async () => {
    const t = now();
    if (!cached || t >= cached.expiresAt) {
      const value = await fn();
      cached = { value, expiresAt: t + ttlMs };
    }
    return cached.value;
  };
}

// extractPartsText joins the text of a message's parts, skipping our injected
// recall context (synthetic) and ignored parts so captured turns hold only what
// the user wrote and what the assistant replied. Exported for testing.
export function extractPartsText(parts) {
  if (!Array.isArray(parts)) return "";
  return parts
    .filter((p) => p && p.type === "text" && p.synthetic !== true && p.ignored !== true)
    .map((p) => (typeof p.text === "string" ? p.text : ""))
    .join("\n")
    .trim();
}

// formatResults returns an array of bullet lines; the caller passes it to
// fitByTokens to apply a token ceiling, then joins + appends a footer.
//
// `labels` (optional) toggles the rich prefix: empty -> "- (tier) text" (the
// prior format, kept identical so snapshots don't break); non-empty ->
// "[tier · conf · age] text", same shape as the Claude Code plugin's
// formatMemory in plugin/scripts/session-start.mjs. Exported for testing.
export function formatResults(results, limit, labels) {
  if (!Array.isArray(results) || results.length === 0) return [];
  const useLabels = labels && labels.size > 0 ? labels : null;
  return results
    .slice(0, limit || DEFAULT_RECALL_LIMIT)
    .map((result, index) => {
      const mem = (result && result.memory) || {};
      const text = truncate(String(mem.summary || mem.content || `Memory ${index + 1}`).trim(), 300);
      if (!text) return null;
      const tier = String(mem.tier || "memory").trim();
      if (!useLabels) return `- (${tier}) ${text}`;
      const tagParts = [];
      if (useLabels.has("tier") && tier) tagParts.push(tier);
      if (useLabels.has("confidence") && typeof mem.confidence === "number") {
        tagParts.push(`conf=${mem.confidence.toFixed(2)}`);
      }
      if (useLabels.has("age") && mem.created_at) {
        const ageMs = Date.now() - new Date(mem.created_at).getTime();
        if (Number.isFinite(ageMs) && ageMs >= 0) {
          const days = Math.floor(ageMs / 86400000);
          tagParts.push(days === 0 ? "today" : `${days}d`);
        }
      }
      if (tagParts.length === 0) return `- (${tier}) ${text}`;
      return `[${tagParts.join(" · ")}] ${text}`;
    })
    .filter(Boolean);
}

function normalizedHostname(hostname) {
  return hostname.replace(/^\[|\]$/g, "").toLowerCase();
}

function usesPlaintextBearerAuth(baseUrl, secret) {
  if (!secret) return false;
  try {
    const parsed = new URL(baseUrl);
    return parsed.protocol === "http:" && !LOOPBACK_HOSTS.has(normalizedHostname(parsed.hostname));
  } catch {
    return false;
  }
}

function plaintextBearerAuthMessage(baseUrl) {
  return `memini: MEMINI_API_KEY is configured for plaintext HTTP to ${baseUrl}. Bearer tokens and memory payloads can be observed on the network; use HTTPS or an SSH tunnel.`;
}

// createPlaintextBearerAuthGuard refuses (MEMINI_REQUIRE_HTTPS=1) or warns once
// when a bearer token would be sent over plaintext HTTP to a non-loopback host.
// Exported for testing.
export function createPlaintextBearerAuthGuard(warn, env) {
  let warned = false;
  return function guardPlaintextBearerAuth(baseUrl, secret) {
    if (!usesPlaintextBearerAuth(baseUrl, secret)) return;
    const message = plaintextBearerAuthMessage(baseUrl);
    if ((env || process.env).MEMINI_REQUIRE_HTTPS === "1") throw new Error(message);
    if (!warned) {
      warned = true;
      warn(message);
    }
  };
}

// --- Injection budget ----------------------------------------------------
//
// Near-verbatim copies of plugin/scripts/_shared.mjs. The opencode plugin
// ships standalone on npm so it can't import across the tree; copy matches
// the precedent set by createPlaintextBearerAuthGuard above. Keep contracts
// identical when both sides change.

/**
 * intEnv parses a positive integer env var (>= 0) and returns `default` when
 * unset or malformed. A negative value also falls back — env values are user
 * input and shouldn't crash a hook.
 */
export function intEnv(name, defaultValue) {
  const raw = process.env[name];
  if (raw == null || raw === "") return defaultValue;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n < 0) return defaultValue;
  return n;
}

/**
 * floatEnv parses a non-negative float env var and returns `default` when
 * unset or malformed. Used for min_score.
 */
export function floatEnv(name, defaultValue) {
  const raw = process.env[name];
  if (raw == null || raw === "") return defaultValue;
  const n = Number.parseFloat(raw);
  if (!Number.isFinite(n) || n < 0) return defaultValue;
  return n;
}

/**
 * labelsEnv parses MEMINI_INJECT_LABELS into a Set of enabled labels.
 * Recognized: "tier", "confidence", "age", "reason". Empty/unset returns an
 * empty Set — the format helpers then skip every label.
 */
export function labelsEnv(name = "MEMINI_INJECT_LABELS") {
  const raw = process.env[name];
  if (!raw) return new Set();
  return new Set(
    raw
      .split(/[|,]/)
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean),
  );
}

/**
 * approxTokens is a cheap token estimator. ~0.75 tokens/word for English-ish
 * content, with a floor of 1 so a single non-empty line never reports 0.
 */
export function approxTokens(text) {
  if (!text) return 0;
  const words = String(text).trim().split(/\s+/).filter(Boolean).length;
  return Math.max(1, Math.ceil((words * 4) / 3));
}

/**
 * fitByTokens trims a list of pre-formatted strings to fit under `maxTokens`,
 * keeping the head (the most-relevant entries first). Returns the trimmed
 * list and the running token total, so callers can render a "[… truncated]"
 * footer when items were dropped.
 */
export function fitByTokens(items, maxTokens) {
  if (!Array.isArray(items) || items.length === 0) return { items: [], tokens: 0, dropped: 0 };
  if (!Number.isFinite(maxTokens) || maxTokens <= 0) {
    const tokens = items.reduce((sum, s) => sum + approxTokens(s), 0);
    return { items: items.slice(), tokens, dropped: 0 };
  }
  const out = [];
  let used = 0;
  let dropped = 0;
  for (const s of items) {
    const t = approxTokens(s);
    if (used + t > maxTokens) {
      dropped++;
      continue;
    }
    out.push(s);
    used += t;
  }
  return { items: out, tokens: used, dropped };
}

/**
 * Truncate to `max` bytes, suffix with a marker. Same shape as the Claude
 * Code plugin's truncate helper.
 */
export function truncate(value, max) {
  if (typeof value === "string") {
    return value.length > max ? value.slice(0, max) + "\n[...truncated]" : value;
  }
  if (value && typeof value === "object") {
    let str;
    try {
      str = JSON.stringify(value);
    } catch {
      return value;
    }
    return str.length > max ? str.slice(0, max) + "...[truncated]" : str;
  }
  return value;
}

// --- Status --------------------------------------------------------------
//
// "What is this plugin actually doing right now?" A list of values would not
// answer that. The case worth catching is MEMINI_NAMESPACE exported globally (a
// shell rc, or a fish universal variable), set once and forgotten, quietly
// collapsing every repo on the machine into one namespace: the value looks
// fine, only its provenance gives it away. So the namespace is resolved twice
// against progressively stripped inputs — as-is and without the env/option
// pin — and both are reported.

/**
 * Render a secret as a recognizable-but-useless fingerprint: enough to tell two
 * tokens apart, not enough to use. Short values are elided entirely rather than
 * half-revealed. Mirrors packages/memini-client's redactValue. Exported for
 * testing.
 */
export function redactSecret(value) {
  if (!value) return "";
  return value.length <= 12 ? "***" : `${value.slice(0, 3)}…${value.slice(-4)}`;
}

/**
 * Build the effective-settings report: the LOCAL namespace resolution (option
 * / env / worktree / default — no network call, so this never blocks), the
 * knobs with their provenance (secrets redacted), and the warnings. The
 * `memini_status` tool overlays the live, handshake-aware values (see
 * MeminiPlugin) on top of this report's namespace/memory sections before
 * rendering, so what the user reads reflects what the plugin actually did on
 * its last handshake — this function alone only ever reports the local view.
 * Exported for testing.
 */
export function describeSettings(env, options, worktree) {
  const e = env || {};
  const o = options || {};
  const dir = worktree || process.cwd();

  const cfg = resolveConfig(e, o, worktree);
  // What this project would resolve to without MEMINI_NAMESPACE / the
  // namespace option — the line that turns "your namespace is X" into "and
  // that's because of a global env pin" when the two disagree.
  const envSansPin = { ...e };
  delete envSansPin.MEMINI_NAMESPACE;
  const derived = resolveConfig(envSansPin, { ...o, namespace: undefined }, worktree);

  const secret = e.MEMINI_API_KEY || "";
  const warnings = [];

  const pin = String(e.MEMINI_NAMESPACE || "").trim();
  if (pin && derived.namespace && derived.namespace !== pin) {
    warnings.push({
      level: "warn",
      code: "global-namespace-pin",
      message:
        `MEMINI_NAMESPACE is set to "${pin}", which pins EVERY project on this machine to one ` +
        `namespace. This project would otherwise resolve to "${derived.namespace}". If it is ` +
        `exported from a shell rc (or a fish universal variable), every repo you work in is ` +
        `sharing one memory pool.`,
      fix: "Unset MEMINI_NAMESPACE and let each repo resolve on its own, or set the namespace option to scope one project deliberately.",
    });
  }

  if (usesPlaintextBearerAuth(cfg.base_url, secret)) {
    warnings.push({
      level: "warn",
      code: "plaintext-bearer",
      message: plaintextBearerAuthMessage(cfg.base_url),
      fix: "Use HTTPS, or tunnel over SSH. Set MEMINI_REQUIRE_HTTPS=1 to make this an error.",
    });
  }

  if (!cfg.home) {
    warnings.push({
      level: "note",
      code: "home-unset",
      message: "MEMINI_HOME is unset: no personal leg merges into recall.",
      fix: "Export MEMINI_HOME=personal/<you>.",
    });
  }

  return {
    project: resolve(dir),
    worktree: dir,
    namespace: {
      effective: cfg.namespace,
      source: cfg.namespace_source,
      derived,
      home: cfg.home,
    },
    connection: {
      base_url: cfg.base_url,
      api_key: secret ? redactSecret(secret) : "",
      require_https: e.MEMINI_REQUIRE_HTTPS === "1",
      timeout_ms: cfg.timeout_ms,
    },
    memory: {
      recall: cfg.recall,
      capture: cfg.capture,
      recall_limit: cfg.recall_limit,
      recall_max_tokens: cfg.recall_max_tokens,
      recall_min_score: cfg.recall_min_score,
      recall_budget_ms: cfg.recall_budget_ms,
      labels: [...labelsEnv()],
    },
    warnings,
  };
}

const padTo = (s, n) => String(s).padEnd(n);

/** Render describeSettings() (optionally overlaid with live values) as the text block the tool hands back. */
export function renderStatus(report) {
  const { namespace: ns, connection, memory } = report;
  const L = [];

  L.push("memini — effective settings (opencode)");
  L.push(`project: ${report.project}`);
  L.push("");

  L.push("NAMESPACE");
  L.push(`  ${padTo("effective", 26)} ${padTo(ns.effective, 30)} <- ${ns.source}`);
  if (ns.derived.namespace !== ns.effective) {
    L.push(
      `  ${padTo("git/cwd would give", 26)} ${padTo(ns.derived.namespace, 30)} <- ${ns.derived.source}`,
    );
  }
  L.push(`  ${padTo("home (personal)", 26)} ${ns.home || "(unset)"}`);
  L.push("");

  L.push("CONNECTION");
  L.push(`  ${padTo("base_url", 26)} ${connection.base_url}`);
  L.push(`  ${padTo("api_key", 26)} ${connection.api_key || "(unset)"}`);
  L.push(`  ${padTo("require_https", 26)} ${connection.require_https ? "1" : "0"}`);
  L.push(`  ${padTo("timeout_ms", 26)} ${connection.timeout_ms}`);
  L.push("");

  L.push("MEMORY");
  L.push(`  ${padTo("recall", 26)} ${memory.recall ? "on" : "off"}`);
  L.push(`  ${padTo("capture", 26)} ${memory.capture ? "on" : "off"}`);
  L.push(`  ${padTo("recall_limit", 26)} ${memory.recall_limit}`);
  L.push(`  ${padTo("recall_max_tokens", 26)} ${memory.recall_max_tokens || "uncapped"}`);
  L.push(`  ${padTo("recall_min_score", 26)} ${memory.recall_min_score}`);
  L.push(`  ${padTo("recall_budget_ms", 26)} ${memory.recall_budget_ms === 0 ? "0 (blocking)" : memory.recall_budget_ms}`);
  L.push(`  ${padTo("labels", 26)} ${memory.labels.length ? memory.labels.join(",") : "(none)"}`);
  L.push("");

  if (report.warnings.length) {
    L.push("WARNINGS");
    for (const w of report.warnings) {
      L.push(`  [${w.level === "warn" ? "!" : "i"}] ${w.code}: ${w.message}`);
      if (w.fix) L.push(`      fix: ${w.fix}`);
    }
  } else {
    L.push("No problems detected.");
  }

  return L.join("\n");
}

export function createClient(cfg, log) {
  const baseUrl = String(cfg.base_url).replace(/\/+$/, "");
  const secret = process.env.MEMINI_API_KEY;
  const guardPlaintextBearerAuth = createPlaintextBearerAuthGuard((m) => log.warn(m));
  if (process.env.MEMINI_REQUIRE_HTTPS === "1") guardPlaintextBearerAuth(baseUrl, secret);

  async function postJson(path, payload, namespace) {
    // Deliberate exception to the fail-soft try/catch below: a plaintext-bearer misconfiguration must raise, matching @memini/client's assertBearerTransportSafe.
    guardPlaintextBearerAuth(baseUrl, secret);
    const headers = { "Content-Type": "application/json", "X-Memini-Namespace": namespace };
    if (secret) headers.Authorization = `Bearer ${secret}`;
    if (cfg.home) headers["X-Memini-Home"] = cfg.home;
    try {
      const res = await fetch(`${baseUrl}${path}`, {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        signal: AbortSignal.timeout(cfg.timeout_ms),
      });
      if (!res.ok) {
        if (cfg.fallback_on_error) {
          // Degrade but never silently: a swallowed 401/500 on a capture or
          // recall looks like "memory isn't working" with nothing to debug.
          log.warn(`memini ${path} failed: ${res.status}`);
          return null;
        }
        const body = await res.text().catch(() => "");
        throw new Error(`memini ${path} failed: ${res.status} ${body}`);
      }
      return await res.json();
    } catch (error) {
      if (!cfg.fallback_on_error) throw error;
      log.warn(`memini: ${String(error)}`);
      return null;
    }
  }

  // handshake calls POST /v1/handshake (api/openapi.yaml) — fail-soft ALWAYS:
  // a network error, non-2xx, malformed JSON, or a ~2.5s timeout all return
  // null, independent of cfg.fallback_on_error (that knob is specifically
  // about degrading /v1/search and /v1/memories; degrading a handshake to
  // local resolution is not optional — see effectiveConfig()). Not memoized
  // here — the caller (MeminiPlugin) memoizes per plugin instance with a
  // 10-minute TTL via memoizeAsync.
  async function handshake(facts) {
    // Same deliberate exception as postJson: fail-soft ALWAYS above, except this one raise, matching @memini/client's assertBearerTransportSafe.
    guardPlaintextBearerAuth(baseUrl, secret);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), HANDSHAKE_TIMEOUT_MS);
    try {
      const headers = { "Content-Type": "application/json" };
      if (secret) headers.Authorization = `Bearer ${secret}`;
      if (cfg.home) headers["X-Memini-Home"] = cfg.home;
      const res = await fetch(`${baseUrl}/v1/handshake`, {
        method: "POST",
        headers,
        body: JSON.stringify({ project: facts, client: { name: CLIENT_NAME, version: CLIENT_VERSION } }),
        signal: controller.signal,
      });
      if (!res.ok) {
        log.warn(`memini handshake failed: ${res.status}`);
        return null;
      }
      return await res.json();
    } catch (error) {
      log.warn(`memini: handshake ${String(error)}`);
      return null;
    } finally {
      clearTimeout(timer);
    }
  }

  return { postJson, handshake, baseUrl };
}

// extractLastTurn returns the latest user and assistant text from the message
// list returned by client.session.messages ([{info, parts}, ...]), plus the id
// of the assistant message (for dedup). Iterates in reverse to short-circuit.
// Exported for testing.
export function extractLastTurn(messages) {
  let userText = "";
  let assistantText = "";
  let assistantID = "";
  if (!Array.isArray(messages)) return { userText, assistantText, assistantID };
  for (const entry of [...messages].reverse()) {
    const info = entry && entry.info;
    if (!info) continue;
    const text = extractPartsText(entry.parts);
    if (!text) continue;
    if (info.role === "user" && !userText) {
      userText = text;
    } else if (info.role === "assistant" && !assistantText) {
      assistantText = text;
      assistantID = info.id || "";
    }
    if (userText && assistantText) break;
  }
  return { userText, assistantText, assistantID };
}

// lastAssistantFailed reports whether the latest assistant turn errored, so the
// capture can flag it (the distiller mines failed→fixed turns into recovery).
// Exported for testing.
export function lastAssistantFailed(messages) {
  if (!Array.isArray(messages)) return false;
  for (const entry of [...messages].reverse()) {
    if (entry && entry.info && entry.info.role === "assistant") {
      return !!entry.info.error;
    }
  }
  return false;
}

export const MeminiPlugin = async ({ client, worktree, directory }, options) => {
  const log = {
    warn: (message) => {
      // client.app.log is opencode's structured logger; fall back to stderr.
      try {
        client?.app?.log?.({ body: { service: "memini", level: "warn", message } });
      } catch {
        /* ignore logging failures */
      }
      console.error(`[memini] ${message}`);
    },
  };

  const dir = worktree || directory;
  const cfg = resolveConfig(process.env, options, dir);
  const rest = createClient(cfg, log);

  // The handshake is memoized on THIS plugin instance (opencode creates one
  // per session) with a 10-minute TTL: long enough that a busy session isn't
  // round-tripping the network on every message, short enough that an
  // operator's pin/settings change is noticed within one long-lived session.
  // A null handshake (fail-soft, see createClient's handshake()) just means
  // effectiveConfig() falls all the way back to cfg's local resolution.
  const getHandshake = memoizeAsync(() => rest.handshake(buildFacts(dir, process.env)), HANDSHAKE_TTL_MS);
  const currentConfig = async () => effectiveConfig(cfg, await getHandshake());

  // Warm the connection (DNS/TCP/TLS) in opencode's embedded bun so a cold
  // start doesn't eat the first recall budget. Silent: even a 404 warms the
  // path, and an ingress that only routes /v1 legitimately has no /healthz.
  if (cfg.recall || cfg.capture) {
    try {
      fetch(`${rest.baseUrl}/healthz`, { signal: AbortSignal.timeout(3000) }).catch(() => {});
    } catch {
      /* ignore */
    }
  }
  // Assistant message ids already captured, so repeated session.idle events for
  // the same turn don't write duplicates. Repeats only concern recent turns,
  // so cap the window instead of growing one entry per turn forever.
  const captured = new Set();
  const MAX_CAPTURED = 200;
  const rememberCaptured = (id) => {
    captured.add(id);
    while (captured.size > MAX_CAPTURED) {
      const oldest = captured.values().next().value;
      if (oldest === undefined) break;
      captured.delete(oldest);
    }
  };
  // boundedPut inserts key -> value and evicts the oldest entries, so a
  // long-lived host can't grow a per-session map without limit.
  const MAX_TRACKED_SESSIONS = 200;
  const boundedPut = (map, key, value) => {
    map.set(key, value);
    while (map.size > MAX_TRACKED_SESSIONS) {
      const oldest = map.keys().next().value;
      if (oldest === undefined) break;
      map.delete(oldest);
    }
  };
  // Memory ids each session has already been shown (mirrors the pi plugin):
  // the injected synthetic part is persisted into the session, so re-injecting
  // an unchanged match every turn stacks identical blocks in the context.
  // The inner cap keeps a stable session — which never ages out of the outer
  // map — from growing its Set for the process lifetime.
  const injectedBySession = new Map();
  const MAX_INJECTED_PER_SESSION = 200;
  const rememberInjected = (session, ids) => {
    let seen = injectedBySession.get(session);
    if (!seen) {
      seen = new Set();
      boundedPut(injectedBySession, session, seen);
    }
    for (const id of ids) if (id) seen.add(id);
    while (seen.size > MAX_INJECTED_PER_SESSION) {
      const oldest = seen.values().next().value;
      if (oldest === undefined) break;
      seen.delete(oldest);
    }
  };
  // Recall results that arrived after the injection budget expired, keyed by
  // session and injected on that session's next chat.message. Latest-replace:
  // a second late recall for the same session supersedes the first.
  const pendingBySession = new Map();
  // /v1/search drops exclude_ids before ranking and the limit, so an
  // already-shown hit frees its slot for the next-best match. Older servers
  // 400 on the unknown field: when a request carrying it fails and the retry
  // without it succeeds, stop sending it. The client-side filter stays.
  let serverExcludeIds = true;
  const searchExcluding = async (body, excludeIds, namespace) => {
    if (!serverExcludeIds || excludeIds.length === 0) {
      return rest.postJson("/v1/search", body, namespace);
    }
    try {
      const result = await rest.postJson("/v1/search", { ...body, exclude_ids: excludeIds }, namespace);
      if (result !== null) return result;
    } catch {
      // With fallback_on_error=false the 400 arrives as a throw, not null.
    }
    const retry = await rest.postJson("/v1/search", body, namespace);
    if (retry !== null) {
      serverExcludeIds = false;
      log.warn("memini: server does not accept exclude_ids; using client-side dedupe only");
    }
    return retry;
  };

  // opencode runs chat.message via an unguarded Effect.promise (a throw aborts the
  // turn) and dispatches event hooks fire-and-forget, so a hook must never reject:
  // swallow and log instead.
  const guard = (name, fn) => async (...args) => {
    try {
      return await fn(...args);
    } catch (error) {
      log.warn(`${name} hook failed: ${String(error)}`);
    }
  };

  return {
    // A tool, not a slash command, and deliberately so. opencode's plugin
    // contract (Hooks in @opencode-ai/plugin) registers tools — `tool: { [id]:
    // { description, args, execute } }` — but exposes no hook for registering a
    // user-invocable command, so there is no `/memini:status` this plugin could
    // offer without inventing one. `args` is empty: declaring a parameter means
    // handing opencode a zod schema, and this plugin has no dependencies (a
    // raw-JSON-Schema arg rides a compatibility path that older hosts feed
    // straight to z.object() and throw on). A zero-arg tool is the shape every
    // version accepts, and a read-only report needs no arguments anyway.
    tool: {
      memini_status: {
        description:
          "Show the memini memory settings in force for this project: which namespace memories " +
          "are written to and recalled from, where that namespace came from (the namespace option, " +
          "MEMINI_NAMESPACE, a server-resolved handshake, or the git worktree fallback), what it " +
          "would be without the env/option pin, and any misconfiguration worth flagging. Read-only; " +
          "secrets are redacted. Call it when the user asks what memini is doing, why a memory " +
          "cannot be recalled, or which namespace is in use.",
        args: {},
        execute: async () => {
          try {
            const report = describeSettings(process.env, options, dir);
            // Overlay the live, handshake-aware values on top of the local
            // report so what the tool reports matches what the hooks
            // actually did on their last handshake.
            const live = await currentConfig();
            report.namespace.effective = live.namespace;
            report.namespace.source = live.namespace_source;
            report.memory.recall = live.recall;
            report.memory.capture = live.capture;
            report.memory.recall_limit = live.recall_limit;
            report.memory.recall_max_tokens = live.recall_max_tokens;
            report.memory.recall_min_score = live.recall_min_score;
            return {
              title: `memini: ${report.namespace.effective}`,
              output: renderStatus(report),
              metadata: { namespace: report.namespace.effective, source: report.namespace.source },
            };
          } catch (error) {
            // A diagnostic that crashes the turn it was meant to diagnose is
            // worse than no diagnostic.
            return `memini status failed: ${String(error)}`;
          }
        },
      },
    },

    "chat.message": guard("chat.message", async (input, output) => {
      const live = await currentConfig();
      if (!live.recall) return;
      const query = extractPartsText(output && output.parts);
      if (!query) return;
      // Borrow sessionID/messageID from the real parts when the hook input
      // omits them (messageID is optional in the contract), so the injected
      // part is attributed to the same message.
      const sibling = output.parts.find((p) => p && p.type === "text") || {};
      const sessionID = input.sessionID || sibling.sessionID;
      const messageID = input.messageID || sibling.messageID;
      const body = { query, limit: live.recall_limit };
      // Exclude this session's own captured turns: they're still in the live
      // context, so recalling them just echoes the conversation back a turn
      // behind. Captures from other (past) sessions are still recalled.
      if (sessionID) body.exclude_metadata = { session_id: sessionID };
      // min_score (fused-score floor) is optional and matches the wire knob
      // the Claude Code plugin's pre-tool-use hook uses; client-side re-filter
      // is a belt-and-braces guard against score-normalization edge cases.
      if (live.recall_min_score > 0) body.min_score = live.recall_min_score;
      // Already-shown ids go along as exclude_ids so a suppressed hit doesn't
      // waste a recall_limit slot.
      const excludeIds = sessionID ? [...(injectedBySession.get(sessionID) ?? [])] : [];
      // opencode awaits this hook before the model sees the message, so the
      // turn only waits live.recall_budget_ms for the search; the fetch itself keeps
      // cfg.timeout_ms as its bound and runs on in the background. A slow or
      // unreachable memini degrades to "no memories this turn" instead of a
      // frozen turn, and late results carry over to the session's next message.
      const fetchPromise = searchExcluding(body, excludeIds, live.namespace);
      // Once the budget expires nothing awaits this promise, and with
      // fallback_on_error off postJson rethrows — catch here or a late
      // rejection surfaces as an unhandled rejection in the host.
      const settled = fetchPromise.catch((error) => {
        log.warn(`memini: ${String(error)}`);
        return null;
      });
      let result;
      if (live.recall_budget_ms > 0) {
        let timer;
        const budget = new Promise((resolve) => {
          timer = setTimeout(() => resolve(BUDGET_EXPIRED), live.recall_budget_ms);
        });
        result = await Promise.race([settled, budget]);
        clearTimeout(timer);
        if (result === BUDGET_EXPIRED) {
          log.warn(
            `recall exceeded its ${live.recall_budget_ms}ms budget; late results will inject next turn`,
          );
          if (sessionID) {
            settled.then((late) => {
              const hits = Array.isArray(late && late.results) ? late.results : [];
              if (hits.length) boundedPut(pendingBySession, sessionID, hits);
            });
          }
          result = null;
        }
      } else {
        result = await settled;
      }
      // Client-side score floor: filter the raw hit list before formatting so
      // the bullet array only contains hits the operator asked for. Without
      // this, the server's default floor could leak low-quality hits in
      // regardless of live.recall_min_score.
      const floor = live.recall_min_score > 0 ? live.recall_min_score : 0;
      let rawHits = Array.isArray(result && result.results) ? result.results : [];
      // Merge in results that arrived late on a previous turn: fresh hits
      // first (they answer the current query), deduped by memory id.
      if (sessionID) {
        const pending = pendingBySession.get(sessionID);
        if (pending && pending.length) {
          pendingBySession.delete(sessionID);
          const fresh = new Set(rawHits.map((r) => r?.memory?.id).filter(Boolean));
          rawHits = rawHits.concat(pending.filter((r) => !fresh.has(r?.memory?.id)));
        }
      }
      // Suppress memories this session has already been shown — the injected
      // part persists in the session, so a repeat adds nothing but noise.
      if (sessionID) {
        const seen = injectedBySession.get(sessionID);
        if (seen && seen.size) rawHits = rawHits.filter((r) => !seen.has(r?.memory?.id));
      }
      const filtered = floor > 0
        ? rawHits.filter((r) => (typeof r?.score === "number" ? r.score : 0) >= floor)
        : rawHits;
      const labels = labelsEnv();
      const hits = formatResults(filtered, live.recall_limit, labels);
      if (hits.length === 0) return;
      // Apply the token ceiling to the rendered bullet lines; with max=0
      // (the default) fitByTokens returns the full list unchanged, so the
      // behaviour matches the prior "no cap" code path for existing installs.
      const fit = fitByTokens(hits, live.recall_max_tokens);
      if (fit.items.length === 0) return;
      if (sessionID) {
        // Mark only the slice formatResults actually renders: with carryover
        // merged in, `filtered` can exceed recall_limit, and marking unshown
        // hits as seen would suppress them forever.
        rememberInjected(
          sessionID,
          filtered
            .slice(0, live.recall_limit || DEFAULT_RECALL_LIMIT)
            .map((r) => r?.memory?.id)
            .filter(Boolean),
        );
      }
      const lines = [
        `Relevant long-term memory from memini (background context — prefer ` +
          `current workspace state and the user's instructions):`,
        ...fit.items,
      ];
      // /v1/search sets `degraded: "keyword_only"` (plus a `note`) when the
      // query embed was unavailable and it fell back to keyword-only matching;
      // both are already on `result`, so surfacing them is a one-line addition.
      if (result && result.degraded) {
        lines.push(`[memini: ${result.note || "semantic search unavailable — results are keyword-only and may be incomplete"}]`);
      }
      if (fit.dropped > 0) lines.push(`[... ${fit.dropped} item(s) truncated by token budget]`);
      // opencode's part schema requires ids to start with `prt`.
      output.parts.unshift({
        id: `prt_${crypto.randomUUID()}`,
        sessionID,
        messageID,
        type: "text",
        synthetic: true,
        text: lines.join("\n"),
      });
    }),

    event: guard("event", async ({ event }) => {
      // Auto-update check: fires once per process on the first session.created
      if (
        !autoUpdateChecked &&
        event &&
        event.type === "session.created" &&
        !event.properties?.info?.parentID // skip sub-sessions
      ) {
        autoUpdateChecked = true;
        const live = await currentConfig();
        if (live.auto_update) {
          // Fire-and-forget — never blocks the event hook
          (async () => {
            try {
              const latest = await fetchLatestVersion();
              if (!latest) return;
              if (compareVersions(CLIENT_VERSION, latest) >= 0) return; // already up to date
              // Only auto-update within the same major version
              const cur = parseVersion(CLIENT_VERSION);
              const nxt = parseVersion(latest);
              if (!cur || !nxt || cur.major !== nxt.major) {
                log.warn(`auto-update: v${latest} available (major bump — update manually: pin @eleboucher/opencode-memini@${latest} in opencode.json)`);
                return;
              }
              log.warn(`auto-update: updating ${CLIENT_VERSION} → ${latest}`);
              const installDir = prepareCacheUpdate(latest, log);
              if (!installDir) return;
              const ok = runBunInstall(installDir);
              if (ok) {
                log.warn(`auto-update: installed v${latest} — restart opencode to apply`);
              } else {
                log.warn(`auto-update: bun install failed; will retry next session`);
              }
            } catch (err) {
              log.warn(`auto-update: check failed: ${String(err)}`);
            }
          })();
        }
        return;
      }

      const live = await currentConfig();
      if (!live.capture || !event || event.type !== "session.idle") return;
      const sessionID = event.properties && event.properties.sessionID;
      if (!sessionID) return;
      const res = await client.session.messages({ path: { id: sessionID } });
      const { userText, assistantText, assistantID } = extractLastTurn(res && res.data);
      if (!userText || !assistantText) return;
      if (assistantID && captured.has(assistantID)) return;
      const metadata = { source: "opencode", session_id: sessionID, format: "turn" };
      if (lastAssistantFailed(res && res.data)) metadata.failed = true;
      const stored = await rest.postJson(
        "/v1/memories",
        {
          content: `${userText.slice(0, 1000)}\n\n${assistantText.slice(0, 3000)}`,
          tags: ["opencode"],
          metadata,
        },
        live.namespace,
      );
      if (stored !== null && assistantID) rememberCaptured(assistantID);
    }),
  };
};

export default { id: "memini", server: MeminiPlugin };
