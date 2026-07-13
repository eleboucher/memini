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
// resolveHarnessCwd walks the process tree instead: the parent IS the session,
// and its cwd IS the project. From that cwd we run the SAME handshake flow the
// hooks use — reuse a valid per-session cached handshake, else POST a live one
// (and cache it), else fall back to env/local derivation. There is deliberately
// NO global-namespace-file last resort: that file was shared by every concurrent
// session, so two sessions in two repos made it last-writer-wins — the exact
// PR-#111 race the per-session handshake cache exists to end. With no project
// signal at all we emit AUTH-ONLY headers and let the SERVER apply the key's
// default namespace, rather than guess.

import {
  readBootstrap,
  gatherFacts,
  readCachedHandshake,
  writeCachedHandshake,
  performHandshake,
  resolveNamespace,
  resolveHarnessCwd,
  assertBearerTransportSafe,
} from "./_client.gen.mjs";
import { DEBUG } from "./_shared.mjs";

async function resolveNamespaceForHeader(boot, ppid) {
  // No project signal (Windows before any hook has run, an unusual launch path):
  // emit no namespace header. The server then applies the key's default
  // namespace — a defined outcome, unlike a namespace guessed from the plugin's
  // own install dir.
  const harness = resolveHarnessCwd(process.env, ppid);
  if (!harness) return { namespace: "", source: "auth-only" };

  const facts = gatherFacts(harness.cwd, process.env);

  // A valid per-session cached handshake wins — SessionStart populated it and it
  // is authoritative for this session's namespace + settings.
  const cached = readCachedHandshake(ppid, harness.cwd, facts, process.env);
  if (cached) return { namespace: cached.namespace, source: `cache:${cached.namespace_source}`, cwd: harness.cwd };

  // Miss (no hook has run yet, or the TTL lapsed): do one bounded live handshake
  // and cache it. performHandshake runs the plaintext-bearer guard outside its
  // own try/catch, so wrap it — a guard throw must degrade, never crash the
  // helper (which would break the MCP connection JSON).
  let hs;
  try {
    hs = await performHandshake(boot, facts, { timeoutMs: 2500, clientName: "memini-claude-plugin" });
  } catch {
    hs = undefined;
  }
  if (hs) {
    writeCachedHandshake(ppid, harness.cwd, facts, hs, process.env);
    return { namespace: hs.namespace, source: `server:${hs.namespace_source}`, cwd: harness.cwd };
  }

  // Server unreachable: env override or local derivation — never the network.
  const r = resolveNamespace(boot, facts, undefined);
  return { namespace: r.namespace, source: r.source, cwd: harness.cwd };
}

async function main() {
  const boot = readBootstrap(process.env);
  const ppid = process.ppid;

  const { namespace, source, cwd } = await resolveNamespaceForHeader(boot, ppid);

  const headers = {};
  if (namespace) headers["X-Memini-Namespace"] = namespace;

  // X-Memini-Home: the caller's personal namespace (MEMINI_HOME). Absent when
  // unset — no home leg, matching the server's "no header = no home" contract.
  if (boot.homeEnv) headers["X-Memini-Home"] = boot.homeEnv;

  // Same plaintext-bearer guard as the hooks' REST client (the shared bundle
  // export). The MCP endpoint is ${MEMINI_BASE_URL}/mcp, so check the base URL:
  // under MEMINI_REQUIRE_HTTPS a bearer bound for plaintext HTTP is omitted
  // rather than sent (the guard's throw must not crash the helper — no auth
  // means the server refuses, which is the refusal REQUIRE_HTTPS asks for).
  if (boot.apiKey) {
    try {
      assertBearerTransportSafe(boot.baseUrl, boot.apiKey);
      headers.Authorization = `Bearer ${boot.apiKey}`;
    } catch (e) {
      console.error(`[memini] ${e?.message || e}`);
    }
  }

  if (DEBUG) {
    console.error(
      `[memini] headersHelper: namespace=${namespace || "(none)"} via ${source}` +
        `${cwd ? ` cwd=${cwd}` : ""} ppid=${ppid}`,
    );
  }

  process.stdout.write(JSON.stringify(headers));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] headersHelper error:", e);
  // Never emit malformed output: an empty object is a valid "no extra headers".
  process.stdout.write("{}");
});
