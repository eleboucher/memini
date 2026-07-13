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
} from "./_client.gen.mjs";

const args = process.argv.slice(2).filter((a) => a !== "--");
const cwd = process.cwd();
const boot = readBootstrap(process.env);

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
    signal: AbortSignal.timeout(5000),
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
    `MCP:    run /reload-plugins to apply (the headersHelper only runs on connect).`,
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
    `MCP:    run /reload-plugins to apply.`,
  ].join("\n");
}

async function main() {
  let result;
  if (args.length === 0) {
    result = await show();
  } else if (args[0] === "--clear" || args[0] === "clear") {
    result = await clear();
  } else {
    result = await set(args.join(" "));
  }
  if (result) process.stdout.write(result + "\n");
}

main().catch((e) => {
  console.error(`memini namespace failed: ${e?.message || e}`);
  process.exitCode = 1;
});
