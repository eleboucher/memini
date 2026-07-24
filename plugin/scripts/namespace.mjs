#!/usr/bin/env node
// `/memini:namespace [<namespace> | --clear]` — show, set, or clear this
// project's namespace PIN.
//
// Pins live on the memini server (POST/PUT/DELETE /v1/pins), keyed by the
// project's git remote and/or toplevel path. Because they are server-side, a
// pin follows you across machines and every client (hooks, MCP, the Go CLI)
// sees the same one — unlike the old per-machine overrides.json this replaces.
//
// A pin beats every other namespace_source at handshake time, INCLUDING
// MEMINI_NAMESPACE. That ordering is deliberate: a globally exported
// MEMINI_NAMESPACE (a shell rc, or a fish universal variable) pins every repo on
// the machine to one namespace, and if the environment won, this command would
// silently do nothing on exactly the machines that most need it.
//
// Scope is the PROJECT (its remote/toplevel), not the session — the MCP
// headersHelper is given no session id, so per-project is the honest
// granularity.

import {
  readBootstrap,
  assertBearerTransportSafe,
  gatherFacts,
  deriveLocalNamespace,
  performHandshake,
  invalidateAllHandshakes,
  normalizeNamespace,
  validateNamespace,
  readOverrides,
  defaultOverridesPath,
} from "./_client.gen.mjs";
import fs from "node:fs";
import path from "node:path";

const args = process.argv.slice(2).filter((a) => a !== "--");
const cwd = process.cwd();
const boot = readBootstrap(process.env);
const CODEX_HOST = Boolean(process.env.PLUGIN_ROOT);

// Send a JSON request to the pins endpoint. Returns { ok, status, body } or
// throws (network/guard) so callers can render the offline message.
async function pinsRequest(method, body) {
  assertBearerTransportSafe(boot.baseUrl, boot.apiKey); // throws under MEMINI_REQUIRE_HTTPS
  const headers = { "Content-Type": "application/json" };
  if (boot.apiKey) headers["Authorization"] = `Bearer ${boot.apiKey}`;
  if (boot.homeEnv) headers["X-Memini-Home"] = boot.homeEnv;
  const res = await fetch(`${boot.baseUrl}/v1/pins`, {
    method,
    headers,
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(boot.timeoutMs),
  });
  let parsed = null;
  try {
    parsed = await res.json();
  } catch {
    // 204 (delete) and empty bodies parse to null — fine.
  }
  return { ok: res.ok, status: res.status, body: parsed };
}

// Facts that can key a pin. At least one of remote_url/toplevel_path is
// required server-side (400 otherwise) — this repo has neither only when it is
// not a git repo at all.
function pinFacts() {
  const f = gatherFacts(cwd, process.env);
  const out = {};
  if (f.remote_url) out.remote_url = f.remote_url;
  if (f.toplevel_path) out.toplevel_path = f.toplevel_path;
  return out;
}

const OFFLINE_HELP = [
  `Could not reach the memini server at ${boot.baseUrl}.`,
  `Pins live on the server, so setting one needs it reachable. For an offline,`,
  `machine-local override instead, export MEMINI_NAMESPACE=<namespace>.`,
].join("\n");

async function show() {
  let hs;
  try {
    hs = await performHandshake(boot, gatherFacts(cwd, process.env), {
      timeoutMs: 3000,
      clientName: "memini-namespace-command",
    });
  } catch {
    hs = undefined;
  }

  const out = [];
  if (!hs) {
    // Degraded: no server. Show what the hooks would derive locally (env pin, else
    // the same facts-only derivation) so the user still learns their effective
    // namespace, and say why it is a guess.
    const f = gatherFacts(cwd, process.env);
    const local = boot.namespaceEnv || deriveLocalNamespace(f).namespace;
    out.push(`namespace: ${local}  (local derivation — server unreachable)`);
    out.push("");
    out.push(`Could not reach ${boot.baseUrl}, so this is a local guess, not the`);
    out.push(`server's authority. A pin (if any) can only be read from the server.`);
    return out.join("\n");
  }

  out.push(`namespace: ${hs.namespace}  (source: ${hs.namespace_source})`);
  if (hs.namespace_source === "pin" && hs.pin) {
    out.push(`pin:       key ${hs.pin.key}`);
    if (hs.pin.created_by) out.push(`           set by ${hs.pin.created_by}`);
    if (hs.pin.updated_at) out.push(`           updated ${hs.pin.updated_at}`);
    if (hs.pin.note) out.push(`           note: ${hs.pin.note}`);
    if (boot.namespaceEnv) {
      out.push("");
      out.push(`MEMINI_NAMESPACE is set to "${boot.namespaceEnv}", but the pin wins — a pin`);
      out.push(`beats the environment on purpose (see /memini:namespace docs).`);
    }
  } else if (hs.namespace_source === "env") {
    out.push("");
    out.push(`This comes from MEMINI_NAMESPACE, which pins EVERY repo on this machine to`);
    out.push(`one namespace. To scope just this project, set a pin: /memini:namespace <ns>`);
    out.push(`(a pin beats the environment).`);
  }
  out.push("");
  out.push(`Set a pin with:    /memini:namespace <namespace>`);
  out.push(`Clear it with:     /memini:namespace --clear`);
  return out.join("\n");
}

