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
  # set -uo pipefail (no -e): a jq parse failure exits non-zero and prints
  # nothing to stdout, so an unchecked call would silently write a
  # mislabelled $engine-.json fixture instead of failing loudly.
  if ! name=$(jq -r --arg event "$event" '.hook_event_name // $event' <<<"$payload"); then
    echo "capture-hook: jq failed to parse payload for $engine/$event" >&2
    exit 1
  fi
  # $name is a fixed protocol enum per engine (hookyard's own validated event
  # name for pi), not realistically attacker-controlled -- this is a one-line
  # path-safety guard, not a fix for a real vulnerability. Quoting already
  # stops word-splitting/globbing; this stops a "/" or ".." from escaping $out.
  case $name in
    */*|*..*)
      echo "capture-hook: unsafe hook_event_name from payload: $name" >&2
      exit 1
      ;;
  esac
  # First firing per event wins. A denied call is answered by the model trying
  # something else, so last-write-wins would leave the tool_call fixture holding
  # whatever it retried with rather than the call the probe prompt asked for.
  # The create must be atomic, not check-then-act: pi fires sibling tool calls
  # concurrently, so two firings for the same engine+event can both pass an
  # `-e` test before either writes, and the later `>` would silently invert
  # that guarantee. `set -C` (noclobber) maps to open(..., O_EXCL), so only
  # the first writer's file survives; it's scoped to a subshell so noclobber
  # doesn't leak into the rest of the script.
  if ! (set -C; printf '%s\n' "$payload" >"$out/$engine-$name.json") 2>/dev/null; then
    # Losing the O_EXCL race is expected and not an error. A write that fails
    # for any other reason (permissions, disk full, ...) leaves no file behind
    # -- that case must still be reported, not swallowed with the race.
    [[ -e $out/$engine-$name.json ]] ||
      { echo "capture-hook: failed to write $out/$engine-$name.json" >&2; exit 1; }
  fi
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
