/**
 * @memini/namespace-resolver
 *
 * Shared namespace resolution for memini integrations.
 *
 * Resolution chain (mirrors today's per-integration logic, generalized):
 *   1. MEMINI_NAMESPACE env → wins immediately (backward compat)
 *   2. Config file tenant roots → {tenant} segment
 *   3. Git remote > git toplevel basename > cwd basename → {project} segment
 *   4. agentId / MEMINI_AGENT env → {agent} segment
 *   5. Per-integration override namespace → {namespace} segment (OpenClaw pattern)
 *
 * Template engine drops unresolvable segments with slash collapse:
 *   "{tenant}/{project}/{agent}" with no agent → "work/memini"
 *
 * Config file: ~/.config/memini/config.json (JSON, zero-dep)
 * If absent or malformed → empty config → today's exact behavior.
 */

import { execSync } from "node:child_process";
import { basename } from "node:path";
import { homedir } from "node:os";
import fs from "node:fs";
import path from "node:path";

// ─── Types ───────────────────────────────────────────────────────────

export interface TenantRoot {
  /** Filesystem path, supports ~ expansion. e.g. "~/dev/work" */
  path: string;
  /** Namespace segment for this root. e.g. "work" */
  tenant: string;
}

export interface IntegrationOverride {
  /** Override the default template for this integration. */
  template?: string;
  /** Static base namespace for integrations without filesystem (OpenClaw). */
  namespace?: string;
}

export interface NamespaceConfig {
  tenantRoots: TenantRoot[];
  /** Default template for all integrations. Default: "{tenant}/{project}/{agent}" */
  template: string;
  /** Per-integration overrides keyed by integration name. */
  overrides: Record<string, IntegrationOverride>;
  /**
   * True when a config file was found and parsed. False (no file / unreadable /
   * malformed JSON) makes resolveNamespace reproduce the pre-config behavior
   * exactly — templates and agent segments only apply once a user opts in by
   * creating the file.
   */
  found: boolean;
}

export interface ResolveOptions {
  /** Working directory (required for path-aware resolution). */
  cwd: string;
  /** Environment variables (defaults to process.env). */
  env?: Record<string, string>;
  /** Pre-resolved git remote URL. If omitted, resolver tries `git remote get-url origin`. */
  gitRemoteUrl?: string;
  /** Agent identity (from OpenClaw's ctx.agentId, etc). */
  agentId?: string;
  /** Path to config file (default: ~/.config/memini/config.json). */
  configPath?: string;
  /** Integration name for per-integration override lookup (e.g. "openclaw"). */
  integration?: string;
}

export interface ResolvedSegments {
  tenant?: string;
  project?: string;
  agent?: string;
  namespace?: string;
}

export interface ResolveResult {
  /** Final resolved namespace string. */
  namespace: string;
  /** Per-segment breakdown for diagnostics / doctor. */
  segments: ResolvedSegments;
  /** Which precedence level produced the result. */
  source: "env" | "config" | "git" | "cwd" | "default";
  /**
   * The caller's personal namespace (MEMINI_HOME), resolved independently of
   * `namespace`/`source` above. Env-only — mirrors the MEMINI_NAMESPACE
   * precedence (wins immediately, no config-file involvement). undefined
   * when MEMINI_HOME is unset or blank, meaning "no home leg": the caller
   * should omit X-Memini-Home entirely rather than send it empty.
   */
  home?: string;
  /** Which precedence level produced `home`. Only ever "env" (unset) today —
   * kept as its own field (rather than reusing `source`) since home and
   * namespace resolve independently and a future home source (e.g. a config
   * default) shouldn't collide with the namespace source. */
  homeSource?: "env";
}

// ─── Constants ──────────────────────────────────────────────────────

export const DEFAULT_TEMPLATE = "{tenant}/{project}/{agent}";

// ─── Config ─────────────────────────────────────────────────────────

/**
 * Default config file path: $XDG_CONFIG_HOME/memini/config.json,
 * or ~/.config/memini/config.json if XDG_CONFIG_HOME is unset.
 */
export function defaultConfigPath(): string {
  const xdg = process.env["XDG_CONFIG_HOME"];
  const base = xdg && xdg.trim() ? xdg : path.join(homedir(), ".config");
  return path.join(base, "memini", "config.json");
}

/**
 * Read and parse the namespace config file.
 * Returns an empty/default config on any error (missing file, bad JSON, etc.)
 * so callers always get a usable object — this is the backward-compat boundary.
 */
export function readConfig(configPath?: string): NamespaceConfig {
  const p = configPath || defaultConfigPath();
  try {
    const raw = fs.readFileSync(p, "utf8");
    const parsed = JSON.parse(raw);
    return {
      tenantRoots: Array.isArray(parsed.tenantRoots) ? parsed.tenantRoots : [],
      template: typeof parsed.template === "string" ? parsed.template : DEFAULT_TEMPLATE,
      overrides:
        parsed.overrides && typeof parsed.overrides === "object" ? parsed.overrides : {},
      found: true,
    };
  } catch {
    // File missing or malformed — backward compat: empty config, today's behavior
    return { tenantRoots: [], template: DEFAULT_TEMPLATE, overrides: {}, found: false };
  }
}

