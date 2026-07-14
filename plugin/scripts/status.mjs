#!/usr/bin/env node
// `/memini:status` — what this plugin is actually doing right now, and why.
//
// The whole point of the config-handshake redesign is that ONE source of truth —
// the server handshake — decides the namespace and every behavioral setting.
// This command runs a live handshake (a diagnostic must never trust a cache)
// and shows, with provenance: which namespace the server resolved and how, the
// caller's identity, every effective setting and where it came from
// (env override > server key/global/default > built-in), and — when the server
// is unreachable — that everything below is a local-derived degraded guess.
//
// Everything here is read-only. Secrets are redacted, so the output is safe to
// paste into an issue.

import { getSessionContext, getJSON, DEBUG, pluginRootFile } from "./_shared.mjs";
import {
  BEHAVIOR_KNOBS,
  deriveLocalNamespace,
  factsFingerprint,
  gatherFacts,
  resolveHarnessCwd,
  isPlaintextBearerUnsafe,
  redactValue,
  cacheDir,
} from "./_client.gen.mjs";
import fs from "node:fs";
import path from "node:path";

const JSON_OUT = process.argv.includes("--json");

function readInstalledPluginVersion() {
  try {
    const root = fs.readFileSync(pluginRootFile(), "utf8").trim();
    if (!root) return null;
    const manifest = JSON.parse(fs.readFileSync(path.join(root, ".claude-plugin", "plugin.json"), "utf8"));
    return { root, version: manifest.version || "?" };
  } catch {
    return null;
  }
}

// ─── rendering ───────────────────────────────────────────────────────

const pad = (s, n) => String(s).padEnd(n);

function fmtValue(v) {
  if (Array.isArray(v)) return v.length ? v.join("|") : "(none)";
  if (typeof v === "boolean") return v ? "on" : "off";
  if (v === "" || v == null) return "(unset)";
  return String(v);
}

// The source column: env override beats server (key > global > default per the
// handshake's settings_sources) beats the built-in default.
function sourceLabel(row, reachable) {
  if (row.source === "env-override") return reachable ? "<- env (overriding server)" : "<- env";
  if (row.source === "server") return `<- server (${row.serverSource || "?"})`;
  return "(default)";
}

function render(report) {
  const { ns, identity, connection, settings, server, readSet, warnings, plugin } = report;
  const L = [];

  L.push(`memini — status`);
  L.push(`cwd: ${report.cwd}`);
  L.push("");

  L.push(`NAMESPACE`);
  L.push(`  ${pad("effective", 20)} ${pad(ns.effective, 34)} <- ${ns.sourceLabel}`);
  L.push(`  ${pad("local fallback", 20)} ${pad(ns.localFallback, 34)} (facts-only derivation)`);
  if (ns.pin) {
    const who = ns.pin.created_by ? `set by ${ns.pin.created_by}` : "set by admin/dev";
    L.push(`  ${pad("pin", 20)} ${pad(ns.effective, 34)} (key ${ns.pin.key}, ${who}${ns.pin.updated_at ? `, ${ns.pin.updated_at}` : ""})`);
    if (ns.pin.note) L.push(`  ${pad("", 20)} note: ${ns.pin.note}`);
  }
  L.push("");

  L.push(`IDENTITY (from handshake)`);
  if (!server.reachable) {
    L.push(`  (unavailable — server unreachable)`);
  } else {
    L.push(`  ${pad("key", 20)} ${identity.key_name || "(unnamed: admin key or dev mode)"}`);
    L.push(`  ${pad("home", 20)} ${pad(identity.home || "(unset)", 34)} <- ${identity.homeSource}`);
    L.push(`  ${pad("default namespace", 20)} ${identity.default_namespace || "(none)"}`);
  }
  L.push("");

  L.push(`CONNECTION`);
  L.push(`  ${pad("base_url", 20)} ${connection.base_url}`);
  L.push(`  ${pad("api_key", 20)} ${connection.api_key || "(unset)"}`);
  L.push(`  ${pad("require_https", 20)} ${connection.require_https ? "on" : "off"}`);
  L.push("");

  L.push(`SETTINGS`);
  for (const r of settings) {
    L.push(`  ${pad(r.name.replace(/^MEMINI_/, "").toLowerCase(), 28)} ${pad(fmtValue(r.value), 22)} ${sourceLabel(r, server.reachable)}`);
  }
  L.push("");

  L.push(`SERVER`);
  if (!server.reachable) {
    L.push(`  ${pad("handshake", 20)} FAILED — degraded to local derivation + built-in defaults`);
    L.push(`  ${pad("base_url", 20)} ${connection.base_url}`);
  } else {
    L.push(`  ${pad("handshake", 20)} ok`);
    const ver = server.version ? `, ${server.version}` : "";
    const lat = server.latencyMs != null ? ` (${server.latencyMs}ms${ver})` : ver ? ` (${server.version})` : "";
    L.push(`  ${pad("reachable", 20)} yes${lat}`);
  }
  L.push("");

  if (readSet?.entries?.length) {
    L.push(`READ SET for "${ns.effective}" — where a plain recall looks`);
    L.push(`  ${pad("NAMESPACE", 34)} ${pad("ORIGIN", 12)} TIERS`);
    for (const e of readSet.entries) {
      const tiers = Array.isArray(e.tiers) && e.tiers.length ? e.tiers.join(",") : "all";
      L.push(`  ${pad(e.namespace, 34)} ${pad(e.origin, 12)} ${tiers}`);
    }
    L.push("");
  }

  if (plugin) {
    L.push(`PLUGIN`);
    L.push(`  ${pad("installed", 20)} ${plugin.version}  ${plugin.root}`);
    L.push(`  ${pad("cache", 20)} ${report.cacheDir}`);
    L.push("");
  }

  if (warnings.length) {
    L.push(`WARNINGS`);
    for (const w of warnings) {
      L.push(`  [${w.level === "warn" ? "!" : "i"}] ${w.code}: ${w.message}`);
      if (w.fix) L.push(`      fix: ${w.fix}`);
    }
  } else {
    L.push(`No problems detected.`);
  }

  return L.join("\n");
}

