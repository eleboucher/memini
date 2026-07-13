import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import {
  performHandshake,
  handshakeCachePath,
  readCachedHandshake,
  writeCachedHandshake,
  deleteCachedHandshake,
  invalidateAllHandshakes,
  HANDSHAKE_TTL_MS,
  type HandshakeResult,
} from "../src/handshake.js";
import { readBootstrap, type Bootstrap } from "../src/bootstrap.js";
import { sessionCwdPath, writeSessionCwd } from "../src/session.js";
import type { ProjectFacts } from "../src/facts.js";

function tmpCacheEnv(): Record<string, string> {
  return { XDG_CACHE_HOME: fs.mkdtempSync(path.join(os.tmpdir(), "memini-cache-")) };
}

function tmpDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "memini-proj-"));
}

function fakeResult(overrides: Partial<HandshakeResult> = {}): HandshakeResult {
  return {
    namespace: "acme/phoenix",
    namespace_source: "remote",
    identity: { authenticated: true },
    settings: { capture_turns: true },
    settings_sources: { capture_turns: "default" },
    read_set: [{ namespace: "acme/phoenix", origin: "primary" }],
    server: { version: "0.7.0-test", default_namespace: "default" },
    ...overrides,
  };
}

/** Swap global fetch for the duration of `fn`, then always restore it. */
async function withFetch<T>(impl: typeof fetch, fn: () => Promise<T>): Promise<T> {
  const original = globalThis.fetch;
  globalThis.fetch = impl;
  try {
    return await fn();
  } finally {
    globalThis.fetch = original;
  }
}

// ─── performHandshake: fetch matrix ─────────────────────────────────

test("performHandshake: success shape passes through untouched, POSTing to <baseUrl>/v1/handshake", async () => {
  const boot = readBootstrap({ MEMINI_BASE_URL: "http://localhost:8080" });
  const facts: ProjectFacts = { cwd_basename: "phoenix" };
  const result = fakeResult();

  let capturedUrl: string | undefined;
  let capturedMethod: string | undefined;
  let capturedBody: unknown;

  const got = await withFetch(
    (async (url: string | URL, init?: RequestInit) => {
      capturedUrl = String(url);
      capturedMethod = init?.method;
      capturedBody = JSON.parse(String(init?.body));
      return new Response(JSON.stringify(result), { status: 200 });
    }) as typeof fetch,
    () => performHandshake(boot, facts, { clientName: "test-client", clientVersion: "1.2.3" }),
  );

  assert.deepEqual(got, result);
  assert.equal(capturedUrl, "http://localhost:8080/v1/handshake");
  assert.equal(capturedMethod, "POST");
  assert.deepEqual(capturedBody, {
    project: facts,
    client: { name: "test-client", version: "1.2.3" },
  });
});

test("performHandshake: Authorization present only when apiKey set; X-Memini-Home present only when homeEnv set", async () => {
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  async function captureHeaders(boot: Bootstrap): Promise<Record<string, string>> {
    let headers: Record<string, string> | undefined;
    await withFetch(
      (async (_url: string | URL, init?: RequestInit) => {
        headers = init?.headers as Record<string, string>;
        return new Response(JSON.stringify(fakeResult()), { status: 200 });
      }) as typeof fetch,
      () => performHandshake(boot, facts),
    );
    return headers!;
  }

  const withBoth = await captureHeaders(readBootstrap({ MEMINI_API_KEY: "sk-1", MEMINI_HOME: "personal/kit" }));
  assert.equal(withBoth["Authorization"], "Bearer sk-1");
  assert.equal(withBoth["X-Memini-Home"], "personal/kit");

  const withNeither = await captureHeaders(readBootstrap({}));
  assert.equal("Authorization" in withNeither, false);
  assert.equal("X-Memini-Home" in withNeither, false);
});

test("performHandshake: a 401/500 response is a fail-soft undefined, never a throw", async () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  for (const status of [401, 500]) {
    const got = await withFetch(
      (async () => new Response("nope", { status })) as typeof fetch,
      () => performHandshake(boot, facts),
    );
    assert.equal(got, undefined, `status ${status}`);
  }
});

test("performHandshake: malformed JSON in a 200 response is a fail-soft undefined", async () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  const got = await withFetch(
    (async () => new Response("{ not json", { status: 200 })) as typeof fetch,
    () => performHandshake(boot, facts),
  );
  assert.equal(got, undefined);
});

test("performHandshake: a network error is a fail-soft undefined", async () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  const got = await withFetch(
    (async () => {
      throw new Error("ECONNREFUSED");
    }) as typeof fetch,
    () => performHandshake(boot, facts),
  );
  assert.equal(got, undefined);
});

test("performHandshake: a timeout (abort firing) is a fail-soft undefined", async () => {
  const boot = readBootstrap({});
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  const got = await withFetch(
    (async (_url: string | URL, init?: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      })) as typeof fetch,
    () => performHandshake(boot, facts, { timeoutMs: 20 }),
  );
  assert.equal(got, undefined);
});

