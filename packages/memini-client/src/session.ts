/**
 * Recovering the *project directory* in processes the harness starts without one.
 *
 * The problem this solves is specific and load-bearing. Claude Code's MCP
 * `headersHelper` is the only thing that sets `X-Memini-Namespace` for MCP tool
 * calls, and it is handed almost nothing to work with. Measured, in a live
 * session (Claude Code 2.x, plugin 0.6.7):
 *
 *   cwd                : <plugin install root>
 *   PWD                : <plugin install root>   <- rewritten; NOT the project
 *   CLAUDE_PROJECT_DIR : unset
 *   CLAUDE_PLUGIN_ROOT : <plugin install root>
 *   process.ppid       : the session's `claude` process, cwd = the project dir
 *
 * So the helper historically fell back to a single global file that the
 * SessionStart hook wrote — and with two sessions open in two repos, that file
 * is last-writer-wins. Both sessions' MCP calls then target one namespace while
 * their hooks write to another: the exact "writes land where recall doesn't
 * look" failure `memini doctor` exists to diagnose.
 *
 * The parent process is the fix. The helper's parent *is* the session, and its
 * cwd *is* the project dir. Hooks reach the same ppid (Claude Code runs them via
 * `sh -c '…run.sh script'`, and run.sh `exec`s node, which preserves the
 * parent), so both sides key off the same value without coordinating.
 *
 * Everything here returns a DIRECTORY, never a namespace. Callers re-resolve the
 * namespace from it on every use, which is what keeps a namespace override
 * authoritative — a cached namespace would go stale the moment someone set one.
 */

import { execFileSync } from "node:child_process";
import { homedir, tmpdir } from "node:os";
import fs from "node:fs";
import path from "node:path";

export type CwdSource =
  | "CLAUDE_PROJECT_DIR"
  | "parent-process"
  | "session-file"
  | "none";

export interface HarnessCwd {
  cwd: string;
  source: CwdSource;
}

/** $XDG_CACHE_HOME/memini, else ~/.cache/memini. Matches the hooks' cache dir. */
export function cacheDir(env: Record<string, string | undefined> = process.env): string {
  const base = env["XDG_CACHE_HOME"] || path.join(homedir() || tmpdir(), ".cache");
  return path.join(base, "memini");
}

/**
 * Where a session records its project dir, keyed by the harness process id that
 * both the hooks and the headersHelper see as their parent. Lives in sessions/
 * so the existing stale-buffer sweeper garbage-collects it.
 */
export function sessionCwdPath(
  ppid: number,
  env: Record<string, string | undefined> = process.env,
): string {
  return path.join(cacheDir(env), "sessions", `pid-${ppid}.cwd`);
}

/**
 * How long a recorded project dir stays trustworthy.
 *
 * A pid is not a durable identity: operating systems recycle them, Windows
 * especially aggressively. If a session crashes (a clean exit deletes its record
 * — see deleteSessionCwd) and the OS later hands the same pid to an unrelated
 * Claude Code session in a different repo, a record with no freshness check would
 * hand that new session the OLD repo's directory. Its MCP calls would then target
 * one namespace while its hooks wrote to another: the exact split this module
 * exists to prevent, reintroduced.
 *
 * The hooks refresh the record while a session is alive (SessionStart, and every
 * Stop, i.e. once per assistant turn), and the helper only ever reads it at
 * connect — which happens right around SessionStart. So a short window is
 * generous for any live session, while bounding how long a crashed session's pid
 * can impersonate a live one.
 *
 * `fs.existsSync` on the recorded path is NOT a freshness check: the old repo
 * still exists on disk. It is just the wrong repo.
 */
export const SESSION_CWD_TTL_MS = 6 * 60 * 60 * 1000; // 6 hours

interface SessionCwdRecord {
  cwd: string;
  /** Epoch ms. Compared against SESSION_CWD_TTL_MS on read. */
  writtenAt: number;
}

/**
 * Record this session's project dir. Best-effort; never throws.
 *
 * `now` is injectable so tests can age a record without sleeping.
 */
export function writeSessionCwd(
  ppid: number,
  cwd: string,
  env: Record<string, string | undefined> = process.env,
  now: number = Date.now(),
): void {
  if (!ppid || !cwd || !cwd.trim()) return;
  try {
    const p = sessionCwdPath(ppid, env);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    const rec: SessionCwdRecord = { cwd: path.resolve(cwd), writtenAt: now };
    fs.writeFileSync(p, JSON.stringify(rec));
  } catch {
    // best-effort: a hook must never fail the agent
  }
}