// ─── main ────────────────────────────────────────────────────────────

async function main() {
  const cwd = process.cwd();

  // A live handshake — status is a diagnostic, always fresh. allowNetwork
  // "always" round-trips the server (or degrades to local derivation and
  // built-in defaults when it can't reach it). noPersist keeps this read-only:
  // a diagnostic must not overwrite the session's cached handshake that the
  // hooks own.
  const ctx = await getSessionContext({ cwd, ppid: process.ppid, allowNetwork: "always", timeoutMs: 4000, noPersist: true });
  const hs = ctx.handshake;
  const boot = ctx.boot;
  const reachable = !ctx.degraded;

  // effective namespace source, spelled out.
  let sourceLabelText;
  if (reachable) {
    if (hs.namespace_source === "pin") sourceLabelText = `server:pin (key ${hs.pin?.key || "?"})`;
    else if (hs.namespace_source === "env") sourceLabelText = `env (overriding — server would otherwise derive)`;
    else sourceLabelText = `server:${hs.namespace_source}`;
  } else {
    sourceLabelText = `${ctx.source} (degraded — server unreachable)`;
  }

  const localFallback = deriveLocalNamespace(ctx.facts).namespace;

  // Settings rows with per-field provenance (env-override > server key/global/
  // default > built-in default).
  const settings = BEHAVIOR_KNOBS.map((k) => {
    const { value, source } = ctx.setting(k.wireKey);
    const serverSource = source === "server" ? hs?.settings_sources?.[k.wireKey] || "?" : "";
    return { name: k.envName, wireKey: k.wireKey, value, source, serverSource };
  });

  // Identity + home provenance.
  const identity = hs?.identity || {};
  const homeSource = identity.home ? "key binding" : boot.homeEnv ? "MEMINI_HOME" : "unset";
  const home = identity.home || boot.homeEnv || "";

  // Read set: a fresh probe of the RENAMED endpoint (no dash) doubles as the
  // reachability/latency measure, and shows where a plain recall actually looks.
  let readSet = null;
  let latencyMs = null;
  if (reachable) {
    const started = Date.now();
    readSet = await getJSON("/v1/namespaces/readset", ctx.namespace, 4000);
    latencyMs = Date.now() - started;
    // The handshake already carried a read set; fall back to it if the probe
    // failed for any reason so the section still renders.
    if (!readSet && Array.isArray(hs?.read_set)) readSet = { entries: hs.read_set };
  }

  const warnings = [];

  // Degraded: the server is unreachable, so everything below is a local guess.
  if (!reachable) {
    warnings.push({
      level: "warn",
      code: "degraded-mode",
      message: `could not reach the memini server at ${boot.baseUrl}: the namespace is local-derived and every setting is a built-in default, not what the server would return.`,
      fix: "Check MEMINI_BASE_URL and that the server is running; recall and capture are both failing until it is reachable.",
    });
  }

  // MEMINI_NAMESPACE is a machine-wide pin: it beats server derivation for every
  // repo on this machine (only a server-side pin overrides it).
  if (boot.namespaceEnv) {
    warnings.push({
      level: "warn",
      code: "global-namespace-pin",
      message: `MEMINI_NAMESPACE is set to "${boot.namespaceEnv}", which overrides the server's namespace for EVERY repo on this machine (unless that repo is pinned server-side). If it is exported from a shell rc or a fish universal variable, every repo shares one memory pool.`,
      fix: "Unset MEMINI_NAMESPACE and pin per-project instead: /memini:namespace <ns> (a pin beats the environment).",
    });
  }

  // Home unset only when NEITHER the key binding NOR MEMINI_HOME provides one.
  if (!identity.home && !boot.homeEnv) {
    warnings.push({
      level: "warn",
      code: "home-unset",
      message: 'no personal namespace: the key has no bound home and MEMINI_HOME is unset, so visibility:"personal" writes will error and no personal leg merges into recall.',
      fix: "Export MEMINI_HOME=personal/<you>, or bind a home on the API key.",
    });
  }

  // Plaintext bearer.
  if (isPlaintextBearerUnsafe(boot.baseUrl, boot.apiKey)) {
    warnings.push({
      level: "warn",
      code: "plaintext-bearer",
      message: `a bearer token is configured for plaintext HTTP to ${boot.baseUrl}; the token and your memory payloads can be observed on the network.`,
      fix: "Use HTTPS, or tunnel over SSH. Set MEMINI_REQUIRE_HTTPS=1 to make this a hard refusal.",
    });
  }

  // Split-brain check: compare the command's cwd against the cwd the MCP
  // headersHelper would resolve from the process tree. Server-determined
  // namespaces make a namespace comparison redundant — but if the two cwds
  // yield different project FACTS, the MCP tools and these hooks are describing
  // different projects to the server.
  const harness = resolveHarnessCwd(process.env, process.ppid);
  if (harness && harness.cwd) {
    const here = factsFingerprint(ctx.facts);
    const there = factsFingerprint(gatherFacts(harness.cwd, process.env));
    if (here !== there) {
      warnings.unshift({
        level: "warn",
        code: "cwd-split",
        message: `this command's project (${cwd}) and the MCP-resolved project (${harness.cwd}, via ${harness.source}) have different git facts, so MCP tool calls may target a different namespace than these hooks. `,
        fix: "Run /memini:status from the project root, and check which directory the harness process is in.",
      });
    }
  }

  const plugin = readInstalledPluginVersion();

  const report = {
    cwd,
    cacheDir: cacheDir(process.env),
    degraded: ctx.degraded,
    ns: {
      effective: ctx.namespace,
      source: ctx.source,
      sourceLabel: sourceLabelText,
      localFallback,
      pin: reachable && hs.namespace_source === "pin" ? hs.pin : null,
    },
    identity: {
      authenticated: !!identity.authenticated,
      key_name: identity.key_name || "",
      home,
      homeSource,
      default_namespace: identity.default_namespace || "",
    },
    connection: {
      base_url: boot.baseUrl,
      api_key: boot.apiKey ? redactValue(boot.apiKey) : "",
      require_https: boot.requireHttps,
    },
    settings,
    server: {
      reachable,
      latencyMs,
      version: hs?.server?.version || "",
      default_namespace: hs?.server?.default_namespace || "",
    },
    readSet,
    plugin,
    warnings,
  };

  if (JSON_OUT) {
    process.stdout.write(JSON.stringify(report, null, 2) + "\n");
    return;
  }
  process.stdout.write(render(report) + "\n");
}

main().catch((e) => {
  console.error(`memini status failed: ${e?.message || e}`);
  if (DEBUG) console.error(e);
  process.exitCode = 1;
});