test("performHandshake: the plaintext-bearer guard throw propagates — it is not swallowed as fail-soft", async () => {
  const boot = readBootstrap({
    MEMINI_BASE_URL: "http://remote.example.com",
    MEMINI_API_KEY: "sk-1",
    MEMINI_REQUIRE_HTTPS: "1",
  });
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  // No fetch mock installed — if the guard's throw were swallowed, this would
  // hit the real network and time out/fail for an unrelated reason.
  await assert.rejects(() => performHandshake(boot, facts), /plaintext HTTP/);
});

// ─── cache ───────────────────────────────────────────────────────────

test("handshakeCachePath: keyed by ppid, under <cacheDir>/sessions", () => {
  const env = tmpCacheEnv();
  const p = handshakeCachePath(4242, env);
  assert.equal(p, path.join(env.XDG_CACHE_HOME, "memini", "sessions", "pid-4242.handshake.json"));
});

test("cache round-trip: write then read returns the exact same result", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };
  const result = fakeResult();

  writeCachedHandshake(1111, cwd, facts, result, env);
  assert.deepEqual(readCachedHandshake(1111, cwd, facts, env), result);
});

test("cache: TTL expiry via injected now", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };
  const t0 = 1_000_000_000_000;

  writeCachedHandshake(2222, cwd, facts, fakeResult(), env, t0);
  assert.notEqual(readCachedHandshake(2222, cwd, facts, env, t0 + 1000), undefined);
  assert.notEqual(readCachedHandshake(2222, cwd, facts, env, t0 + HANDSHAKE_TTL_MS - 1), undefined);
  assert.equal(readCachedHandshake(2222, cwd, facts, env, t0 + HANDSHAKE_TTL_MS + 1), undefined);
});

test("cache: a future writtenAt (clock skew) is rejected, not treated as fresh", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };
  const t0 = 1_000_000_000_000;

  writeCachedHandshake(3333, cwd, facts, fakeResult(), env, t0);
  assert.equal(readCachedHandshake(3333, cwd, facts, env, t0 - 1000), undefined);
});

test("cache: a cwd mismatch is a miss", () => {
  const env = tmpCacheEnv();
  const cwdA = tmpDir();
  const cwdB = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  writeCachedHandshake(4444, cwdA, facts, fakeResult(), env);
  assert.equal(readCachedHandshake(4444, cwdB, facts, env), undefined);
  assert.notEqual(readCachedHandshake(4444, cwdA, facts, env), undefined);
});

test("cache: a factsHash mismatch (the project's facts changed) is a miss", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const factsAtWrite: ProjectFacts = { cwd_basename: "phoenix", remote_url: "https://github.com/acme/phoenix.git" };
  const factsNow: ProjectFacts = { cwd_basename: "phoenix", remote_url: "https://github.com/acme/other.git" };

  writeCachedHandshake(5555, cwd, factsAtWrite, fakeResult(), env);
  assert.equal(readCachedHandshake(5555, cwd, factsNow, env), undefined);
  assert.notEqual(readCachedHandshake(5555, cwd, factsAtWrite, env), undefined);
});

test("cache: pid isolation — two ppids under the same cache dir never cross", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };
  const resultA = fakeResult({ namespace: "acme/a" });
  const resultB = fakeResult({ namespace: "acme/b" });

  writeCachedHandshake(6001, cwd, facts, resultA, env);
  writeCachedHandshake(6002, cwd, facts, resultB, env);

  assert.equal(readCachedHandshake(6001, cwd, facts, env)?.namespace, "acme/a");
  assert.equal(readCachedHandshake(6002, cwd, facts, env)?.namespace, "acme/b");

  deleteCachedHandshake(6001, env);
  assert.equal(readCachedHandshake(6001, cwd, facts, env), undefined);
  // The other pid's record is untouched.
  assert.equal(readCachedHandshake(6002, cwd, facts, env)?.namespace, "acme/b");
});

test("cache: a malformed cache file degrades to a miss rather than throwing", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };
  const p = handshakeCachePath(7777, env);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, "{ not json");

  assert.equal(readCachedHandshake(7777, cwd, facts, env), undefined);
});

test("deleteCachedHandshake is idempotent", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  writeCachedHandshake(8888, cwd, facts, fakeResult(), env);
  deleteCachedHandshake(8888, env);
  assert.equal(readCachedHandshake(8888, cwd, facts, env), undefined);
  assert.doesNotThrow(() => deleteCachedHandshake(8888, env));
});

test("invalidateAllHandshakes removes only *.handshake.json, leaving pid-*.cwd files alone", () => {
  const env = tmpCacheEnv();
  const cwd = tmpDir();
  const facts: ProjectFacts = { cwd_basename: "phoenix" };

  writeCachedHandshake(9001, cwd, facts, fakeResult(), env);
  writeCachedHandshake(9002, cwd, facts, fakeResult(), env);
  writeSessionCwd(9001, cwd, env); // session.ts's sibling record type, same directory

  invalidateAllHandshakes(env);

  assert.equal(readCachedHandshake(9001, cwd, facts, env), undefined);
  assert.equal(readCachedHandshake(9002, cwd, facts, env), undefined);
  assert.equal(fs.existsSync(sessionCwdPath(9001, env)), true);
});

test("invalidateAllHandshakes on a machine with no sessions dir yet does not throw", () => {
  const env = tmpCacheEnv();
  assert.doesNotThrow(() => invalidateAllHandshakes(env));
});
