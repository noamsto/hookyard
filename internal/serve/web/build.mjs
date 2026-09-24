// Builds the committed bundle under internal/serve/assets/flow/ (SPEC 4.8:
// the flow view is a committed esbuild bundle, checked against a fresh
// rebuild by nix/checks/flow-bundle.nix).
import { build } from "esbuild";
import { readFileSync, copyFileSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);

const outdirArg = process.argv.indexOf("--outdir");
const outdir = outdirArg !== -1 ? process.argv[outdirArg + 1] : "../assets/flow";

rmSync(outdir, { recursive: true, force: true });
mkdirSync(outdir, { recursive: true });

await build({
  entryPoints: [{ in: "src/main.tsx", out: "flow" }],
  bundle: true,
  format: "esm",
  minify: true,
  legalComments: "eof",
  define: { "process.env.NODE_ENV": '"production"' },
  external: ["../app.js"],
  jsx: "automatic",
  target: "es2022",
  outdir,
});

// elkjs's minified worker carries no licence header upstream — prepend one,
// byte-for-byte otherwise.
const elkPkg = JSON.parse(readFileSync(require.resolve("elkjs/package.json"), "utf8"));
const banner = `/*! elkjs ${elkPkg.version} | EPL-2.0 | https://github.com/kieler/elkjs */\n`;
const worker = readFileSync(require.resolve("elkjs/lib/elk-worker.min.js"), "utf8");
writeFileSync(`${outdir}/elk-worker.js`, banner + worker);
copyFileSync(require.resolve("elkjs/LICENSE.md"), `${outdir}/elk-worker.LICENSE`);
