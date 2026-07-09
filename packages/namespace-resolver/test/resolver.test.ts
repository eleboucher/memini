import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import {
  resolveNamespace,
  applyTemplate,
  readConfig,
  repoNameFromRemote,
  repoSlugFromRemote,
  DEFAULT_TEMPLATE,
} from "../src/index.js";

// Helper: create a temp config file, return path
function tempConfig(config: object): string {
  const configPath = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "mnr-")), "config.json");
  fs.writeFileSync(configPath, JSON.stringify(config));
  return configPath;
}

// Helper: create a temp project dir under parent
function projectDir(parent: string, name: string): string {
  const p = path.join(parent, name);
  fs.mkdirSync(p, { recursive: true });
  return p;
}

// ─── applyTemplate ─────────────────────────────────────────────────

describe("applyTemplate", () => {
  it("fills all segments", () => {
    assert.equal(
      applyTemplate("{tenant}/{project}/{agent}", {
        tenant: "work",
        project: "memini",
        agent: "orchestrator",
      }),
      "work/memini/orchestrator",
    );
  });

  it("drops unresolvable segments with slash collapse", () => {
    assert.equal(
      applyTemplate("{tenant}/{project}/{agent}", {
        tenant: "work",
        project: "memini",
      }),
      "work/memini",
    );
  });

  it("drops leading+trailing segments cleanly", () => {
    assert.equal(
      applyTemplate("{tenant}/{project}/{agent}", { agent: "miso" }),
      "miso",
    );
  });

  it("handles {namespace} segment (OpenClaw pattern)", () => {
    assert.equal(
      applyTemplate("{namespace}/{agent}", {
        namespace: "work/openclaw",
        agent: "miso",
      }),
      "work/openclaw/miso",
    );
  });

  it("handles dashed template (backward compat)", () => {
    assert.equal(
      applyTemplate("{namespace}-{agent}", {
        namespace: "openclaw",
        agent: "miso",
      }),
      "openclaw-miso",
    );
  });

  it("handles {tenant}/{project} without agent", () => {
    assert.equal(
      applyTemplate("{tenant}/{project}", {
        tenant: "personal",
        project: "honcho",
      }),
      "personal/honcho",
    );
  });

  it("produces empty string when no segments resolve", () => {
    assert.equal(applyTemplate("{tenant}/{project}/{agent}", {}), "");
  });

  it("handles single-segment templates", () => {
    assert.equal(applyTemplate("{project}", { project: "memini" }), "memini");
    assert.equal(applyTemplate("{agent}", { agent: "miso" }), "miso");
    assert.equal(applyTemplate("{project}", {}), "");
  });

  it("preserves nested slashes in namespace segment", () => {
    assert.equal(
      applyTemplate("{namespace}/{agent}", {
        namespace: "work/openclaw",
        agent: "saffron",
      }),
      "work/openclaw/saffron",
    );
  });
});

// ─── repoNameFromRemote / repoSlugFromRemote ───────────────────────

describe("repoNameFromRemote", () => {
  it("parses https URL", () => {
    assert.equal(repoNameFromRemote("https://github.com/eleboucher/memini.git"), "memini");
  });

  it("parses ssh URL", () => {
    assert.equal(repoNameFromRemote("git@github.com:eleboucher/memini.git"), "memini");
  });

  it("parses scp-style URL", () => {
    assert.equal(repoNameFromRemote("git@github.com:eleboucher/memini"), "memini");
  });

  it("returns null for empty", () => {
    assert.equal(repoNameFromRemote(""), null);
    assert.equal(repoNameFromRemote(null as unknown as string), null);
  });
});

describe("repoSlugFromRemote", () => {
  it("produces owner-repo slug", () => {
    assert.equal(repoSlugFromRemote("https://github.com/eleboucher/memini.git"), "eleboucher-memini");
  });

  it("falls back to bare repo for single segment", () => {
    assert.equal(repoSlugFromRemote("memini"), "memini");
  });
});

// ─── readConfig ────────────────────────────────────────────────────