// ─── Git helpers ───────────────────────────────────────────────────

/**
 * Run a git command in dir, returning trimmed stdout or "" on any error.
 * Best-effort — never throws. Mirrors _shared.mjs gitOut behavior.
 */
function gitOut(args: string, dir: string): string {
  try {
    return execSync(`git ${args}`, {
      cwd: dir,
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 500,
    })
      .toString()
      .trim();
  } catch {
    return "";
  }
}

/**
 * Extract the repo name (last path segment) from a git remote URL.
 * Handles ssh://, https://, and scp-style URLs; strips trailing .git.
 * Returns null on parse failure.
 */
export function repoNameFromRemote(url: string): string | null {
  if (typeof url !== "string" || !url) return null;
  const cleaned = url.trim().replace(/\/+$/, "").replace(/\.git$/i, "");
  if (!cleaned) return null;
  const scpMatch = cleaned.match(/^[^/:]+:[^/]/);
  const p = scpMatch ? cleaned.slice(scpMatch[0].indexOf(":") + 1) : cleaned;
  const segs = p.split("/").filter(Boolean);
  return segs.length ? segs[segs.length - 1] : null;
}

/**
 * Extract an "owner-repo" slug from a git remote URL (last two segments
 * joined with a dash), so same-named repos under different owners don't
 * collide. Falls back to the bare repo name for single-segment remotes.
 */
export function repoSlugFromRemote(url: string): string | null {
  if (typeof url !== "string" || !url) return null;
  const cleaned = url.trim().replace(/\/+$/, "").replace(/\.git$/i, "");
  if (!cleaned) return null;
  const scpMatch = cleaned.match(/^[^/:]+:[^/]/);
  const p = scpMatch ? cleaned.slice(scpMatch[0].indexOf(":") + 1) : cleaned;
  const segs = p.split("/").filter(Boolean);
  if (!segs.length) return null;
  if (segs.length === 1) return segs[0];
  const owner = segs[segs.length - 2].replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  const repo = segs[segs.length - 1];
  return owner ? `${owner}-${repo}` : repo;
}

// ─── Helpers ────────────────────────────────────────────────────────

/** Expand a leading ~ to the home directory. */
function expandTilde(p: string): string {
  if (p === "~") return homedir();
  if (p.startsWith("~/")) return path.join(homedir(), p.slice(2));
  return p;
}

