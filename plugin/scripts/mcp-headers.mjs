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
  applyCredentialFallback,
  credentialsPath,
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
  if (!harness) return { namespace: "", source: "auth-only", authExpected: false };

  const facts = gatherFacts(harness.cwd, process.env);

  // A valid per-session cached handshake wins — SessionStart populated it and it
  // is authoritative for this session's namespace + settings.
  const cached = readCachedHandshake(ppid, harness.cwd, facts, process.env);
  if (cached) {
    return {
      namespace: cached.namespace,
      source: `cache:${cached.namespace_source}`,
      cwd: harness.cwd,
      // The cache remembers whether this session's hooks authenticated — the
      // signal main() uses to tell "no auth configured anywhere" (fine) apart
      // from "auth works for the hooks but not for this helper" (the silent
      // 2.1.238 failure worth shouting about).
      authExpected: cached.identity?.authenticated === true,
    };
  }

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
    return {
      namespace: hs.namespace,
      source: `server:${hs.namespace_source}`,
      cwd: harness.cwd,
      authExpected: hs.identity?.authenticated === true,
    };
  }

  // Server unreachable: env override or local derivation — never the network.
  const r = resolveNamespace(boot, facts, undefined);
  // Server unreachable and no cache: nothing proves auth is expected.
  return { namespace: r.namespace, source: r.source, cwd: harness.cwd, authExpected: false };
}

async function main() {
  // Claude Code >= 2.1.238 runs a plugin headersHelper WITHOUT inherited
  // credential env vars (intentional hardening — see credential.ts in
  // @memini/client). MEMINI_BASE_URL survives (not credential-shaped);
  // MEMINI_API_KEY does not. Fall back to the 0600 credentials file the
  // SessionStart hook (full env) mirrors it into, keyed by base URL so a
  // bearer stored for one server is never sent to another. An env-provided
  // key still wins, so pre-2.1.238 behavior is unchanged.
  const { boot, source: credSource } = applyCredentialFallback(readBootstrap(process.env), process.env);
  const ppid = process.ppid;

  const { namespace, source, cwd, authExpected } = await resolveNamespaceForHeader(boot, ppid);

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

  // The silent failure mode this plugin shipped with on Claude Code >= 2.1.238:
  // hooks authenticated (reads look healthy) while the MCP channel dies with
  // no Authorization (writes silently impossible). If this session's cached
  // handshake proves the hooks ARE authenticated and we still ended up with no
  // bearer from env or file, say so loudly — this stderr lands in Claude
  // Code's MCP logs — instead of letting the server 401 and Claude Code wander
  // into OAuth discovery ("Dynamic Client Registration rejected").
  if (authExpected && !headers.Authorization) {
    console.error(
      `[memini] headersHelper has no API key: MEMINI_API_KEY is not in this process's environment ` +
        `(Claude Code >= 2.1.238 strips credential env vars from plugin headersHelpers) and no stored ` +
        `credential for ${boot.baseUrl} exists at ${credentialsPath(process.env)}. memini's MCP tools ` +
        `will fail to authenticate. Update the memini plugin, then start a new session so the ` +
        `SessionStart hook can store the credential — or register the server at user scope ` +
        `(see plugin/README.md, "Claude Code 2.1.238 and credential env vars").`,
    );
  }

  if (DEBUG) {
    console.error(
      `[memini] headersHelper: namespace=${namespace || "(none)"} via ${source}` +
        `${cwd ? ` cwd=${cwd}` : ""} ppid=${ppid} cred=${credSource}`,
    );
  }

  process.stdout.write(JSON.stringify(headers));
}

main().catch((e) => {
  if (DEBUG) console.error("[memini] headersHelper error:", e);
  // Never emit malformed output: an empty object is a valid "no extra headers".
  process.stdout.write("{}");
});
