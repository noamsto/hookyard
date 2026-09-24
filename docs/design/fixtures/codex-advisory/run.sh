#!/usr/bin/env bash
# Codex advisory probe — issue #87.
#
#   docs/design/fixtures/codex-advisory/run.sh ["prompt"]
#
# Headless, and needs no credentials: both hooks run before the turn reaches the
# API, so a 401'd turn still produces the evidence. The result of the first run
# is in outcome-0.154.0-positive.md; this script is how to reproduce it against a
# future Codex.
#
# Sets up a throwaway CODEX_HOME, runs one exec, then reports which events fired
# and which markers reached the model-visible input. It does not judge the
# result beyond printing the counts.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
codex_bin=${CODEX_BIN:-codex}
prompt=${1:-say hi}

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
mkdir -p "$scratch"

# One marker per event, so a marker in the rollout can be attributed to the
# event that produced it. Absolute paths: Codex runs a hook command with the
# session cwd, not this one. UserPromptSubmit carries no matcher on purpose —
# Codex documents that event as not supporting them.
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
    ]
  }
}
JSON

echo "codex: $("$codex_bin" --version 2>&1 | head -1)"
echo "scratch: $scratch"
echo "--- running (exit 1 without credentials is expected and harmless)"
CODEX_HOME=$scratch timeout 120 "$codex_bin" --dangerously-bypass-hook-trust exec "$prompt" \
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
  grep -o 'SS-MARKER\|UPS-MARKER' "$rollout" | sort | uniq -c || echo "  none reached"
  echo "--- how Codex labelled them"
  jq -c 'select(tostring | contains("MARKER")) | .payload.internal_chat_message_metadata_passthrough.content_item_kinds' \
    "$rollout" | sort -u
else
  echo "  no session rollout written"
fi

echo
echo "Expected on a working channel: each marker present once, each labelled"
echo "[\"hooks.additional_context\"]. Record the outcome beside this fixture as"
echo "outcome-<version>-<positive|negative>.md."