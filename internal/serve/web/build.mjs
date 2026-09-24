// Builds the committed bundle under internal/serve/assets/flow/ (SPEC 4.8:
// the flow view is a committed esbuild bundle, checked against a fresh
// rebuild by nix/checks/flow-bundle.nix).
import { build } from "esbuild";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const outdirArg = process.argv.indexOf("--outdir");
const outdir = outdirArg !== -1 ? process.argv[outdirArg + 1] : "../assets/flow";

rmSync(outdir, { recursive: true, force: true });
mkdirSync(outdir, { recursive: true });

const result = await build({
  entryPoints: [{ in: "src/main.ts", out: "flow" }],
  bundle: true,
  format: "esm",
  minify: true,
  legalComments: "eof",
  external: ["../app.js"],
  target: "es2022",
  outdir,
  metafile: true,
});

// The d3 modules carry no licence header in their sources — name every
// package that made it into the bundle, with its licence, up front.
const js = join(outdir, "flow.js");
const out = Object.entries(result.metafile.outputs).find(([path]) => path.endsWith("flow.js"))[1];
const pkgs = new Set();
for (const [path, { bytesInOutput }] of Object.entries(out.inputs)) {
  const m = path.match(/node_modules\/((?:@[^/]+\/)?[^/]+)\//);
  if (m && bytesInOutput > 0) pkgs.add(m[1]);
}
const notes = [...pkgs].sort().map((name) => {
  const pkg = JSON.parse(readFileSync(join("node_modules", name, "package.json"), "utf8"));
  return `${name} ${pkg.version} (${pkg.license})`;
});
if (notes.length > 0) {
  writeFileSync(js, `/*! ${notes.join(", ")} | https://github.com/d3 */\n` + readFileSync(js, "utf8"));
}