async function set(raw) {
  const ns = normalizeNamespace(raw);
  const bad = validateNamespace(ns);
  if (bad) {
    console.error(`memini: invalid namespace ${JSON.stringify(raw)}: ${bad}`);
    process.exitCode = 1;
    return null;
  }

  const facts = pinFacts();
  if (!facts.remote_url && !facts.toplevel_path) {
    console.error(
      `memini: this directory has no git remote or toplevel to pin a namespace to.\n` +
        `A pin is keyed by the project's git identity; run inside a git repository, ` +
        `or export MEMINI_NAMESPACE=${ns} for a machine-local override.`,
    );
    process.exitCode = 1;
    return null;
  }

  let res;
  try {
    res = await pinsRequest("PUT", { namespace: ns, ...facts });
  } catch (e) {
    console.error(`memini: ${e?.message || e}\n\n${OFFLINE_HELP}`);
    process.exitCode = 1;
    return null;
  }
  if (!res.ok) {
    const msg = res.body?.error || res.body?.message || `HTTP ${res.status}`;
    console.error(`memini: could not set the pin: ${msg}\n\n${OFFLINE_HELP}`);
    process.exitCode = 1;
    return null;
  }

  // Every session's cached handshake is now stale — a pin changed. Drop them so
  // the next hook invocation re-resolves against the new pin.
  invalidateAllHandshakes();

  const entry = res.body || {};
  return [
    `namespace pinned: ${entry.namespace || ns}`,
    `project key:      ${entry.key || Object.values(facts)[0]}`,
    ``,
    `hooks:  active on the next invocation (session digests, turn capture, recall).`,
    CODEX_HOST
      ? `MCP:    start a new thread (or restart Codex) to apply the new namespace.`
      : `MCP:    run /reload-plugins to apply (the headersHelper only runs on connect).`,
    ``,
    `The pin lives on the memini server, so it follows you across machines and`,
    `every client resolves the same namespace. It beats MEMINI_NAMESPACE.`,
  ].join("\n");
}

async function clear() {
  const facts = pinFacts();
  if (!facts.remote_url && !facts.toplevel_path) {
    console.error(
      `memini: this directory has no git remote or toplevel, so it cannot have a pin to clear.`,
    );
    process.exitCode = 1;
    return null;
  }

  let res;
  try {
    res = await pinsRequest("DELETE", facts);
  } catch (e) {
    console.error(`memini: ${e?.message || e}\n\n${OFFLINE_HELP}`);
    process.exitCode = 1;
    return null;
  }

  if (res.status === 404) {
    return `No pin was set for this project — nothing to clear.`;
  }
  if (!res.ok) {
    const msg = res.body?.error || res.body?.message || `HTTP ${res.status}`;
    console.error(`memini: could not clear the pin: ${msg}\n\n${OFFLINE_HELP}`);
    process.exitCode = 1;
    return null;
  }

  invalidateAllHandshakes();
  return [
    `namespace pin cleared — this project resolves automatically again.`,
    ``,
    `hooks:  active on the next invocation.`,
    CODEX_HOST ? `MCP:    start a new thread (or restart Codex) to apply.` : `MCP:    run /reload-plugins to apply.`,
  ].join("\n");
}

// --- --migrate: bulk overrides.json -> server pins -------------------------
//
// Every overrides.json entry is keyed by an absolute directory path — the
// git toplevel at the time it was written, or the resolved cwd for a non-git
// directory (see the bundle's overrideKey). That key IS the toplevel_path
// fact: there is no stored git remote to re-derive, and the directory may not
// even exist anymore (a moved or deleted checkout), so this migrates purely
// off the stored path rather than re-running git. An entry whose directory
// is long gone still migrates fine as a path-keyed pin.

