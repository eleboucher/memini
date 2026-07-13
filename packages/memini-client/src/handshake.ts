/**
 * The client half of the config-handshake redesign: POST /v1/handshake
 * (api/openapi.yaml — HandshakeRequest/HandshakeResponse) plus a per-session
 * cache so a harness that calls in on every tool use doesn't round-trip the
 * network for every single call.
 *
 * performHandshake is fail-soft by design (returns undefined on ANY failure)
 * because the whole point of the redesign is that a client degrades to local
 * derivation when the server is unreachable — see resolve.ts's
 * resolveNamespace, which is exactly the caller of this fallback path. The one
 * exception is the plaintext-bearer guard: sending a real secret is not
 * something to silently swallow an error around, so its throw is left to
 * propagate.
 */

import fs from "node:fs";
import path from "node:path";

import { assertBearerTransportSafe, type Bootstrap } from "./bootstrap.js";
import { factsFingerprint, type ProjectFacts } from "./facts.js";
import { cacheDir } from "./session.js";

/** Mirrors HandshakeResponse (api/openapi.yaml) exactly. */
export interface HandshakeResult {
  namespace: string;
  namespace_source: string;
  /** Present only when namespace_source is "pin". */
  pin?: {
    key: string;
    note?: string;
    created_by?: string;
    updated_at: string;
  };
  identity: {
    authenticated: boolean;
    key_name?: string;
    home?: string;
    default_namespace?: string;
  };
  /** Fully resolved ClientSettings — every field present. */
  settings: Record<string, unknown>;
  /** Per-field provenance for `settings`, keyed by the same field names. */
  settings_sources: Record<string, string>;
  /** The read-set `namespace` resolves to (same shape as GET /v1/namespaces/readset). */
  read_set: unknown[];
  server: {
    version: string;
    default_namespace: string;
  };
}

/**
 * How long a cached handshake stays trustworthy. Deliberately much shorter
 * than session.ts's SESSION_CWD_TTL_MS (6h): unlike a project directory, the
 * resolved namespace and settings can change server-side at any time (an
 * operator adds a pin, edits global defaults, rotates a key's settings), so a
 * long TTL would mean a harness process could act on stale server-side state
 * for hours after an operator fixed it.
 */
export const HANDSHAKE_TTL_MS = 10 * 60 * 1000;

/**
 * POST the handshake. `boot` supplies the transport (base URL, bearer token,
 * X-Memini-Home) and `facts` the project the server should resolve a
 * namespace for. Returns undefined on ANY failure — network error, non-2xx,
 * malformed JSON, or a timeout (default 2500ms, overridable via
 * `opts.timeoutMs`) — so callers always have a well-defined fallback path
 * (resolve.ts's resolveNamespace) rather than a rejected promise to handle.
 *
 * The plaintext-bearer guard runs BEFORE the try/catch on purpose: a guard
 * throw is a real throw, not one of the fail-soft failure modes above.
 */
export async function performHandshake(
  boot: Bootstrap,
  facts: ProjectFacts,
  opts: { timeoutMs?: number; clientName?: string; clientVersion?: string } = {},
): Promise<HandshakeResult | undefined> {
  assertBearerTransportSafe(boot.baseUrl, boot.apiKey, {
    MEMINI_REQUIRE_HTTPS: boot.requireHttps ? "1" : "0",
  });

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), opts.timeoutMs ?? 2500);

  try {
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (boot.apiKey) headers["Authorization"] = `Bearer ${boot.apiKey}`;
    if (boot.homeEnv) headers["X-Memini-Home"] = boot.homeEnv;

    const body = JSON.stringify({
      project: facts,
      client: { name: opts.clientName, version: opts.clientVersion },
    });

    const res = await fetch(`${boot.baseUrl}/v1/handshake`, {
      method: "POST",
      headers,
      body,
      signal: controller.signal,
    });
    if (!res.ok) return undefined;

    return (await res.json()) as HandshakeResult;
  } catch {
    return undefined;
  } finally {
    clearTimeout(timer);
  }
}

