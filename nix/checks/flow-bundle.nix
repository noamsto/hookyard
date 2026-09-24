# The serve flow view (#97) is a committed esbuild bundle under
# internal/serve/assets/flow/, source in internal/serve/web/ — not built at
# hookyard's build time (SPEC 4.8). A committed generated artefact drifts
# from its source unless something checks: this rebuilds the bundle from
# scratch and diffs it against what's checked in.
{pkgs}: let
  web = ../../internal/serve/web;
  nodejs = pkgs.nodejs_24 or pkgs.nodejs;
  # node --test type-strips .ts natively from this version on.
  checkedNodejs = assert pkgs.lib.versionAtLeast nodejs.version "22.6"; nodejs;
in
  pkgs.buildNpmPackage {
    pname = "hookyard-flow-bundle-check";
    version = "0.1.0";
    src = web;
    npmDeps = pkgs.importNpmLock {npmRoot = web;};
    npmConfigHook = pkgs.importNpmLock.npmConfigHook;
    nodejs = checkedNodejs;

    buildPhase = ''
      runHook preBuild
      npm run typecheck
      npm test
      node build.mjs --outdir "$TMPDIR/flow"
      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall
      if ! diff -r ${../../internal/serve/assets/flow} "$TMPDIR/flow"; then
        echo "internal/serve/assets/flow/ is stale relative to internal/serve/web/ — re-run:" >&2
        echo "  cd internal/serve/web && npm ci && npm run build" >&2
        exit 1
      fi
      touch $out
      runHook postInstall
    '';
  }
