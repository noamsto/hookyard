#!/usr/bin/env bash
# Prepare the Codex advisory probe — issue #87.
#
#   docs/design/fixtures/codex-advisory/run.sh
#
# Builds a throwaway CODEX_HOME so nothing here touches ~/.codex, writes the
# hooks.json that answers SessionStart and UserPromptSubmit with
# additionalContext, and prints the launch line. It does NOT launch Codex: the
# only Codex invocation this fleet has proven is interactive in a tmux pane
# (`codex --profile worker ...`, adapters/core/dispatch.sh:443), this fixture's
# author had no Codex host to run it on, and inventing a headless form here
# would make an unrun probe look like a tested one.
#
# UNRUN, deliberately. See README.md for what to observe and where the result
# belongs. Read-only against the repo; the only writes are under a temp dir.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
scratch=$(mktemp -d)
home=$scratch/codex-home
log=$scratch/hook-log.jsonl
marker="PROBE-ADVISORY-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"

mkdir -p "$home"

# One handler per event, both routed at the same script. Absolute paths: Codex
# runs a hook command with the session cwd, not this one. Matchers are omitted
# on UserPromptSubmit on purpose — Codex documents that event as not supporting
# them, and a matcher there would be ignored rather than honoured.
cat >"$home/hooks.json" <<JSON
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear",
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh $marker $log", "timeout": 10}]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh $marker $log", "timeout": 10}]
      }
    ]
  }
}
JSON

cat <<BANNER
Codex advisory probe (issue #87) — prepared, not run.

  CODEX_HOME  $home
  marker      $marker
  hook log    $log

Marker text for the model to look for:
  $marker

Launch Codex against the probe home. The profile is the fleet's own, so this
matches how a worker gets started rather than a shape invented for the probe:

  CODEX_HOME=$home codex --profile worker --dangerously-bypass-hook-trust

Then, at the first prompt, ask for the marker by name only — never paste it:

  Reply with the PROBE-ADVISORY marker from your context, or NO-MARKER if you
  have none.

--dangerously-bypass-hook-trust is Codex's documented path for one-off
automation whose hook sources are already vetted. Without it Codex skips an
unreviewed hook, and the probe would measure nothing while looking like a
negative result.

BANNER

read -r -p "press enter once that Codex turn is done > " _ || true

echo
echo "--- hook log ($log) ---"
if [[ -s $log ]]; then
  jq -r '"fired: \(.hook_event_name)  session: \(.session_id // "-")"' <"$log" || cat "$log"
else
  echo "EMPTY — no hook fired. Not an answer to the advisory question: check"
  echo "trust and CODEX_HOME before reading anything into it."
fi
echo
echo "Now read the transcript: did the reply contain the marker?"
echo "  yes -> Codex delivers additionalContext; capability.go:71 and the"
echo "         hookyard.md:2334 table must both be corrected, from this run."
echo "  no  -> record the negative in README.md beside the marker and the"
echo "         Codex version. The boundary finally rests on a measurement."
echo
echo "Scratch dir kept for the record: $scratch"