#!/usr/bin/env bash
# Codex advisory probe hook — issue #87.
#
# Answers a Codex hook event with hookSpecificOutput.additionalContext, and
# records the inbound payload, so one firing yields both halves: what Codex
# sent and what it was told back. The marker travels to the model, not to this
# script, which is what makes the check falsifiable — a marker that was never
# in the prompt cannot appear in the reply unless the hook put it there.
#
# Arguments, not environment: an engine may not pass its own env to a hook
# subprocess (the lesson capture-hook.sh already records), and a probe whose
# marker silently never arrived would be indistinguishable from a channel that
# does not deliver.
#
#   $1 marker text to inject
#   $2 file to append each inbound payload to, one JSON line per firing
set -uo pipefail

marker=${1:?marker argument}
log=${2:?log argument}
payload=$(cat)

printf '%s\n' "$payload" >>"$log"

# The event name comes back off the payload rather than from an argument: a
# mismatched hookEventName is exactly the kind of shape error that would make
# Codex reject the answer, and the probe would then be measuring the wrong
# thing.
event=$(jq -r '.hook_event_name // empty' <<<"$payload" 2>/dev/null)
if [[ -z $event ]]; then
  echo "emit-advisory: no hook_event_name in payload" >&2
  exit 1
fi

# jq builds the JSON so a marker containing a quote cannot break the shape.
jq -cn --arg event "$event" --arg ctx "$marker" \
  '{hookSpecificOutput: {hookEventName: $event, additionalContext: $ctx}}'
exit 0