/**
 * Read a session's recorded project dir, or undefined.
 *
 * Returns undefined for a record older than SESSION_CWD_TTL_MS rather than
 * trusting it, because a stale record is worse than no record: falling back to
 * the legacy global file is merely racy, whereas a recycled pid's record is
 * confidently wrong.
 */
export function readSessionCwd(
  ppid: number,
  env: Record<string, string | undefined> = process.env,
  now: number = Date.now(),
): string | undefined {
  try {
    const raw = fs.readFileSync(sessionCwdPath(ppid, env), "utf8").trim();
    if (!raw) return undefined;

    const rec = JSON.parse(raw) as SessionCwdRecord;
    if (!rec || typeof rec.cwd !== "string" || !rec.cwd) return undefined;
    if (typeof rec.writtenAt !== "number" || !Number.isFinite(rec.writtenAt)) return undefined;

    const age = now - rec.writtenAt;
    // A clock skew into the future is as untrustworthy as an expired record.
    if (age < 0 || age > SESSION_CWD_TTL_MS) return undefined;

    return fs.existsSync(rec.cwd) ? rec.cwd : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Drop a session's record. Called on SessionEnd, which closes the pid-reuse
 * window entirely for every session that exits cleanly — leaving only crashes to
 * be covered by the TTL above.
 */
export function deleteSessionCwd(
  ppid: number,
  env: Record<string, string | undefined> = process.env,
): void {
  if (!ppid) return;
  try {
    fs.rmSync(sessionCwdPath(ppid, env), { force: true });
  } catch {
    // best-effort
  }
}

/**
 * The cwd of a process, by pid. Linux reads the /proc symlink; macOS shells out
 * to lsof (there is no /proc). Windows has neither, and returns undefined —
 * which is precisely why the session file above exists as a portable fallback.
 */
export function processCwd(pid: number): string | undefined {
  if (!pid || pid <= 1) return undefined;

  // Linux (and anything else exposing procfs).
  try {
    const link = fs.readlinkSync(`/proc/${pid}/cwd`);
    if (link && fs.existsSync(link)) return link;
  } catch {
    // not procfs — fall through
  }

  // macOS. `lsof -a -d cwd -p <pid> -Fn` prints a line "n/the/path".
  if (process.platform === "darwin") {
    try {
      const out = execFileSync("lsof", ["-a", "-d", "cwd", "-p", String(pid), "-Fn"], {
        stdio: ["ignore", "pipe", "ignore"],
        timeout: 1000,
      }).toString();
      for (const line of out.split("\n")) {
        if (line.startsWith("n/")) {
          const p = line.slice(1).trim();
          if (p && fs.existsSync(p)) return p;
        }
      }
    } catch {
      // lsof missing or refused — fall through
    }
  }

  return undefined;
}

/**
 * True when `dir` is a plugin install root rather than a project. Guards against
 * the failure this module exists to prevent: resolving a namespace from the
 * plugin's own version-named directory (which would scatter memories into
 * namespaces like "0.6.7").
 */
export function looksLikePluginRoot(
  dir: string,
  env: Record<string, string | undefined> = process.env,
): boolean {
  if (!dir) return true;
  const d = path.resolve(dir);
  const pluginRoot = env["CLAUDE_PLUGIN_ROOT"];
  if (pluginRoot && path.resolve(pluginRoot) === d) return true;
  // Defensive: any path inside a harness's plugin cache is never a project.
  return /[/\\](plugins?)[/\\]cache[/\\]/.test(d);
}

/**
 * Resolve the project directory for a process the harness gave no cwd to.
 *
 * Order matters:
 *   1. CLAUDE_PROJECT_DIR — authoritative if the harness ever starts setting it.
 *   2. The parent process's cwd — always fresh, and works on the very first
 *      connect, before any hook has run.
 *   3. The session file — portable (Windows), but only exists once a hook has
 *      fired, so it is the fallback rather than the primary.
 *
 * Returns undefined when none apply; the caller should then fall back to its own
 * legacy behavior rather than guess.
 */
export function resolveHarnessCwd(
  env: Record<string, string | undefined> = process.env,
  ppid: number = process.ppid,
): HarnessCwd | undefined {
  const explicit = env["CLAUDE_PROJECT_DIR"];
  if (explicit && explicit.trim() && !looksLikePluginRoot(explicit, env)) {
    return { cwd: path.resolve(explicit.trim()), source: "CLAUDE_PROJECT_DIR" };
  }

  const parent = processCwd(ppid);
  if (parent && !looksLikePluginRoot(parent, env)) {
    return { cwd: parent, source: "parent-process" };
  }

  const recorded = readSessionCwd(ppid, env);
  if (recorded && !looksLikePluginRoot(recorded, env)) {
    return { cwd: recorded, source: "session-file" };
  }

  return undefined;
}
