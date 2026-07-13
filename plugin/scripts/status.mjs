#!/usr/bin/env node
// `/memini:status` — what this plugin is actually doing right now.
//
// Three things have to agree for memory to work, and nothing previously showed
// you whether they did:
//
//   1. the namespace the HOOKS write to (session digests, turn capture)
//   2. the namespace the MCP TOOLS write to (memory_remember / memory_recall)
//   3. the namespace the SERVER reads from when it assembles a read set
//
// When (1) and (2) diverge, memory silently half-works: the agent saves things
// it can never recall. That is the failure `memini doctor` was written to
// diagnose, and this reports it from the client side — where it originates, and
// without needing the `memini` binary at all, which matters because plugin-only
// users pointing at a remote server do not have it.
//
// Everything here is read-only.

import {
  resolveProjectDetailed,
  resolveProject,
  readCachedNamespace,
  namespaceCacheFile,
  pluginRootFile,
  tenantConfigPath,
  getJSON,
} from "./_shared.mjs";
import {
  describeSettings,
  resolveHarnessCwd,
  cacheDir,
  defaultOverridesPath,
} from "./_client.gen.mjs";
import fs from "node:fs";
import path from "node:path";

const JSON_OUT = process.argv.includes("--json");

// ─── what the MCP headersHelper will send ────────────────────────────
//
// Recomputed here with exactly the same logic mcp-headers.mjs uses, rather than
// read from a cache. The point is to answer "what WILL happen on the next MCP
// call", so a stale cache would defeat the check.
function mcpHeaderNamespace(cwd) {
  // The helper runs as a child of the session's `claude` process. From THIS
  // process (a Bash tool call) the parent is a shell, not claude — so we cannot
  // observe the helper's ppid. What we can do is verify the mechanism it relies
  // on: resolve from the same project dir, and separately report the legacy
  // cache file it would fall back to.
  const viaProject = resolveProject(cwd);
  const cached = readCachedNamespace();
  const harness = resolveHarnessCwd(process.env, process.ppid);
  return {
    // What the fixed helper resolves for this project.
    willSend: viaProject,
    // What the legacy global file holds — shared by every concurrent session.
    cacheFile: cached || "",
    cacheFilePath: namespaceCacheFile(),
    harnessCwd: harness?.cwd,
    harnessSource: harness?.source,
  };
}

function readInstalledPluginVersion() {
  try {
    const root = fs.readFileSync(pluginRootFile(), "utf8").trim();
    if (!root) return null;
    const manifest = JSON.parse(
      fs.readFileSync(path.join(root, ".claude-plugin", "plugin.json"), "utf8"),
    );
    return { root, version: manifest.version || "?" };
  } catch {
    return null;
  }
}

async function fetchServer(ns) {
  const out = { reachable: false };

  // Reachability is decided by an endpoint the plugin ACTUALLY depends on.
  // /healthz is tempting but wrong for this: a remote memini typically sits
  // behind an ingress that routes only /v1 and /mcp, so /healthz 404s while the
  // server is perfectly healthy. Reporting "server unreachable" there would be a
  // false alarm on the most common remote deployment.
  //
  // The read set doubles as the probe: it is the server's own introspection of
  // which namespaces a plain recall draws from, so it cannot drift from what
  // recall really does, and we need it anyway.
  const started = Date.now();
  out.readSet = await getJSON("/v1/namespaces/read-set", ns, 4000);
  out.latencyMs = Date.now() - started;
  out.reachable = out.readSet != null;

  // Dependency detail, when the deployment exposes it. Quiet: a 404 here means
  // "not routed", not "broken".
  const health = await getJSON("/healthz?verbose=1", ns, 4000, { quiet: true });
  if (health) {
    out.version = health.version;
    out.status = health.status;
    out.deps = health.deps;
  } else {
    out.healthExposed = false;
  }
  return out;
}

// ─── rendering ───────────────────────────────────────────────────────

const pad = (s, n) => String(s).padEnd(n);

