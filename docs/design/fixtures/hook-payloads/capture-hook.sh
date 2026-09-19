#!/usr/bin/env bash
# Capture hook for the hookyard payload probe.
#   $1 engine label, $2 event label, $3 mode (observe|deny|tick), $4 out file or dir
# Records each firing under $4 — one appended JSONL record, or one file per
# event when $4 is a directory — then answers per mode. The out file
# is an argument, not just an env var: engines may not pass their own env to a
# hook subprocess, and a capture that silently wrote nowhere would look like a
# hook that never fired.
set -uo pipefail

engine=${1:?engine}
event=${2:?event}
mode=${3:-observe}
out=${4:-${CAPTURE_OUT:-}}
if [[ -z $out ]]; then
  echo "capture-hook: no out file given as \$4 or CAPTURE_OUT" >&2
  exit 1
fi

payload=$(cat)

# A directory as $4 selects one file per event instead of one appended record
# per firing, and $2 is then only a fallback name. Pi's capture runs through
# hookyard's own bridge in the router position rather than through an engine's
# hook protocol, so the payload on stdin already is the fixture — appending it
# into a shared log would only mean taking it apart again, once per event.
if [[ -d $out ]]; then
  name=$(jq -r --arg event "$event" '.hook_event_name // $event' <<<"$payload")
  # First firing per event wins. A denied call is answered by the model trying
  # something else, so last-write-wins would leave the tool_call fixture holding
  # whatever it retried with rather than the call the probe prompt asked for.
  [[ -e $out/$engine-$name.json ]] || printf '%s\n' "$payload" >"$out/$engine-$name.json"
else
  jq -c -n \
    --arg engine "$engine" \
    --arg event "$event" \
    --arg mode "$mode" \
    --arg cwd "$PWD" \
    --arg raw "$payload" \
    '{engine: $engine, event: $event, mode: $mode, cwd: $cwd, raw: $raw}' \
    >>"$out"
fi

case $mode in
  deny)
    case $engine in
      cursor) printf '%s\n' '{"permission":"deny","user_message":"hookyard probe deny"}' ;;
      # Pi's verdict is hookyard's own router reply, not an engine protocol:
      # the bridge is what reads it, and it blocks only on block:true.
      pi)     printf '%s\n' '{"block":true,"reason":"hookyard probe deny"}' ;;
      *)      printf '%s\n' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"hookyard probe deny"}}' ;;
    esac
    ;;
  tick)
    # Emit one tick per second until the engine kills us. The last tick recorded
    # is the engine's own cutoff, which is how an undeclared default timeout gets
    # measured rather than guessed.
    for i in $(seq 1 180); do
      printf '%s %s\n' "$i" "$(date +%s.%N)" >>"$out.ticks"
      sleep 1
    done
    ;;
esac
exit 0
