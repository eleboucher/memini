/**
 * Gathering what this client currently knows about the project.
 *
 * These are the "project facts" the config-handshake redesign sends as the
 * `project` field of HandshakeRequest (api/openapi.yaml): everything the
 * server's nsresolve.Resolve chain needs to derive a namespace, PLUS the raw
 * material for resolve.ts's local fallback when the handshake itself is
 * unreachable. Best-effort throughout — a caller gathering facts on every
 * tool call cannot afford to ever throw or block noticeably.
 */

import { execFileSync } from "node:child_process";
import path from "node:path";
import crypto from "node:crypto";

export interface ProjectFacts {
  remote_url?: string;
  toplevel_path?: string;
  toplevel_basename?: string;
  cwd_basename: string;
  agent?: string;
  env_namespace?: string;
  /** For gateway/CI callers with no meaningful cwd. Set by those callers, never by gatherFacts. */
  declared_namespace?: string;
}

/** Run `git <args>` in `dir`, trimmed stdout or undefined on any failure. Never throws. */
function gitOut(args: string[], dir: string): string | undefined {
  try {
    const out = execFileSync("git", args, {
      cwd: dir,
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 500,
    })
      .toString()
      .trim();
    return out || undefined;
  } catch {
    return undefined;
  }
}

/**
 * Gather this project's facts: the git remote and toplevel (best-effort, via
 * `git -C <cwd> remote get-url origin` / `rev-parse --show-toplevel`), the cwd
 * basename (always present — the last-resort fallback), and agent/env_namespace
 * from MEMINI_AGENT/MEMINI_NAMESPACE. declared_namespace is never set here —
 * it is a gateway/CI caller's own field to populate after the fact.
 */
export function gatherFacts(
  cwd: string,
  env: Record<string, string | undefined> = process.env,
): ProjectFacts {
  const dir = cwd && cwd.trim() ? cwd : process.cwd();

  const facts: ProjectFacts = {
    cwd_basename: path.basename(dir),
  };

  const remote = gitOut(["remote", "get-url", "origin"], dir);
  if (remote) facts.remote_url = remote;

  const toplevel = gitOut(["rev-parse", "--show-toplevel"], dir);
  if (toplevel) {
    facts.toplevel_path = toplevel;
    facts.toplevel_basename = path.basename(toplevel);
  }

  const agent = env["MEMINI_AGENT"];
  if (agent) facts.agent = agent;

  const ns = (env["MEMINI_NAMESPACE"] || "").trim();
  if (ns) facts.env_namespace = ns;

  return facts;
}

/**
 * A stable short fingerprint of a facts object, used by the handshake cache to
 * tell whether the project's facts have changed since a cached result was
 * written (see handshake.ts's readCachedHandshake). Sorting the keys makes the
 * fingerprint independent of insertion order; hashing (rather than comparing
 * the JSON string) keeps the cache record small and avoids leaking raw facts
 * into a diff-friendly form.
 */
export function factsFingerprint(f: ProjectFacts): string {
  const keys = Object.keys(f).sort();
  const rec = f as unknown as Record<string, unknown>;
  const parts = keys.map((k) => `${k}=${rec[k]}`);
  return crypto.createHash("sha256").update(parts.join("\n"), "utf8").digest("hex").slice(0, 16);
}
