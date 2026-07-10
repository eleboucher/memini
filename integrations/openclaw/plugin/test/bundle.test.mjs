// Run: node --test (from this directory), after `pnpm run build`.
//
// Guards against the class of packaging bug where a plugin ships a runtime
// dependency on an unpublished workspace package (see issue #34): the clawpack
// is installed with a plain `npm install` under whatever package manager the
// host runtime uses, so any `workspace:`/`file:`/`link:` spec — or any
// `@memini/*` package that only exists inside this monorepo — makes the whole
// plugin uninstallable. The build bundles those internal packages into
// dist/index.js instead; these tests fail loudly if that ever regresses.
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";

const pkg = JSON.parse(
  readFileSync(new URL("../package.json", import.meta.url), "utf8"),
);
const distUrl = new URL("../dist/index.js", import.meta.url);

test("package.json declares only installable, published specs", () => {
  for (const field of ["dependencies", "devDependencies", "peerDependencies"]) {
    for (const [name, spec] of Object.entries(pkg[field] ?? {})) {
      assert.ok(
        !/^(workspace|file|link):/.test(spec),
        `${field}.${name} = "${spec}" is a monorepo-only spec that a host's npm install cannot resolve`,
      );
      assert.ok(
        !name.startsWith("@memini/"),
        `${field}.${name} depends on an unpublished workspace package; bundle it into dist/ instead of declaring it`,
      );
    }
  }
});

test("dist bundle inlines workspace packages (no bare @memini/* import)", (t) => {
  // The build (esbuild) inlines @memini/* workspace packages; this only runs
  // against a built dist/. CI always builds before testing, so the guard fires
  // there; a source-only local run without a build skips it rather than failing.
  if (!existsSync(distUrl)) {
    t.skip("dist/index.js not built");
    return;
  }
  const code = readFileSync(distUrl, "utf8");
  assert.ok(
    !/["']@memini\//.test(code),
    "dist/index.js still imports an @memini/* package; the build must bundle it",
  );
});