function renderKnobs(lines, settings, group, names) {
  const rows = settings.filter((s) => names.includes(s.name));
  if (!rows.length) return;
  lines.push(`${group}`);
  for (const r of rows) {
    const origin = r.source === "env" ? `<- env` : `(default)`;
    lines.push(`  ${pad(r.name.replace(/^MEMINI_/, "").toLowerCase(), 28)} ${pad(r.value, 34)} ${origin}`);
  }
  lines.push("");
}

function render(report) {
  const { settings, ns, mcp, server, plugin } = report;
  const L = [];

  L.push(`memini — effective settings`);
  L.push(`cwd: ${settings.cwd}`);
  L.push("");

  // Namespace first: it is what people actually come here to find out.
  L.push(`NAMESPACE`);
  L.push(`  ${pad("effective", 28)} ${pad(ns.effective, 34)} <- ${ns.source}`);
  if (ns.override) {
    L.push(`  ${pad("without the override", 28)} ${pad(ns.withoutOverride.namespace, 34)} <- ${ns.withoutOverride.source}`);
  }
  if (ns.derived.namespace !== ns.effective) {
    L.push(`  ${pad("git/cwd would give", 28)} ${pad(ns.derived.namespace, 34)} <- ${ns.derived.source}`);
  }
  L.push(`  ${pad("home (personal)", 28)} ${ns.home || "(unset)"}`);
  L.push("");

  L.push(`MCP TOOL CALLS`);
  L.push(`  ${pad("header the helper sends", 28)} ${mcp.willSend || "(none)"}`);
  if (mcp.cacheFile && mcp.cacheFile !== mcp.willSend) {
    L.push(
      `  ${pad("legacy global cache file", 28)} ${pad(mcp.cacheFile, 34)} (shared by all sessions)`,
    );
  }
  L.push("");

  renderKnobs(L, settings.settings, "CONNECTION", [
    "MEMINI_BASE_URL",
    "MEMINI_MCP_URL",
    "MEMINI_API_KEY",
    "MEMINI_REQUIRE_HTTPS",
  ]);
  renderKnobs(L, settings.settings, "NAMESPACE INPUTS", [
    "MEMINI_NAMESPACE",
    "MEMINI_NAMESPACE_SCOPE",
    "MEMINI_AGENT",
    "MEMINI_HOME",
  ]);
  renderKnobs(L, settings.settings, "CAPTURE", [
    "MEMINI_CAPTURE_TURNS",
    "MEMINI_INLINE_EXTRACT",
    "MEMINI_AUTO_SAVE",
    "MEMINI_AUTO_SAVE_INTERVAL",
  ]);
  renderKnobs(L, settings.settings, "INJECTION BUDGETS", [
    "MEMINI_INJECT_BRIEFING_PINNED",
    "MEMINI_INJECT_BRIEFING_FACTS",
    "MEMINI_INJECT_BRIEFING_PROCEDURES",
    "MEMINI_INJECT_BRIEFING_RECENT",
    "MEMINI_INJECT_BRIEFING_MAX_TOK",
    "MEMINI_INJECT_PRETOOL_ITEMS",
    "MEMINI_INJECT_PRETOOL_MAX_TOK",
    "MEMINI_INJECT_PRETOOL_MIN_SCORE",
    "MEMINI_INJECT_PRETOOL_TOOLS",
    "MEMINI_INJECT_LABELS",
  ]);

  L.push(`SERVER`);
  if (!server.reachable) {
    L.push(`  ${pad("reachable", 28)} NO — could not reach the server`);
  } else {
    const ver = server.version ? `, ${server.version}` : "";
    L.push(`  ${pad("reachable", 28)} yes (${server.latencyMs}ms${ver})`);
    const d = server.deps || {};
    if (d.store) L.push(`  ${pad("store", 28)} ${d.store.ok ? "ok" : `FAILING — ${d.store.last_error || "?"}`}`);
    if (d.embedder) {
      L.push(
        `  ${pad("embedder", 28)} ${d.embedder.ok ? "ok" : `FAILING — ${d.embedder.last_error || "?"}`}`,
      );
    }
    if (d.llm) {
      L.push(
        `  ${pad("llm", 28)} ${d.llm.configured ? (d.llm.ok ? "ok" : `FAILING — ${d.llm.last_error || "?"}`) : "not configured"}`,
      );
    }
    if (server.healthExposed === false) {
      L.push(
        `  ${pad("dependency detail", 28)} unavailable (/healthz not routed — normal behind an ingress)`,
      );
    }
  }
  L.push("");

  if (server.readSet?.entries?.length) {
    L.push(`READ SET for "${ns.effective}" — where a plain recall looks`);
    L.push(`  ${pad("NAMESPACE", 34)} ${pad("ORIGIN", 12)} TIERS`);
    for (const e of server.readSet.entries) {
      const tiers = Array.isArray(e.tiers) && e.tiers.length ? e.tiers.join(",") : "all";
      L.push(`  ${pad(e.namespace, 34)} ${pad(e.origin, 12)} ${tiers}`);
    }
    L.push("");
  }

  L.push(`PATHS`);
  L.push(`  ${pad("overrides", 28)} ${report.paths.overrides}${fs.existsSync(report.paths.overrides) ? "" : " (absent)"}`);
  L.push(`  ${pad("tenant config", 28)} ${report.paths.config}${fs.existsSync(report.paths.config) ? "" : " (absent)"}`);
  L.push(`  ${pad("cache", 28)} ${report.paths.cache}`);
  if (plugin) L.push(`  ${pad("installed plugin", 28)} ${plugin.version}  ${plugin.root}`);
  L.push("");

  if (report.warnings.length) {
    L.push(`WARNINGS`);
    for (const w of report.warnings) {
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

  const settings = describeSettings({
    cwd,
    env: process.env,
    // Hand describeSettings THIS harness's resolver, so what it reports is what
    // the hooks actually do — project map, tenant template, agent segment and
    // all — rather than an idealized chain that only resembles it. The opts
    // pass-through carries ignoreOverride, which is how the counterfactual lines
    // see past an override (it lives in a file, so no amount of env-doctoring
    // would remove it).
    resolve: (env, o) => resolveProjectDetailed(cwd, env, o),
    cacheDir: cacheDir(process.env),
  });

  const ns = settings.namespace;
  const mcp = mcpHeaderNamespace(cwd);
  const plugin = readInstalledPluginVersion();
  const server = await fetchServer(ns.effective);

  const warnings = [...settings.warnings];

  // The split-brain check. The hooks and the MCP tools must target the same
  // namespace or the agent saves what it cannot recall.
  if (mcp.willSend && mcp.willSend !== ns.effective) {
    warnings.unshift({
      level: "warn",
      code: "namespace-split",
      message:
        `the hooks write to "${ns.effective}" but MCP tool calls would target ` +
        `"${mcp.willSend}". Memories saved via memory_remember will not be found ` +
        `by recall in this project.`,
      fix: "This should not happen — please report it.",
    });
  }

  // The legacy global file is shared by every concurrent session. If it
  // disagrees with this project, an older installed headersHelper (one without
  // the process-tree fix) would send the wrong namespace.
  if (mcp.cacheFile && mcp.cacheFile !== ns.effective) {
    warnings.push({
      level: "note",
      code: "stale-global-namespace-cache",
      message:
        `the legacy global namespace file holds "${mcp.cacheFile}", not "${ns.effective}" — ` +
        `it is written by whichever session started most recently. The current headersHelper ` +
        `ignores it (it resolves per session from the process tree), but an older installed ` +
        `plugin would send it.`,
      fix: `Harmless once every session is on this plugin version. Path: ${mcp.cacheFilePath}`,
    });
  }

  if (!server.reachable) {
    warnings.push({
      level: "warn",
      code: "server-unreachable",
      message: `could not reach the memini server; recall and capture are both failing.`,
      fix: "Check MEMINI_BASE_URL and that the server is running.",
    });
  }

  const report = {
    settings,
    ns,
    mcp,
    server,
    plugin,
    warnings,
    paths: {
      overrides: defaultOverridesPath(process.env),
      config: tenantConfigPath(process.env),
      cache: cacheDir(process.env),
    },
  };

  if (JSON_OUT) {
    process.stdout.write(JSON.stringify(report, null, 2) + "\n");
    return;
  }
  process.stdout.write(render(report) + "\n");
}

main().catch((e) => {
  console.error(`memini status failed: ${e?.message || e}`);
  process.exitCode = 1;
});