// GET /v1/pins and return the Set of existing pin keys ("remote:..."/
// "path:..."), so a bulk migration can skip a project that already has an
// explicit pin (set via /memini:namespace since these overrides were written)
// rather than clobber it.
async function fetchExistingPinKeys() {
  const res = await pinsRequest("GET");
  if (!res.ok) throw new Error(res.body?.error || res.body?.message || `HTTP ${res.status}`);
  const entries = Array.isArray(res.body?.entries) ? res.body.entries : [];
  return new Set(entries.map((e) => e.key));
}

// Reads ~/.config/memini/config.json (or null on any error/absence) — the
// retired client-side tenancy config (`tenantRoots`/`template`), kept only as
// legacy data to migrate FROM. Never written here: this command must not
// recreate the file it exists to help retire.
function readClientConfig() {
  const dir = path.dirname(defaultOverridesPath(process.env));
  try {
    const parsed = JSON.parse(fs.readFileSync(path.join(dir, "config.json"), "utf8"));
    return parsed && typeof parsed === "object" ? parsed : null;
  } catch {
    return null;
  }
}

async function migrate() {
  const file = readOverrides({ env: process.env });
  const keys = Object.keys(file.overrides || {}).sort();
  const out = [];

  if (keys.length === 0) {
    out.push("No overrides.json entries to migrate.");
  } else {
    let existing;
    try {
      existing = await fetchExistingPinKeys();
    } catch (e) {
      console.error(`memini: ${e?.message || e}\n\n${OFFLINE_HELP}`);
      process.exitCode = 1;
      return null;
    }

    const rows = [];
    let migratedCount = 0;
    let failedCount = 0;
    for (const key of keys) {
      const entry = file.overrides[key];
      const namespace = entry && typeof entry.namespace === "string" ? entry.namespace : "";
      if (!namespace) {
        rows.push({ key, namespace: "(invalid entry)", status: "failed: no namespace stored" });
        failedCount++;
        continue;
      }
      if (existing.has(`path:${key}`)) {
        rows.push({ key, namespace, status: "already-pinned" });
        continue;
      }
      try {
        const res = await pinsRequest("PUT", { namespace, toplevel_path: key, note: "migrated from overrides.json" });
        if (res.ok) {
          rows.push({ key, namespace, status: "migrated" });
          migratedCount++;
        } else {
          const msg = res.body?.error || res.body?.message || `HTTP ${res.status}`;
          rows.push({ key, namespace, status: `failed: ${msg}` });
          failedCount++;
        }
      } catch (e) {
        rows.push({ key, namespace, status: `failed: ${e?.message || e}` });
        failedCount++;
      }
    }

    out.push(`KEY -> NAMESPACE -> STATUS`);
    for (const r of rows) out.push(`${r.key} -> ${r.namespace} -> ${r.status}`);
    out.push("");

    if (migratedCount > 0) invalidateAllHandshakes();

    const overridesPath = defaultOverridesPath(process.env);
    if (failedCount === 0) {
      try {
        fs.renameSync(overridesPath, `${overridesPath}.migrated`);
        out.push(`All entries migrated or already pinned — renamed overrides.json to overrides.json.migrated.`);
      } catch (e) {
        out.push(`Migration succeeded, but could not rename overrides.json: ${e?.message || e}`);
      }
    } else {
      out.push(`${failedCount} entr${failedCount === 1 ? "y" : "ies"} failed — overrides.json left in place; re-run --migrate to retry.`);
    }
  }

  // config.json's tenantRoots/template is a separate, retired client-side
  // mechanism that this cannot auto-translate: it encodes a tenancy decision
  // only a human can make, so we surface it (read-only) rather than migrate it.
  const cfg = readClientConfig();
  if (cfg && (cfg.tenantRoots || cfg.template)) {
    out.push("");
    out.push(`Also found ~/.config/memini/config.json with tenantRoots/template. This cannot`);
    out.push(`be auto-translated:`);
    if (cfg.tenantRoots) out.push(`  tenantRoots: ${JSON.stringify(cfg.tenantRoots)}`);
    if (cfg.template) out.push(`  template:    ${cfg.template}`);
    out.push(``);
    out.push(`Recreate it by hand, either as:`);
    out.push(`  - a namespace_prefix on each API key (per-credential tenancy), or`);
    out.push(`  - a per-project pin (/memini:namespace <ns>, one per project).`);
  }

  return out.join("\n");
}

async function main() {
  let result;
  if (args.length === 0) {
    result = await show();
  } else if (args[0] === "--clear" || args[0] === "clear") {
    result = await clear();
  } else if (args[0] === "--migrate" || args[0] === "migrate") {
    result = await migrate();
  } else {
    result = await set(args.join(" "));
  }
  if (result) process.stdout.write(result + "\n");
}

main().catch((e) => {
  console.error(`memini namespace failed: ${e?.message || e}`);
  process.exitCode = 1;
});
