# Re-renders every docs/diagrams/*.d2 source, light and dark, and fails if
# either committed SVG differs from its fresh render. d2's sketch mode is
# deterministic (no per-render random seed), so a stale SVG is the only thing
# that should ever fail this — a generated artifact drifts from its source
# unless something checks.
#
# Light and dark are rendered as separate files, not one file with
# --dark-theme: a <picture> element's per-theme <source> is what lets a
# GitHub README follow the page's own theme toggle, and that only works with
# two standalone SVGs — a single file's embedded prefers-color-scheme media
# query sees the browser's preference, never GitHub's toggle.
{pkgs}: let
  diagramsDir = ../../docs/diagrams;
  names =
    map (pkgs.lib.removeSuffix ".d2")
    (builtins.filter (pkgs.lib.hasSuffix ".d2")
      (builtins.attrNames (builtins.readDir diagramsDir)));

  checkVariant = name: variant: theme: ''
    ${pkgs.d2}/bin/d2 --theme ${theme} ${diagramsDir}/${name}.d2 "$TMPDIR"/${variant}.svg
    if ! diff -q ${diagramsDir}/${variant}.svg "$TMPDIR"/${variant}.svg >/dev/null; then
      echo "docs/diagrams/${variant}.svg is stale relative to ${name}.d2 — re-run:" >&2
      echo "  d2 --theme ${theme} docs/diagrams/${name}.d2 docs/diagrams/${variant}.svg" >&2
      diff ${diagramsDir}/${variant}.svg "$TMPDIR"/${variant}.svg >&2 || true
      exit 1
    fi
  '';
  checkOne = name: (checkVariant name name "0") + (checkVariant name "${name}-dark" "200");
in
  pkgs.runCommand "hookyard-diagrams-check" {} ''
    set -euo pipefail
    ${pkgs.lib.concatMapStringsSep "\n" checkOne names}
    touch $out
  ''
