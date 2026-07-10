#!/usr/bin/env node
// headersHelper for the memini MCP server. Claude Code runs this per connection
// and merges the JSON it prints over the static headers. We emit the
// cwd-resolved project namespace (the SAME resolver the hooks use, so capture
// and recall target one namespace) and a bearer token when one is configured —
// which is what makes a single remote memini work per-project.

import { resolveProject, readCachedNamespace, createPlaintextBearerAuthGuard } from "./_shared.mjs";

// Per the Claude Code docs, the headersHelper runs with cwd = the plugin's
// (version-named) install dir and is NOT given CLAUDE_PROJECT_DIR — so
// process.cwd() must NEVER be used to resolve the namespace (it would yield the
// plugin version, e.g. "0.6.3", and scatter memories into version namespaces).
// Prefer CLAUDE_PROJECT_DIR if it's ever present, else the namespace the hooks
// cached for the active project. If neither is available, omit the header and
// let the server's default namespace apply — better than a wrong guess.
const projectDir = process.env.CLAUDE_PROJECT_DIR;
const ns = projectDir && projectDir.trim() ? resolveProject(projectDir) : readCachedNamespace();
const headers = {};
if (ns) headers["X-Memini-Namespace"] = ns;

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

process.stdout.write(JSON.stringify(headers));
