#!/usr/bin/env bash
# Codex advisory probe — issue #87.
#
#   docs/design/fixtures/codex-advisory/run.sh ["prompt"]
#
# Registers all five events Codex documents an advisory channel on, each with
# its own marker, runs one headless turn, then reports which events fired and
# which markers reached the model-visible input. It does not judge the result
# beyond printing counts and labels.
#
# Works logged in or not, and that is deliberate: SessionStart and
# UserPromptSubmit both run before the turn reaches the API, so an
# unauthenticated run already answers for those two (see
# outcome-0.154.0-positive.md). PreToolUse, PostToolUse and SubagentStart need a
# real turn — a 401'd turn never executes a tool — so they answer only once
# Codex is authenticated.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
codex_bin=${CODEX_BIN:-codex}
prompt=${1:-"Run the shell command 'echo probe-tool' and then reply with the PROBE markers you were given."}

if ! command -v "$codex_bin" >/dev/null 2>&1; then
  echo "run.sh: '$codex_bin' not on PATH. Set CODEX_BIN to the binary, e.g." >&2
  echo "  CODEX_BIN=\$(ls -d /nix/store/*-codex-*/bin/codex | tail -1) $0" >&2
  exit 1
fi

# Deliberately not under /tmp: Codex refuses to create its helper binaries in a
# temporary directory ("Refusing to create helper binaries under temporary dir")
# and continues degraded, which is noise unrelated to what is measured.
scratch=${XDG_CACHE_HOME:-$HOME/.cache}/codex-advisory-probe
rm -rf "$scratch"
mkdir -p "$scratch/work"

# One marker per event, so a marker in the rollout is attributable to the event
# that produced it. Absolute paths: Codex runs a hook command with the session
# cwd, not this one. UserPromptSubmit carries no matcher on purpose — Codex
# documents that event as not supporting them.
cat >"$scratch/hooks.json" <<JSON
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear",
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh SS-MARKER $scratch/hook-log.jsonl", "timeout": 10}]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh UPS-MARKER $scratch/hook-log.jsonl", "timeout": 10}]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh PRE-MARKER $scratch/hook-log.jsonl", "timeout": 10}]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh POST-MARKER $scratch/hook-log.jsonl", "timeout": 10}]
      }
    ],
    "SubagentStart": [
      {
        "hooks": [{"type": "command", "command": "$here/emit-advisory.sh SUB-MARKER $scratch/hook-log.jsonl", "timeout": 10}]
      }
    ]
  }
}
JSON

echo "codex: $("$codex_bin" --version 2>&1 | head -1)"
echo "scratch: $scratch"
echo "--- running (exit 1 without credentials is expected; only the first two"
echo "--- events can fire before the API call, so a 401 is partial evidence)"

# -C keeps the turn inside a throwaway directory, and the approvals/sandbox
# bypass is the same one the fleet's worker launch uses (adapters/core/dispatch.sh)
# — without it the tool call never runs and PreToolUse/PostToolUse never fire.
CODEX_HOME=$scratch timeout 180 "$codex_bin" \
  --dangerously-bypass-hook-trust --dangerously-bypass-approvals-and-sandbox \
  -C "$scratch/work" exec "$prompt" \
  >"$scratch/exec.out" 2>"$scratch/exec.err" || true

echo "--- events that fired"
if [[ -s $scratch/hook-log.jsonl ]]; then
  jq -r '.hook_event_name' "$scratch/hook-log.jsonl" | sort | uniq -c
else
  echo "NONE — no hook fired. That is not an answer about the advisory channel:"
  echo "check trust and CODEX_HOME before reading anything into it."
fi

echo "--- hook lifecycle (a rejection would appear here)"
grep -E "^hook: " "$scratch/exec.err" || echo "  (none)"

echo "--- markers in the model-visible input"
rollout=$(find "$scratch/sessions" -name '*.jsonl' 2>/dev/null | head -1)
if [[ -n $rollout ]]; then
  grep -o 'SS-MARKER\|UPS-MARKER\|PRE-MARKER\|POST-MARKER\|SUB-MARKER' "$rollout" | sort | uniq -c ||
    echo "  none reached"
  echo "--- how Codex labelled them"
  jq -c 'select(tostring | contains("MARKER")) | .payload.internal_chat_message_metadata_passthrough.content_item_kinds' \
    "$rollout" | sort -u
else
  echo "  no session rollout written"
fi

echo
echo "Expected on a working channel: one line per event that fired, each marker"
echo "present once, each labelled [\"hooks.additional_context\"]."
echo "Unproven as of codex-cli 0.154.0: PreToolUse, PostToolUse, SubagentStart —"
echo "they need a completed turn, so an unauthenticated run cannot reach them."
echo "Record the outcome beside this fixture as outcome-<version>-<polarity>.md."
