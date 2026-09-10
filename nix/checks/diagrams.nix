# Re-renders every docs/diagrams/*.d2 source with the pinned theme pair and
# fails if the committed SVG differs from the fresh render. d2's sketch mode
# is deterministic (no per-render random seed), so a stale SVG is the only
# thing that should ever fail this — a generated artifact drifts from its
# source unless something checks.
{pkgs}: let
  diagramsDir = ../../docs/diagrams;
  names =
    map (pkgs.lib.removeSuffix ".d2")
    (builtins.filter (pkgs.lib.hasSuffix ".d2")
      (builtins.attrNames (builtins.readDir diagramsDir)));

  checkOne = name: ''
    ${pkgs.d2}/bin/d2 --theme 0 --dark-theme 200 ${diagramsDir}/${name}.d2 "$TMPDIR"/${name}.svg
    if ! diff -q ${diagramsDir}/${name}.svg "$TMPDIR"/${name}.svg >/dev/null; then
      echo "docs/diagrams/${name}.svg is stale relative to ${name}.d2 — re-run:" >&2
      echo "  d2 --theme 0 --dark-theme 200 docs/diagrams/${name}.d2 docs/diagrams/${name}.svg" >&2
      diff ${diagramsDir}/${name}.svg "$TMPDIR"/${name}.svg >&2 || true
      exit 1
    fi
  '';
in
  pkgs.runCommand "hookyard-diagrams-check" {} ''
    set -euo pipefail
    ${pkgs.lib.concatMapStringsSep "\n" checkOne names}
    touch $out
  ''
