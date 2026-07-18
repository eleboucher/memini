import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { promisify } from "node:util";
import test from "node:test";

const execFileAsync = promisify(execFile);
const pluginDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");

async function writePeerStub(root, name, source) {
  const packageDir = join(root, "node_modules", ...name.split("/"));
  await mkdir(packageDir, { recursive: true });
  await writeFile(
    join(packageDir, "package.json"),
    JSON.stringify({ name, version: "999.0.0", type: "module", exports: "./index.js" }),
  );
  await writeFile(join(packageDir, "index.js"), source);
}

test("the packed Pi package installs and loads without monorepo files", async () => {
  const tempDir = await mkdtemp(join(tmpdir(), "pi-memini-package-"));
  const packDir = join(tempDir, "pack");
  const consumerDir = join(tempDir, "consumer");

  try {
    await mkdir(packDir);
    await mkdir(consumerDir);
    await writeFile(
      join(consumerDir, "package.json"),
      JSON.stringify({ name: "pi-memini-package-test", private: true, type: "module" }),
    );

    const { stdout } = await execFileAsync(
      "npm",
      ["pack", "--json", "--ignore-scripts", "--pack-destination", packDir],
      { cwd: pluginDir },
    );
    const [packed] = JSON.parse(stdout);
    const tarball = join(packDir, packed.filename);
    const packedPaths = new Set(packed.files.map((file) => file.path));

    assert.deepEqual(
      [...packedPaths].sort(),
      ["dist/index.js", "package.json"],
      "the publication must contain only the fresh bundle and manifest",
    );

    await execFileAsync(
      "npm",
      ["install", "--ignore-scripts", "--legacy-peer-deps", tarball],
      { cwd: consumerDir },
    );

    const installedDir = join(consumerDir, "node_modules", "@eleboucher", "pi-memini");
    const manifest = JSON.parse(await readFile(join(installedDir, "package.json"), "utf8"));
    assert.deepEqual(manifest.pi?.extensions, ["./dist/index.js"]);
    assert.deepEqual(manifest.dependencies ?? {}, {}, "Pi-provided modules must stay peers, not runtime copies");
    assert.equal(manifest.peerDependencies?.["@earendil-works/pi-coding-agent"], "*");
    assert.equal(manifest.peerDependencies?.["@earendil-works/pi-tui"], "*");
    assert.equal(manifest.peerDependencies?.typebox, "*");

    const bundle = await readFile(join(installedDir, "dist", "index.js"), "utf8");
    assert.doesNotMatch(bundle, /["']@memini\//, "workspace packages must be bundled into dist");

    // Pi provides these core modules. Functional host stubs prove both that the
    // packed ESM links and that Pi can invoke its factory to register schemas,
    // tools, renderers, commands, and lifecycle handlers.
    await writePeerStub(consumerDir, "typebox", `
const make = (kind, args) => ({ kind, args });
export const Type = new Proxy({}, { get: (_target, kind) => (...args) => make(String(kind), args) });
`);
    await writePeerStub(consumerDir, "@earendil-works/pi-tui", `
export class Text {
  constructor(text = "") { this.text = text; }
  render() { return [this.text]; }
  invalidate() {}
}
`);
    const extension = await import(pathToFileURL(join(installedDir, "dist", "index.js")).href);
    const tools = [];
    const events = [];
    const commands = [];
    extension.default({
      on(name) { events.push(name); },
      registerTool(tool) { tools.push(tool.name); },
      registerCommand(name) { commands.push(name); },
      registerMessageRenderer() {},
      registerEntryRenderer() {},
      appendEntry() {},
      sendMessage() {},
    });
    assert.deepEqual(tools.sort(), [
      "memory_briefing", "memory_forget", "memory_get", "memory_history",
      "memory_list", "memory_recall", "memory_remember", "memory_update",
    ]);
    for (const event of [
      "session_start", "session_tree", "session_before_compact", "session_compact",
      "session_shutdown", "before_agent_start", "agent_settled", "message_end",
    ]) assert.ok(events.includes(event), `missing lifecycle handler ${event}`);
    assert.deepEqual(commands.sort(), ["memini:namespace", "memini:status"]);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});
