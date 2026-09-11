# internal/render/pi_bridge.ts is a plain-ESM-JavaScript template under a
# .ts name, go:embed-ed straight into the binary — the Go compiler never
# parses it. Without this check a syntactically broken bridge (a stray type
# annotation, a dangling brace) ships the moment `go build` and `go test` go
# green on a machine with no node, and Pi only discovers it at hook-fire
# time, in-process, on the fail-open path docs/design/hookyard.md §5 exists
# to guard. `node --check` is the cheapest thing that actually parses the
# file the way Pi will.
{pkgs}:
pkgs.runCommand "hookyard-pi-bridge-check" {} ''
  set -euo pipefail
  ${pkgs.nodejs}/bin/node --input-type=module --check < ${../../internal/render/pi_bridge.ts}
  touch $out
''
