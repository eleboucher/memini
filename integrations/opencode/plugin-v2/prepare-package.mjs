import { cpSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";

const root = new URL(".", import.meta.url);
const source = new URL("../plugin/", root);
const dist = new URL("./dist/", root);
const manifest = JSON.parse(readFileSync(new URL("./package.json", root), "utf8"));

rmSync(dist, { recursive: true, force: true });
mkdirSync(dist, { recursive: true });
cpSync(new URL("./memini.js", source), new URL("./memini.js", dist));
cpSync(new URL("./memini-v2.js", source), new URL("./memini-v2.js", dist));
writeFileSync(new URL("./package.json", dist), JSON.stringify({ name: manifest.name, version: manifest.version }));
writeFileSync(
  new URL("./index.js", dist),
  'import { Plugin } from "@opencode-ai/plugin";\nimport { setup } from "./memini-v2.js";\n\nexport default Plugin.define({ id: "memini", setup });\n',
);
