// Builds the committed bundle under internal/serve/assets/flow/ (SPEC 4.8:
// the flow view is a committed esbuild bundle, checked against a fresh
// rebuild by nix/checks/flow-bundle.nix).
import { build } from "esbuild";
import { mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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

// The d3 modules carry no licence header in their sources, and BSD-3 wants
// its notice shipped with the bundle: every package that made it in gets its
// LICENSE text in flow.LICENSE, and flow.js a banner pointing there.
const js = join(outdir, "flow.js");
const out = Object.entries(result.metafile.outputs).find(([path]) => path.endsWith("flow.js"))[1];
const pkgs = new Set();
for (const [path, { bytesInOutput }] of Object.entries(out.inputs)) {
  const m = path.match(/node_modules\/((?:@[^/]+\/)?[^/]+)\//);
  if (m && bytesInOutput > 0) pkgs.add(m[1]);
}
const notices = [...pkgs].sort().map((name) => {
  const dir = join("node_modules", name);
  const pkg = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
  const file = readdirSync(dir).find((f) => /^licen[cs]e(\.|$)/i.test(f));
  if (!file) throw new Error(`${name}: no LICENSE file to ship`);
  const repo = typeof pkg.repository === "string" ? pkg.repository : pkg.repository?.url ?? "";
  const head = `${name} ${pkg.version} (${pkg.license})` + (repo ? ` — ${repo.replace(/^git\+/, "")}` : "");
  return { name: `${name} ${pkg.version}`, text: `${head}\n\n${readFileSync(join(dir, file), "utf8").trim()}\n` };
});
if (notices.length > 0) {
  writeFileSync(join(outdir, "flow.LICENSE"), notices.map((n) => n.text).join("\n" + "-".repeat(72) + "\n\n"));
  const names = notices.map((n) => n.name).join(", ");
  writeFileSync(js, `/*! Bundles ${names}; licences in flow.LICENSE */\n` + readFileSync(js, "utf8"));
}
