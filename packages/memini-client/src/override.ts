/**
 * Per-project namespace override — READ path only.
 *
 * Pre-config-handshake, this was the one namespace input a *user* set
 * deliberately (winning over everything else, including MEMINI_NAMESPACE) via
 * a per-machine overrides.json under $XDG_CONFIG_HOME. Every TypeScript
 * integration has since moved its namespace command onto server-side pins
 * (POST/DELETE /v1/pins) instead, so the WRITE path (writeOverride/
 * clearOverride) was deleted once pi/openclaw — its last callers — stopped
 * using it.
 *
 * The READ path stays: Phase 9's migration still needs readOverride to detect
 * a pre-existing override left behind by an older install and migrate it
 * (e.g. into an equivalent server-side pin) rather than silently orphaning it.
 */

import { execSync } from "node:child_process";
import { homedir } from "node:os";
import fs from "node:fs";
import path from "node:path";

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

// writeOverride/clearOverride (the WRITE path) were removed once pi/openclaw
// — their last callers — moved their namespace commands onto server-side
// pins (POST/DELETE /v1/pins). The READ path above stays: Phase 9's migration
// still needs readOverride to detect and migrate a pre-existing override.