interface HandshakeCacheRecord {
  result: HandshakeResult;
  cwd: string;
  factsHash: string;
  writtenAt: number;
}

/** Where a session's cached handshake lives, keyed by the same ppid session.ts uses for the cwd record. */
export function handshakeCachePath(
  ppid: number,
  env: Record<string, string | undefined> = process.env,
): string {
  return path.join(cacheDir(env), "sessions", `pid-${ppid}.handshake.json`);
}

/**
 * A session's cached handshake result, or undefined. Validates ALL of:
 * the file exists and parses, `now - writtenAt` falls within [0, TTL] (a
 * negative age — a clock moved backward — is as untrustworthy as an expired
 * one, matching session.ts's readSessionCwd), the recorded cwd matches the
 * caller's cwd exactly, and the recorded factsHash matches
 * factsFingerprint(facts) now. Any mismatch is a cache miss, never a throw —
 * a stale or wrong cache entry should fall through to a fresh handshake (or
 * local derivation), not break the caller.
 */
export function readCachedHandshake(
  ppid: number,
  cwd: string,
  facts: ProjectFacts,
  env: Record<string, string | undefined> = process.env,
  now: number = Date.now(),
): HandshakeResult | undefined {
  try {
    const raw = fs.readFileSync(handshakeCachePath(ppid, env), "utf8");
    const rec = JSON.parse(raw) as HandshakeCacheRecord;
    if (!rec || typeof rec !== "object") return undefined;
    if (typeof rec.writtenAt !== "number" || !Number.isFinite(rec.writtenAt)) return undefined;

    const age = now - rec.writtenAt;
    if (age < 0 || age > HANDSHAKE_TTL_MS) return undefined;

    if (typeof rec.cwd !== "string" || path.resolve(rec.cwd) !== path.resolve(cwd)) return undefined;
    if (typeof rec.factsHash !== "string" || rec.factsHash !== factsFingerprint(facts)) return undefined;
    if (!rec.result || typeof rec.result !== "object") return undefined;

    return rec.result;
  } catch {
    return undefined;
  }
}

/**
 * Record a session's handshake result. Best-effort; never throws — a hook or
 * MCP call must never fail because the cache write did.
 */
export function writeCachedHandshake(
  ppid: number,
  cwd: string,
  facts: ProjectFacts,
  result: HandshakeResult,
  env: Record<string, string | undefined> = process.env,
  now: number = Date.now(),
): void {
  try {
    const p = handshakeCachePath(ppid, env);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    const rec: HandshakeCacheRecord = {
      result,
      cwd: path.resolve(cwd),
      factsHash: factsFingerprint(facts),
      writtenAt: now,
    };
    fs.writeFileSync(p, JSON.stringify(rec));
  } catch {
    // best-effort
  }
}

/** Drop a session's cached handshake. Idempotent — calling it twice is not an error. */
export function deleteCachedHandshake(
  ppid: number,
  env: Record<string, string | undefined> = process.env,
): void {
  try {
    fs.rmSync(handshakeCachePath(ppid, env), { force: true });
  } catch {
    // best-effort
  }
}

/**
 * Remove every cached handshake across ALL sessions — used when something
 * has changed that no single session's TTL/cwd/facts check would catch (e.g.
 * an operator just edited a pin or global settings and wants every running
 * harness to pick it up on its next call). Only *.handshake.json is touched;
 * session.ts's pid-*.cwd records live in the same directory and must survive.
 */
export function invalidateAllHandshakes(env: Record<string, string | undefined> = process.env): void {
  const dir = path.join(cacheDir(env), "sessions");
  let names: string[];
  try {
    names = fs.readdirSync(dir);
  } catch {
    return; // no sessions dir yet — nothing to invalidate
  }
  for (const name of names) {
    if (!name.endsWith(".handshake.json")) continue;
    try {
      fs.rmSync(path.join(dir, name), { force: true });
    } catch {
      // best-effort
    }
  }
}
