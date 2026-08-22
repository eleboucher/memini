import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import {
  credentialsPath,
  credentialKey,
  readStoredApiKey,
  syncStoredApiKey,
  applyCredentialFallback,
} from "../src/credential.js";
import { readBootstrap } from "../src/bootstrap.js";

/** A fresh XDG_CONFIG_HOME per test so nothing leaks between tests or machines. */
function freshEnv(): Record<string, string | undefined> {
  return { XDG_CONFIG_HOME: fs.mkdtempSync(path.join(os.tmpdir(), "memini-cred-")) };
}

test("credentialsPath: XDG_CONFIG_HOME wins, else ~/.config", () => {
  assert.equal(credentialsPath({ XDG_CONFIG_HOME: "/x" }), path.join("/x", "memini", "credentials"));
  assert.equal(credentialsPath({}), path.join(os.homedir(), ".config", "memini", "credentials"));
});

test("credentialKey: normalizes trailing slashes and whitespace", () => {
  assert.equal(credentialKey(" https://memini.example.com/ "), "https://memini.example.com");
  assert.equal(credentialKey("http://localhost:8080"), "http://localhost:8080");
});

test("sync + read roundtrip, keyed by normalized base URL", () => {
  const env = freshEnv();
  const r = syncStoredApiKey("https://memini.example.com/", "tok-abc", env);
  assert.equal(r.ok, true);
  assert.equal(r.action, "written");
  assert.equal(readStoredApiKey("https://memini.example.com", env), "tok-abc");
  // Second identical sync is a no-op.
  assert.equal(syncStoredApiKey("https://memini.example.com", "tok-abc", env).action, "unchanged");
});

test("file is 0600 and its directory 0700", { skip: process.platform === "win32" }, () => {
  const env = freshEnv();
  syncStoredApiKey("http://localhost:8080", "tok", env);
  const p = credentialsPath(env);
  assert.equal(fs.statSync(p).mode & 0o777, 0o600);
  assert.equal(fs.statSync(path.dirname(p)).mode & 0o777, 0o700);
});

test("readStoredApiKey: empty on miss, mismatched base URL, or corrupt file", () => {
  const env = freshEnv();
  assert.equal(readStoredApiKey("http://localhost:8080", env), "", "no file yet");
  syncStoredApiKey("https://a.example.com", "tok-a", env);
  assert.equal(readStoredApiKey("https://b.example.com", env), "", "other server's bearer must never travel");
  fs.writeFileSync(credentialsPath(env), "{not json");
  assert.equal(readStoredApiKey("https://a.example.com", env), "", "corrupt file is a miss, not a throw");
});

test("empty key retires the entry; an emptied file is removed", () => {
  const env = freshEnv();
  syncStoredApiKey("https://a.example.com", "tok-a", env);
  syncStoredApiKey("https://b.example.com", "tok-b", env);
  const r1 = syncStoredApiKey("https://a.example.com", "", env);
  assert.equal(r1.action, "removed");
  assert.equal(readStoredApiKey("https://a.example.com", env), "");
  assert.equal(readStoredApiKey("https://b.example.com", env), "tok-b", "unrelated server survives");
  const r2 = syncStoredApiKey("https://b.example.com", "", env);
  assert.equal(r2.action, "removed");
  assert.equal(fs.existsSync(credentialsPath(env)), false, "empty file deleted, not left as {}");
  assert.equal(syncStoredApiKey("https://b.example.com", "", env).action, "skipped", "nothing to retire");
});

test("applyCredentialFallback: env wins, file fills, none when neither", () => {
  const env = freshEnv();
  syncStoredApiKey("http://localhost:8080", "tok-file", env);
  const withEnvKey = applyCredentialFallback(readBootstrap({ MEMINI_API_KEY: "tok-env", ...env }), env);
  assert.equal(withEnvKey.source, "env");
  assert.equal(withEnvKey.boot.apiKey, "tok-env");
  const fromFile = applyCredentialFallback(readBootstrap({ ...env }), env);
  assert.equal(fromFile.source, "file");
  assert.equal(fromFile.boot.apiKey, "tok-file");
  const none = applyCredentialFallback(readBootstrap({ MEMINI_BASE_URL: "https://other.example.com", ...env }), env);
  assert.equal(none.source, "none");
  assert.equal(none.boot.apiKey, "");
});
