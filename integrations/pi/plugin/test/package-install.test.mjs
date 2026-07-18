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
    assert.equal(manifest.peerDependencies?.["@earendil-works/pi-coding-agent"], ">=0.80.6");
    assert.equal(manifest.peerDependencies?.["@earendil-works/pi-tui"], ">=0.80.6");
    assert.equal(manifest.peerDependencies?.typebox, ">=1.1.38");

    const bundle = await readFile(join(installedDir, "dist", "index.js"), "utf8");
    assert.doesNotMatch(bundle, /["']@memini\//, "workspace packages must be bundled into dist");

    // Pi provides these core modules. Minimal host stubs prove the packed ESM
    // links only against the declared peers and no checkout-relative imports.
    await writePeerStub(consumerDir, "typebox", "export const Type = {};\n");
    await writePeerStub(consumerDir, "@earendil-works/pi-tui", "export class Text {}\n");
    await import(pathToFileURL(join(installedDir, "dist", "index.js")).href);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});
