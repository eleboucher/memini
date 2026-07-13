#!/usr/bin/env node
// `/memini:namespace [<namespace> | --clear]` — show, set, or clear the
// namespace override for this project.
//
// The override wins over MEMINI_NAMESPACE, deliberately. A globally exported
// MEMINI_NAMESPACE (a shell rc, or a fish universal variable) pins every repo on
// the machine to one namespace; if the environment beat the override, this
// command would silently do nothing on exactly the machines that need it.
//
// Scope is the project (keyed by git toplevel), not the session. The MCP
// headersHelper cannot distinguish two sessions in the same repo — it is given
// no session id — so per-project is the honest granularity, and pretending
// otherwise would be a lie the user eventually discovers the hard way.

import {
  resolveProjectDetailed,
  readOverride,
} from "./_shared.mjs";
import {
  writeOverride,
  clearOverride,
  normalizeNamespace,
  validateNamespace,
  overrideKey,
} from "./_client.gen.mjs";

const args = process.argv.slice(2).filter((a) => a !== "--");
const cwd = process.cwd();

function show() {
  const current = readOverride(cwd, { env: process.env });
  const detailed = resolveProjectDetailed(cwd, process.env);

  const out = [];
  out.push(`namespace: ${detailed.namespace}  (source: ${detailed.source})`);
  out.push(`project:   ${overrideKey(cwd)}`);

  if (current) {
    out.push("");
    out.push(`An override is active (set ${current.setAt}).`);
    out.push(`Clear it with:  /memini:namespace --clear`);
  } else {
    out.push("");
    out.push(`No override — resolving automatically.`);
    out.push(`Set one with:  /memini:namespace <namespace>`);
  }
  return out.join("\n");
}

function set(raw) {
  const ns = normalizeNamespace(raw);
  const bad = validateNamespace(ns);
  if (bad) {
    console.error(`memini: invalid namespace ${JSON.stringify(raw)}: ${bad}`);
    process.exitCode = 1;
    return null;
  }

  const before = resolveProjectDetailed(cwd, process.env);
  writeOverride(cwd, ns, { env: process.env });
  const after = resolveProjectDetailed(cwd, process.env);

  return [
    `namespace override set: ${before.namespace} -> ${after.namespace}`,
    `project: ${overrideKey(cwd)}`,
    ``,
    `hooks:  active immediately (session digests, turn capture, recall injection)`,
    `MCP:    run /reload-plugins to apply`,
    ``,
    `The MCP headersHelper only runs when the server connects, so memory_remember and`,
    `memory_recall keep targeting "${before.namespace}" until the plugin reconnects — until`,
    `then the hooks and the MCP tools are pointed at different namespaces.`,
  ].join("\n");
}

function clear() {
  const before = resolveProjectDetailed(cwd, process.env);
  const removed = clearOverride(cwd, { env: process.env });
  if (!removed) {
    return `No override was set for ${overrideKey(cwd)} — nothing to clear.`;
  }
  const after = resolveProjectDetailed(cwd, process.env);
  return [
    `namespace override cleared: ${before.namespace} -> ${after.namespace}  (source: ${after.source})`,
    ``,
    `hooks:  active immediately`,
    `MCP:    run /reload-plugins to apply`,
  ].join("\n");
}

let result;
if (args.length === 0) {
  result = show();
} else if (args[0] === "--clear" || args[0] === "clear") {
  result = clear();
} else {
  result = set(args.join(" "));
}

if (result) process.stdout.write(result + "\n");
