/**
 * Per-project namespace override.
 *
 * The override is the one namespace input a *user* sets deliberately, so it
 * wins over everything else — including MEMINI_NAMESPACE. That ordering is not
 * an accident: a globally exported MEMINI_NAMESPACE (a shell rc, or worse a
 * fish universal variable) pins every repo on the machine to one namespace, and
 * if the env beat the override then `memini namespace <ns>` would silently do
 * nothing on exactly the machines that need it most.
 *
 * Stored under $XDG_CONFIG_HOME rather than $XDG_CACHE_HOME: it is user intent,
 * not derived state, and clearing a cache must never silently discard it.
 */

import { execSync } from "node:child_process";
import { homedir } from "node:os";
import fs from "node:fs";
import path from "node:path";

import { normalizeNamespace, validateNamespace } from "./namespace-validate.js";

export interface NamespaceOverride {
  namespace: string;
  /** RFC3339. Purely informational — shown by `status` so a forgotten override is obvious. */
  setAt: string;
}

export interface OverridesFile {
  version: number;
  overrides: Record<string, NamespaceOverride>;
}

export const OVERRIDES_VERSION = 1;

const EMPTY: OverridesFile = { version: OVERRIDES_VERSION, overrides: {} };

export interface OverrideOptions {
  env?: Record<string, string | undefined>;
  /** Explicit file path (tests). */
  overridesPath?: string;
}

/** $XDG_CONFIG_HOME/memini/overrides.json, else ~/.config/memini/overrides.json. */
export function defaultOverridesPath(env: Record<string, string | undefined> = process.env): string {
  const xdg = env["XDG_CONFIG_HOME"];
  const base = xdg && xdg.trim() ? xdg : path.join(homedir(), ".config");
  return path.join(base, "memini", "overrides.json");
}

/**
 * The key an override is stored under: the git toplevel when there is one, else
 * the resolved cwd. Keying on the repo root (not the raw cwd) means an override
 * set at the top of a repo still applies when the agent is working three
 * directories down.
 */
export function overrideKey(cwd: string): string {
  const dir = cwd && cwd.trim() ? cwd : process.cwd();
  try {
    const top = execSync("git rev-parse --show-toplevel", {
      cwd: dir,
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 500,
    })
      .toString()
      .trim();
    if (top) return path.resolve(top);
  } catch {
    // not a repo, or no git — fall through to the plain path
  }
  return path.resolve(dir);
}

/** Read the overrides file. Any error yields an empty set — never throws. */
export function readOverrides(opts: OverrideOptions = {}): OverridesFile {
  const p = opts.overridesPath || defaultOverridesPath(opts.env);
  try {
    const parsed = JSON.parse(fs.readFileSync(p, "utf8"));
    if (!parsed || typeof parsed !== "object" || typeof parsed.overrides !== "object") {
      return { ...EMPTY, overrides: {} };
    }
    return {
      version: typeof parsed.version === "number" ? parsed.version : OVERRIDES_VERSION,
      overrides: parsed.overrides || {},
    };
  } catch {
    return { ...EMPTY, overrides: {} };
  }
}

/**
 * The override in effect for `cwd`, or undefined.
 *
 * Reads the file BEFORE computing the key, because computing the key shells out
 * to `git rev-parse` and this runs on every hook invocation — including
 * PreToolUse, which fires once per file touched. Nobody should pay for a git
 * call to discover that they have no overrides at all, which is the common case.
 */
export function readOverride(cwd: string, opts: OverrideOptions = {}): NamespaceOverride | undefined {
  const file = readOverrides(opts);
  const keys = Object.keys(file.overrides);
  if (keys.length === 0) return undefined;

  const entry = file.overrides[overrideKey(cwd)];
  if (!entry || typeof entry.namespace !== "string" || !entry.namespace.trim()) return undefined;
  return entry;
}

/**
 * Set the override for `cwd`. Throws on an invalid namespace — this is a direct
 * user action, so a bad value should fail loudly here rather than be silently
 * normalized into something they did not ask for, or worse, be accepted and
 * then rejected by the server on every later call.
 */
export function writeOverride(
  cwd: string,
  namespace: string,
  opts: OverrideOptions & { now?: () => Date } = {},
): NamespaceOverride {
  const ns = normalizeNamespace(namespace);
  const bad = validateNamespace(ns);
  if (bad) throw new Error(`invalid namespace ${JSON.stringify(namespace)}: ${bad}`);

  const p = opts.overridesPath || defaultOverridesPath(opts.env);
  const file = readOverrides(opts);
  const entry: NamespaceOverride = {
    namespace: ns,
    setAt: (opts.now ? opts.now() : new Date()).toISOString(),
  };
  file.version = OVERRIDES_VERSION;
  file.overrides[overrideKey(cwd)] = entry;

  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, JSON.stringify(file, null, 2) + "\n");
  return entry;
}

/** Remove the override for `cwd`. Returns true when one was actually removed. */
export function clearOverride(cwd: string, opts: OverrideOptions = {}): boolean {
  const p = opts.overridesPath || defaultOverridesPath(opts.env);
  const file = readOverrides(opts);
  const key = overrideKey(cwd);
  if (!(key in file.overrides)) return false;
  delete file.overrides[key];
  try {
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, JSON.stringify(file, null, 2) + "\n");
  } catch {
    return false;
  }
  return true;
}
