#!/usr/bin/env bash
# Codex PreToolUse deny+advice probe hook — issue #87.
#
#   emit-deny-advice.sh <logfile>
#
# Answers PreToolUse with a deny that also carries additionalContext: the shape
# hookyard must render for a denied call on a decision slot that also has an
# advisory slot. Run via `CODEX_ADVISORY_DENY_PROBE=1 run.sh`; the probe checks
# whether Codex acts on the deny and still delivers the advice to the model.
set -uo pipefail

log=${1:?log argument}
payload=$(cat)

printf '%s\n' "$payload" >>"$log"

jq -cn '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    permissionDecision: "deny",
    permissionDecisionReason: "PROBE-DENY-REASON",
    additionalContext: "PRE-ADVICE-MARKER"
  }
}'
exit 0
