// Builds the publishable dist/ for @eleboucher/opencode-memini-v2 from the
// shared sources in ../plugin. Run by `prepack`, so `npm publish` always ships
// a freshly staged copy (opencode runs no lifecycle scripts when INSTALLING a
// plugin, but npm still runs prepack when packing one).
//
// The entrypoint re-exports memini-v2.js's default rather than wrapping it in
// Plugin.define: `define` is an identity function upstream (it returns its
// argument unchanged), so the two are indistinguishable to the loader, and
// skipping it keeps the package free of runtime dependencies. That matters
// because opencode installs a plugin's production dependencies into an
// isolated cache — a dependency-free plugin has nothing to resolve, and no SDK
// version to keep matched against the host build.
import { cpSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";

const root = new URL(".", import.meta.url);
const source = new URL("../plugin/", root);
const dist = new URL("./dist/", root);
const manifest = JSON.parse(readFileSync(new URL("./package.json", root), "utf8"));

rmSync(dist, { recursive: true, force: true });
mkdirSync(dist, { recursive: true });
cpSync(new URL("./memini.js", source), new URL("./memini.js", dist));
cpSync(new URL("./memini-v2.js", source), new URL("./memini-v2.js", dist));
// This nested manifest shadows the root one for everything under dist/, so it
// must repeat "type": "module" — without it node reparses the ESM entrypoint as
// CommonJS and warns on every load.
writeFileSync(
  new URL("./package.json", dist),
  JSON.stringify({ name: manifest.name, version: manifest.version, type: "module" }),
);
writeFileSync(new URL("./index.js", dist), 'export { default } from "./memini-v2.js";\n');