describe("readConfig", () => {
  it("returns empty config for missing file", () => {
    const cfg = readConfig("/nonexistent/path/config.json");
    assert.deepEqual(cfg.tenantRoots, []);
    assert.equal(cfg.template, DEFAULT_TEMPLATE);
    assert.deepEqual(cfg.overrides, {});
  });

  it("returns empty config for malformed JSON", () => {
    const cfg = readConfig("/dev/null");
    assert.deepEqual(cfg.tenantRoots, []);
    assert.equal(cfg.template, DEFAULT_TEMPLATE);
  });
});

// ─── resolveNamespace ──────────────────────────────────────────────

describe("resolveNamespace", () => {
  it("MEMINI_NAMESPACE env wins immediately (backward compat)", () => {
    const result = resolveNamespace({
      cwd: "/tmp",
      env: { MEMINI_NAMESPACE: "custom-ns" },
    });
    assert.equal(result.namespace, "custom-ns");
    assert.equal(result.source, "env");
  });

  it("falls back to project-only when no config file (backward compat)", () => {
    // No config file → no tenant → project from cwd basename
    const result = resolveNamespace({
      cwd: "/home/user/dev/memini",
      env: {},
      configPath: "/nonexistent/config.json",
    });
    // cwd basename is "memini", but git may override — just check it's non-empty
    assert.ok(result.namespace.length > 0);
    assert.ok(result.segments.project !== undefined || result.segments.tenant !== undefined ||
              result.namespace.length > 0);
  });

  it("resolves tenant from config tenant roots", () => {
    const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), "mnr-tenant-"));
    const workDir = projectDir(tmpRoot, "work");
    const projectDir2 = projectDir(workDir, "memini");
    const configPath = tempConfig({
      tenantRoots: [{ path: workDir, tenant: "work" }],
      template: "{tenant}/{project}/{agent}",
    });

    const result = resolveNamespace({
      cwd: projectDir2,
      env: {},
      configPath,
    });
    assert.equal(result.segments.tenant, "work");
    assert.equal(result.source, "config");
    assert.ok(result.namespace.startsWith("work/"));
  });

  it("resolves agent from agentId (config file present)", () => {
    const result = resolveNamespace({
      cwd: "/tmp/some-project",
      env: {},
      configPath: tempConfig({}),
      agentId: "orchestrator",
    });
    assert.equal(result.segments.agent, "orchestrator");
    // Default template includes {agent} — but if project is also set, agent appends
    // Without a real git repo, project = basename(cwd) = "some-project"
    assert.ok(result.namespace.includes("orchestrator"));
  });

  it("resolves agent from MEMINI_AGENT env (config file present)", () => {
    const result = resolveNamespace({
      cwd: "/tmp/myproject",
      env: { MEMINI_AGENT: "reviewer" },
      configPath: tempConfig({}),
    });
    assert.equal(result.segments.agent, "reviewer");
  });

  it("per-integration override template (OpenClaw pattern)", () => {
    const configPath = tempConfig({
      template: "{tenant}/{project}/{agent}",
      overrides: {
        openclaw: {
          template: "{namespace}/{agent}",
          namespace: "work/openclaw",
        },
      },
    });

    const result = resolveNamespace({
      cwd: "/tmp/whatever",
      env: {},
      configPath,
      agentId: "miso",
      integration: "openclaw",
    });
    assert.equal(result.namespace, "work/openclaw/miso");
  });

  it("OpenClaw backward compat: dashed template", () => {
    const configPath = tempConfig({
      template: "{tenant}/{project}/{agent}",
      overrides: {
        openclaw: {
          template: "{namespace}-{agent}",
          namespace: "openclaw",
        },
      },
    });

    const result = resolveNamespace({
      cwd: "/tmp",
      env: {},
      configPath,
      agentId: "miso",
      integration: "openclaw",
    });
    assert.equal(result.namespace, "openclaw-miso");
  });

  it("no agent → segment dropped (no trailing slash)", () => {
    const result = resolveNamespace({
      cwd: "/tmp/memini",
      env: {},
      configPath: "/nonexistent/config.json",
    });
    assert.ok(!result.namespace.endsWith("/"), `namespace should not end with /, got: ${result.namespace}`);
    assert.equal(result.segments.agent, undefined);
  });

  it("no config file → today's behavior (backward compat)", () => {
    // This is the critical backward-compat test: with no config file,
    // the resolver should behave exactly like today's per-integration logic:
    // env > git > cwd basename, no tenant prefix.
    const result = resolveNamespace({
      cwd: "/tmp/test-project-xyz",
      env: {},
      configPath: "/nonexistent/config.json",
    });
    assert.equal(result.segments.tenant, undefined);
    // namespace should be the project name, NOT prefixed with a tenant
    assert.ok(result.namespace.length > 0);
    assert.ok(!result.namespace.startsWith("work/") || result.segments.tenant === "work",
              `should not have tenant prefix without config: ${result.namespace}`);
  });

  it("sanitizes agent IDs with special chars", () => {
    const result = resolveNamespace({
      cwd: "/tmp/project",
      env: {},
      configPath: tempConfig({}),
      agentId: "agent with spaces & symbols!",
    });
    assert.ok(result.segments.agent);
    assert.ok(!result.segments.agent!.includes(" "));
    assert.ok(!result.segments.agent!.includes("!"));
  });

  it("handles personal vs work tenant isolation", () => {
    const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), "mnr-iso-"));
    const workRoot = path.join(tmpRoot, "work");
    const personalRoot = path.join(tmpRoot, "personal");
    const workProject = projectDir(workRoot, "memini");
    const personalProject = projectDir(personalRoot, "honcho");
    const configPath = tempConfig({
      tenantRoots: [
        { path: workRoot, tenant: "work" },
        { path: personalRoot, tenant: "personal" },
      ],
      template: "{tenant}/{project}",
    });

    const workResult = resolveNamespace({ cwd: workProject, env: {}, configPath });
    const personalResult = resolveNamespace({ cwd: personalProject, env: {}, configPath });

    assert.equal(workResult.segments.tenant, "work");
    assert.ok(workResult.namespace.startsWith("work/"));

    assert.equal(personalResult.segments.tenant, "personal");
    assert.ok(personalResult.namespace.startsWith("personal/"));

    // Critical: no cross-contamination
    assert.notEqual(workResult.segments.tenant, personalResult.segments.tenant);
    assert.ok(!personalResult.namespace.startsWith("work/"));
    assert.ok(!workResult.namespace.startsWith("personal/"));
  });

  // ─── No-config legacy path (zero-migration guarantee) ─────────────

  it("no config file → cwd basename even inside a git repo with a remote", () => {
    // Pre-config Pi derived the namespace from the cwd basename only; with no
    // config file the resolver must not prefer the git remote name, or
    // existing users' namespaces get silently renamed.
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "mnr-git-"));
    execSync("git init -q", { cwd: dir });
    execSync("git remote add origin https://github.com/acme/other-name.git", { cwd: dir });
    const result = resolveNamespace({
      cwd: dir,
      env: {},
      configPath: "/nonexistent/config.json",
    });
    assert.equal(result.namespace, path.basename(dir));
    assert.equal(result.source, "cwd");
  });

  it("no config file → agentId does not append a segment", () => {
    const result = resolveNamespace({
      cwd: "/tmp/legacy-project",
      env: {},
      configPath: "/nonexistent/config.json",
      agentId: "orchestrator",
    });
    assert.equal(result.namespace, "legacy-project");
    assert.equal(result.segments.agent, undefined);
  });

  it("no config file → MEMINI_AGENT does not append a segment", () => {
    const result = resolveNamespace({
      cwd: "/tmp/legacy-project",
      env: { MEMINI_AGENT: "reviewer" },
      configPath: "/nonexistent/config.json",
    });
    assert.equal(result.namespace, "legacy-project");
    assert.equal(result.segments.agent, undefined);
  });

  it("no config file → MEMINI_NAMESPACE env still wins", () => {
    const result = resolveNamespace({
      cwd: "/tmp/legacy-project",
      env: { MEMINI_NAMESPACE: "forced-ns" },
      configPath: "/nonexistent/config.json",
    });
    assert.equal(result.namespace, "forced-ns");
    assert.equal(result.source, "env");
  });

  it("readConfig reports found=true for a present file, false for a missing one", () => {
    assert.equal(readConfig(tempConfig({})).found, true);
    assert.equal(readConfig("/nonexistent/config.json").found, false);
  });
});
