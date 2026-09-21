#!/usr/bin/env bash
# A PreToolUse hook that allows by abstaining and emits only additionalContext —
# the shape git-commit-autostage-guard.sh uses.
cat >/dev/null
printf '%s\n' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"PROBE_CLAUDE_ADDITIONALCONTEXT"}}'
