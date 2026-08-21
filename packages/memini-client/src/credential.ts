/**
 * The env-independent credential source for MCP headersHelper processes.
 *
 * Claude Code 2.1.238 stopped passing inherited credential env vars to
 * headersHelper scripts registered by a plugin, a project .mcp.json, or an
 * agent file (intentional hardening: a plugin is third-party code, and env
 * inheritance handed every installed plugin every *_API_KEY in the shell).
 * memini's helper read its bearer exclusively from MEMINI_API_KEY, so on
 * 2.1.238+ it emitted no Authorization header, the server 401'd, Claude Code
 * fell into OAuth discovery, and every memini MCP tool silently vanished —
 * while the hooks (ordinary processes with full env) kept working, so reads
 * looked healthy while writes were impossible.
 *
 * Filesystem access was never restricted — only env inheritance was. So the
 * SessionStart hook (full env) mirrors MEMINI_API_KEY into a 0600 file here,
 * and the helper falls back to it when its own env has no key. Same shape as
 * the plugin-root breadcrumb and the per-session handshake cache.
 *
 * Deliberately NOT gated on a MEMINI_API_KEY_FILE-style env var: any name
 * containing KEY/TOKEN/SECRET would be stripped by the same heuristic. Fixed
 * well-known path, no env var required.
 *
 * Honest tradeoff: this puts a bearer at rest on disk where any process
 * running as the user can read it — a partial re-opening of what Anthropic
 * closed. Mitigations: it is memini's own file at 0600 in the user's config
 * dir, holding only memini's key (never other apps' credentials), and the
 * hardening targeted inheritance BREADTH (every plugin seeing every
 * credential), not secrets-at-rest. Entries are keyed by base URL so a
 * bearer stored for one server is never sent to another, and an unset
 * MEMINI_API_KEY retires the stored copy on the next session start, so the
 * file tracks the env rather than outliving it.
 */

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import type { Bootstrap } from "./bootstrap.js";

export const CREDENTIALS_VERSION = 1;

export interface StoredCredential {
  api_key: string;
  updated_at: string;
}

export interface CredentialsFile {
  version: number;
  /** Keyed by credentialKey(baseUrl) so multiple servers coexist. */
  credentials: Record<string, StoredCredential>;
}

/** $XDG_CONFIG_HOME/memini/credentials, else ~/.config/memini/credentials. */
export function credentialsPath(env: Record<string, string | undefined> = process.env): string {
  const xdg = env["XDG_CONFIG_HOME"];
  const base = xdg && xdg.trim() ? xdg : path.join(os.homedir() || os.tmpdir(), ".config");
  return path.join(base, "memini", "credentials");
}

/** The map key for a server: trimmed base URL without trailing slashes. */
export function credentialKey(baseUrl: string): string {
  return (baseUrl || "").trim().replace(/\/+$/, "");
}

/** Read + parse the file. Any error yields an empty map — never throws. */
function readFile(p: string): CredentialsFile {
  try {
    const parsed = JSON.parse(fs.readFileSync(p, "utf8"));
    if (!parsed || typeof parsed !== "object" || typeof parsed.credentials !== "object" || parsed.credentials === null) {
      return { version: CREDENTIALS_VERSION, credentials: {} };
    }
    return parsed as CredentialsFile;
  } catch {
    return { version: CREDENTIALS_VERSION, credentials: {} };
  }
}

/** The stored bearer for `baseUrl`, or "" on any miss or error. */
export function readStoredApiKey(baseUrl: string, env: Record<string, string | undefined> = process.env): string {
  const entry = readFile(credentialsPath(env)).credentials[credentialKey(baseUrl)];
  return typeof entry?.api_key === "string" ? entry.api_key : "";
}

export interface SyncResult {
  ok: boolean;
  path: string;
  action: "written" | "removed" | "unchanged" | "skipped";
  error?: string;
}

/**
 * Mirror the env truth into the file: a non-empty `apiKey` upserts the entry
 * for `baseUrl`; an empty one retires it (deleting the file outright when it
 * holds nothing else, so no empty husk lingers). Atomic tmp+rename write at
 * 0600 in a 0700 dir. Never throws — callers get {ok:false, error} instead,
 * because this runs inside hooks that must not fail the agent.
 */
export function syncStoredApiKey(
  baseUrl: string,
  apiKey: string,
  env: Record<string, string | undefined> = process.env,
): SyncResult {
  const p = credentialsPath(env);
  try {
    const file = readFile(p);
    const key = credentialKey(baseUrl);
    const existing = file.credentials[key];

    if (!apiKey) {
      if (!existing) return { ok: true, path: p, action: "skipped" };
      delete file.credentials[key];
      if (Object.keys(file.credentials).length === 0) {
        fs.rmSync(p, { force: true });
        return { ok: true, path: p, action: "removed" };
      }
      writeAtomic(p, file);
      return { ok: true, path: p, action: "removed" };
    }

    if (existing?.api_key === apiKey) return { ok: true, path: p, action: "unchanged" };
    file.version = CREDENTIALS_VERSION;
    file.credentials[key] = { api_key: apiKey, updated_at: new Date().toISOString() };
    writeAtomic(p, file);
    return { ok: true, path: p, action: "written" };
  } catch (e) {
    return { ok: false, path: p, action: "skipped", error: (e as Error)?.message || String(e) };
  }
}

function writeAtomic(p: string, file: CredentialsFile): void {
  fs.mkdirSync(path.dirname(p), { recursive: true, mode: 0o700 });
  const tmp = `${p}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(file, null, 2) + "\n", { mode: 0o600 });
  fs.renameSync(tmp, p);
}

/**
 * The read-side fallback: an env-provided key always wins; only when the env
 * has none is the stored key for this exact base URL substituted. Returns the
 * (possibly new) Bootstrap plus where the key came from, so callers can log
 * and warn precisely.
 */
export function applyCredentialFallback(
  boot: Bootstrap,
  env: Record<string, string | undefined> = process.env,
): { boot: Bootstrap; source: "env" | "file" | "none" } {
  if (boot.apiKey) return { boot, source: "env" };
  const stored = readStoredApiKey(boot.baseUrl, env);
  if (!stored) return { boot, source: "none" };
  return { boot: { ...boot, apiKey: stored }, source: "file" };
}
