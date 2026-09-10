#!/usr/bin/env bash
# Capture hook for the hookyard payload probe.
#   $1 engine label, $2 event label, $3 mode (observe|deny|sleep), $4 out file
# Appends one JSONL record per firing to $4, then answers per mode. The out file
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

jq -c -n \
  --arg engine "$engine" \
  --arg event "$event" \
  --arg mode "$mode" \
  --arg cwd "$PWD" \
  --arg raw "$payload" \
  --argjson env "$(jq -n '$ENV | with_entries(select(.key | test("^(CLAUDE|CODEX|CURSOR|AGENT|HOOK)")))')" \
  '{engine: $engine, event: $event, mode: $mode, cwd: $cwd, env: $env, raw: $raw}' \
  >>"$out"

case $mode in
  deny)
    case $engine in
      cursor) printf '%s\n' '{"permission":"deny","user_message":"hookyard probe deny"}' ;;
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
