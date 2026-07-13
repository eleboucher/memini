#!/usr/bin/env node
// headersHelper for the memini MCP server. Claude Code runs this fresh on each
// connection (session start and reconnect; no caching) and merges the JSON it
// prints over the static headers. We emit the project's namespace and a bearer
// token when one is configured — which is what makes a single remote memini work
// per-project.
//
// The hard part is knowing WHICH project. Measured, in a live session:
//
//   cwd                : <plugin install root>
//   PWD                : <plugin install root>   <- rewritten; NOT the project
//   CLAUDE_PROJECT_DIR : unset
//   CLAUDE_PLUGIN_ROOT : <plugin install root>
//   process.ppid       : the session's `claude` process, cwd = the project dir
//
// So process.cwd() and PWD are both traps — they would resolve the namespace
// from the plugin's own version-named directory and scatter memories into
// namespaces like "0.6.7".
//
// This used to fall back to a single global cache file that the SessionStart
// hook wrote. That file is shared by every concurrent session, so with two
// sessions open in two repos it is last-writer-wins: both sessions' MCP calls
// target one namespace while their hooks write to another — the exact "writes
// land where recall doesn't look" split that `memini doctor` exists to diagnose.
//
// resolveHarnessCwd walks the process tree instead. The parent IS the session,
// and its cwd IS the project. The global file remains only as a last resort.

import {
  resolveProject,
  readCachedNamespace,
  createPlaintextBearerAuthGuard,
  resolveHome,
  DEBUG,
} from "./_shared.mjs";
import { resolveHarnessCwd } from "./_client.gen.mjs";

// Resolve a DIRECTORY first, then derive the namespace from it. Never cache the
// namespace itself here: re-resolving on every connect is what lets a namespace
// override take effect on reconnect, where a cached value would go stale.
const harness = resolveHarnessCwd(process.env, process.ppid);

let ns = "";
let nsSource = "none";
if (harness) {
  ns = resolveProject(harness.cwd);
  nsSource = harness.source;
} else {
  // No project signal at all (Windows before any hook has run, an unusual launch
  // path). Fall back to the namespace the hooks last cached. Racy across
  // concurrent sessions, which is why it is last — but better than no header,
  // which would silently drop every MCP call into the server's default namespace.
  ns = readCachedNamespace();
  if (ns) nsSource = "cache-file";
}

const headers = {};
if (ns) headers["X-Memini-Namespace"] = ns;

// X-Memini-Home: the caller's personal namespace (MEMINI_HOME), same env-only
// resolution as the hooks' REST client. Absent when unset — no home leg,
// matching the server's "no header = no home" contract.
const home = resolveHome();
if (home) headers["X-Memini-Home"] = home;

// Same plaintext-bearer guard as the hooks' REST client. The MCP endpoint is
// ${MEMINI_BASE_URL}/mcp (see .mcp.json), so check the base URL: warn when the
// token would travel over plaintext HTTP to a non-loopback host, and under
// MEMINI_REQUIRE_HTTPS=1 omit the header entirely (the guard's throw must not
// crash the headersHelper — no auth means the server refuses, which is the
// refusal REQUIRE_HTTPS asks for).
const mcpBase = process.env.MEMINI_BASE_URL || "http://localhost:8080";
const token = process.env.MEMINI_API_KEY || process.env.MEMINI_TOKEN;
if (token) {
  try {
    createPlaintextBearerAuthGuard((m) => console.error(`[memini] ${m}`))(mcpBase, token);
    headers.Authorization = `Bearer ${token}`;
  } catch (e) {
    console.error(`[memini] ${e?.message || e}`);
  }
}

if (DEBUG) {
  console.error(
    `[memini] headersHelper: namespace=${ns || "(none)"} via ${nsSource}` +
      `${harness ? ` cwd=${harness.cwd}` : ""} ppid=${process.ppid}`,
  );
}

process.stdout.write(JSON.stringify(headers));
