/**
 * The client-side namespace resolution story, in two parts:
 *
 *   - deriveLocalNamespace: the LOCAL fallback derivation chain (declared >
 *     remote > toplevel > cwd, plus an agent suffix). This is a deliberate
 *     mirror of the server's internal/nsresolve.Resolve (minus pin/env, which
 *     only the server can see, and minus key/server-default, which only the
 *     server knows) — vector-verified against the exact same fixture Go's
 *     TestDerivationVectors consumes, so the two languages can never quietly
 *     drift apart.
 *   - resolveNamespace: what a caller should actually DO with a
 *     performHandshake result — trust it outright when present (the server
 *     already applied pin > env > declared > derive), otherwise fall back to
 *     the env pin, otherwise to local derivation. Every non-handshake path is
 *     `degraded: true`, because it is a guess the server hasn't confirmed.
 *
 * The client never second-guesses a successful handshake: reimplementing
 * pin-lookup client-side is not just redundant, it would require shipping the
 * project_map to every client, which the server intentionally does not do.
 */

import type { ProjectFacts } from "./facts.js";
import type { Bootstrap } from "./bootstrap.js";
import type { HandshakeResult } from "./handshake.js";

export type LocalSource = "declared" | "remote" | "toplevel" | "cwd" | "default";

/**
 * Split a git remote URL into its path segments (owner, repo, ...), dropping
 * the host/user/port and any empty segments. Ported verbatim from
 * plugin/scripts/_shared.mjs:33-40 (remotePathSegments) so this package's
 * derivation matches the plugin hooks' and the Go port's byte-for-byte.
 */
function remotePathSegments(url: string | undefined): string[] {
  if (typeof url !== "string" || !url) return [];
  const cleaned = url.trim().replace(/\/+$/, "").replace(/\.git$/i, "");
  if (!cleaned) return [];
  const scpMatch = cleaned.match(/^[^/:]+:[^/]/);
  const p = scpMatch ? cleaned.slice(scpMatch[0].indexOf(":") + 1) : cleaned;
  return p.split("/").filter(Boolean);
}

/**
 * The repo name (last path segment) of a git remote URL, or undefined when
 * unparseable. Ported from plugin/scripts/_shared.mjs:46-49.
 */
export function repoNameFromRemote(url: string | undefined): string | undefined {
  const segs = remotePathSegments(url);
  return segs.length ? segs[segs.length - 1] : undefined;
}

/**
 * An "owner-repo" slug (last two path segments joined with "-"), so
 * same-named repos under different owners don't collide — or the bare repo
 * name when only one segment is present, or undefined when unparseable. Only
 * the owner segment is sanitized; the repo segment and both segments' case
 * are left untouched. Ported from plugin/scripts/_shared.mjs:58-65
 * (repoSlugFromRemote). Used when namespace_scope is "owner_repo".
 */
export function repoSlugFromRemote(url: string | undefined): string | undefined {
  const segs = remotePathSegments(url);
  if (!segs.length) return undefined;
  if (segs.length === 1) return segs[0];
  const owner = segs[segs.length - 2].replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  const repo = segs[segs.length - 1];
  return owner ? `${owner}-${repo}` : repo;
}

/**
 * Nest `ns` under a per-agent segment ("ns/reviewer") when `agent` is
 * non-blank after sanitizing (collapse anything outside [A-Za-z0-9._-] to
 * "-", then trim leading/trailing "-"). A blank agent, or one that sanitizes
 * to nothing (e.g. "!!!"), adds no suffix. Mirrors
 * plugin/scripts/_shared.mjs:153-158 (withAgent).
 */
function withAgent(ns: string, agent: string | undefined): string {
  const trimmed = (agent || "").trim();
  if (!trimmed) return ns;
  const seg = trimmed.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  return seg ? `${ns}/${seg}` : ns;
}

/**
 * The no-pin, no-env local derivation chain: declared_namespace verbatim (no
 * agent suffix — a caller who declares a namespace outright means exactly
 * that one) beats a remote-derived repo name (or owner-repo slug under
 * scope="owner_repo") beats the toplevel basename beats the cwd basename,
 * then an agent suffix nests under whichever of the last three won. Falls
 * back to the literal namespace "default" only when there is truly nothing to
 * derive from (no declared_namespace, no remote, no toplevel, no cwd_basename).
 *
 * MUST agree with every case in
 * test/fixtures/derivation-vectors.json — the cross-language parity gate
 * Go's internal/nsresolve.Resolve is vector-tested against too.
 */
export function deriveLocalNamespace(
  f: ProjectFacts,
  scope: "repo" | "owner_repo" = "repo",
): { namespace: string; source: LocalSource } {
  if (f.declared_namespace) {
    return { namespace: f.declared_namespace, source: "declared" };
  }

  let base: string | undefined;
  let source: LocalSource = "default";

  if (f.remote_url) {
    const name = scope === "owner_repo" ? repoSlugFromRemote(f.remote_url) : repoNameFromRemote(f.remote_url);
    if (name) {
      base = name;
      source = "remote";
    }
  }
  if (!base && f.toplevel_basename) {
    base = f.toplevel_basename;
    source = "toplevel";
  }
  if (!base && f.cwd_basename) {
    base = f.cwd_basename;
    source = "cwd";
  }
  if (!base) {
    return { namespace: "default", source: "default" };
  }

  return { namespace: withAgent(base, f.agent), source };
}

/**
 * What a caller should actually use: a successful handshake wins outright
 * (the server already applied pin > env > declared > derive, so re-deriving
 * here would be redundant at best and wrong at worst — the client cannot see
 * pins). Absent a handshake, `boot.namespaceEnv` (MEMINI_NAMESPACE) is honored
 * next — the same var the server would have used had the handshake
 * succeeded — and only then does the local derivation chain run. Every
 * non-handshake path is `degraded: true`: it is this client's best guess,
 * not a server-confirmed resolution.
 *
 * The local derivation chain always uses the default "repo" scope: whether an
 * operator wants "owner_repo" is a ClientSettings.namespace_scope value this
 * client only ever learns FROM a handshake, so a degraded fallback with no
 * handshake to consult has no scope opinion to apply.
 */
export function resolveNamespace(
  boot: Bootstrap,
  facts: ProjectFacts,
  hs?: HandshakeResult,
): { namespace: string; source: string; degraded: boolean } {
  if (hs) {
    return { namespace: hs.namespace, source: `server:${hs.namespace_source}`, degraded: false };
  }
  if (boot.namespaceEnv) {
    return { namespace: boot.namespaceEnv, source: "env", degraded: true };
  }
  const { namespace, source } = deriveLocalNamespace(facts);
  return { namespace, source: `local-${source}`, degraded: true };
}