/** Sanitize a namespace segment: keep alnum, dot, dash, underscore, slash. */
function sanitizeSegment(s: string): string {
  return s
    .replace(/[^A-Za-z0-9._/-]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// ─── Segment resolution ─────────────────────────────────────────────

/**
 * Resolve the {tenant} segment by matching cwd against configured tenant roots.
 * Returns the tenant name if cwd is under a mapped root, undefined otherwise.
 */
function resolveTenant(cwd: string, config: NamespaceConfig): string | undefined {
  const resolvedCwd = path.resolve(cwd);
  for (const root of config.tenantRoots) {
    if (!root || typeof root !== "object") continue;
    // An empty/missing path would resolve to the process cwd and match it; skip.
    if (typeof root.path !== "string" || !root.path) continue;
    const rootPath = path.resolve(expandTilde(root.path));
    if (resolvedCwd === rootPath || resolvedCwd.startsWith(rootPath + path.sep)) {
      const t = sanitizeSegment(String(root.tenant || ""));
      if (t) return t;
    }
  }
  return undefined;
}

/**
 * Resolve the {project} segment.
 * Priority: explicit gitRemoteUrl > git remote get-url origin > git toplevel basename > cwd basename.
 */
function resolveProject(cwd: string, gitRemoteUrl?: string, env?: Record<string, string>): string {
  // owner-repo scope (mirrors _shared.mjs MEMINI_NAMESPACE_SCOPE)
  const ownerRepo = (env?.["MEMINI_NAMESPACE_SCOPE"] || "").trim() === "owner-repo";

  // Try pre-resolved remote first, then git command
  let remote = gitRemoteUrl;
  if (!remote) {
    remote = gitOut("remote get-url origin", cwd);
  }

  if (remote) {
    const name = ownerRepo ? repoSlugFromRemote(remote) : repoNameFromRemote(remote);
    if (name) return sanitizeSegment(name);
  }

  const toplevel = gitOut("rev-parse --show-toplevel", cwd);
  if (toplevel) return sanitizeSegment(basename(toplevel));

  const b = basename(cwd);
  return sanitizeSegment(b) || "default";
}

/**
 * Resolve the {agent} segment from opts.agentId or MEMINI_AGENT env.
 */
function resolveAgent(opts: ResolveOptions): string | undefined {
  const agent = (opts.agentId || opts.env?.["MEMINI_AGENT"] || "").trim();
  if (!agent) return undefined;
  return sanitizeSegment(agent);
}

/**
 * Resolve the caller's personal namespace (MEMINI_HOME). Env-only, mirroring
 * the MEMINI_NAMESPACE precedence: no config-file involvement, no git/cwd
 * fallback. Returns undefined when unset/blank — "no home leg" — rather than
 * a sanitized empty string, so callers can tell "unset" from "resolved to
 * empty" without an extra check.
 */
function resolveHome(env: Record<string, string>): string | undefined {
  const home = (env["MEMINI_HOME"] || "").trim();
  return home || undefined;
}

// ─── Template engine ───────────────────────────────────────────────

/**
 * Apply a template by substituting {segment} placeholders with resolved values.
 * Unresolvable segments are replaced with empty string, then orphaned slashes
 * are collapsed.
 *
 * Examples:
 *   "{tenant}/{project}/{agent}", {tenant:"work", project:"memini"} → "work/memini"
 *   "{namespace}/{agent}", {namespace:"work/openclaw", agent:"miso"} → "work/openclaw/miso"
 *   "{namespace}-{agent}", {namespace:"openclaw", agent:"miso"} → "openclaw-miso"
 *   "{tenant}/{project}", {project:"memini"} → "memini"
 */
export function applyTemplate(
  template: string,
  segments: ResolvedSegments,
): string {
  const all: Record<string, string | undefined> = {
    tenant: segments.tenant,
    project: segments.project,
    agent: segments.agent,
    namespace: segments.namespace,
  };

  // Replace each {segment} with its value, or empty if unset
  let result = template.replace(/\{(tenant|project|agent|namespace)\}/g, (_, key: string) => {
    return all[key] ?? "";
  });

  // Collapse consecutive slashes from dropped segments
  result = result.replace(/\/{2,}/g, "/");

  // Trim leading/trailing slashes
  result = result.replace(/^\/+|\/+$/g, "");

  return result;
}

// ─── Main resolver ──────────────────────────────────────────────────

/**
 * Resolve a namespace for a memini integration.
 *
 * Full resolution chain:
 *   1. MEMINI_NAMESPACE env → wins immediately (unchanged from today)
 *   2. Config file → tenant roots, template, per-integration overrides
 *   3. Segment resolution:
 *      - {tenant}: cwd → tenant root match
 *      - {project}: gitRemoteUrl or git remote > toplevel basename > cwd basename
 *      - {agent}: opts.agentId or MEMINI_AGENT env
 *      - {namespace}: per-integration override namespace (OpenClaw pattern)
 *   4. Template selection: override.template > config.template
 *   5. applyTemplate with segment dropping
 *   6. Fallback: if template produces empty, use project or "default"
 *
 * When no config file exists, behavior is identical to today's per-integration
 * resolution. This is the backward-compat guarantee.
 */
export function resolveNamespace(opts: ResolveOptions): ResolveResult {
  const env = opts.env || process.env as Record<string, string>;
  const cwd = opts.cwd && opts.cwd.trim() ? opts.cwd : process.cwd();

  // home resolves independently of namespace/source — env-only, no
  // config-file involvement — so it's computed once and merged into every
  // return path below.
  const home = resolveHome(env);
  const homeSource = home ? ("env" as const) : undefined;

  // 1. MEMINI_NAMESPACE env wins immediately — unchanged from today
  const nsEnv = (env["MEMINI_NAMESPACE"] || "").trim();
  if (nsEnv) {
    return { namespace: nsEnv, segments: {}, source: "env", home, homeSource };
  }

  // 2. Read config. No config file = today's exact behavior: the namespace is
  //    the cwd basename (the pre-resolver Pi derivation) — no tenant, no
  //    template, no agent segment, no git preference. Zero migration.
  const config = readConfig(opts.configPath);
  if (!config.found) {
    const project = sanitizeSegment(basename(cwd));
    return {
      namespace: project,
      segments: project ? { project } : {},
      source: "cwd",
      home,
      homeSource,
    };
  }

  // 3. Resolve segments
  const tenant = resolveTenant(cwd, config);
  const project = resolveProject(cwd, opts.gitRemoteUrl, env);
  const agent = resolveAgent(opts);

  // 4. Pick template and base namespace (per-integration override > default)
  const override = opts.integration ? config.overrides[opts.integration] : undefined;
  const baseNamespace = override?.namespace;

  // 5. Build segments for template
  const segments: ResolvedSegments = { tenant, project, agent, namespace: baseNamespace };

  // 6. Apply template
  const template = override?.template || config.template;
  let namespace = applyTemplate(template, segments);

  // 7. If template produced empty (all segments unresolved), fall back
  if (!namespace) {
    namespace = project || "default";
  }

  // 8. Determine source for diagnostics
  let source: ResolveResult["source"] = "default";
  if (tenant) source = "config";
  else if (opts.gitRemoteUrl || gitOut("remote get-url origin", cwd)) source = "git";
  else source = "cwd";

  return { namespace, segments, source, home, homeSource };
}
