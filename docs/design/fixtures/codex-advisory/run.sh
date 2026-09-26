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
# All five need a completed turn for PreToolUse/PostToolUse and a spawned
# subagent for SubagentStart, so the default prompt asks the model to spawn a
# subagent that runs one shell command. Set CODEX_ADVISORY_AUTH=1 to symlink
# the logged-in ~/.codex/auth.json into the scratch CODEX_HOME: the file's
# contents are never read, copied, or printed, only symlinked. Without it the
# turn aborts at the API and only SessionStart/UserPromptSubmit can fire.
#
# CODEX_ADVISORY_DENY_PROBE=1 runs the separate PreToolUse deny+advice shape
# probe: the hook denies a Bash call and carries an advisory, and the script
# reports whether Codex acted on the deny and delivered the advice.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
codex_bin=${CODEX_BIN:-codex}
prompt=${1:-"Spawn a subagent to run the shell command 'echo probe-tool', then report the subagent's output."}
deny_probe=${CODEX_ADVISORY_DENY_PROBE:-0}

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

if [[ ${CODEX_ADVISORY_AUTH:-0} == 1 ]]; then
  auth=$HOME/.codex/auth.json
  if [[ ! -f $auth ]]; then
    echo "run.sh: CODEX_ADVISORY_AUTH=1 but $auth does not exist; run 'codex login' first" >&2
    exit 1
  fi
  ln -s "$auth" "$scratch/auth.json"
  echo "auth: symlinked from $auth (contents never read)"
fi

if [[ $deny_probe == 1 ]]; then
  prompt=${1:-"Run the shell command 'echo probe-tool' and then say DONE."}
  cat >"$scratch/hooks.json" <<JSON
{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$here/emit-deny-advice.sh $scratch/hook-log.jsonl","timeout":10}]}]}}
JSON
else
  # One marker per event, so a marker in the rollout is attributable to the
  # event that produced it. Absolute paths: Codex runs a hook command with the
  # session cwd, not this one. UserPromptSubmit carries no matcher on purpose —
  # Codex documents that event as not supporting them. SubagentStart needs no
  # matcher either.
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
fi

echo "codex: $("$codex_bin" --version 2>&1 | head -1)"
echo "scratch: $scratch"
echo "mode: $([[ $deny_probe == 1 ]] && echo 'PreToolUse deny+advice' || echo 'advisory on all five events')"
echo "--- running (without CODEX_ADVISORY_AUTH a 401 is expected; only the first"
echo "--- two events can fire before the API call, so a 401 is partial evidence)"

# -C keeps the turn inside a throwaway directory, and the approvals/sandbox
# bypass is the same one the fleet's worker launch uses (adapters/core/dispatch.sh)
# — without it the tool call never runs and PreToolUse/PostToolUse never fire.
CODEX_HOME=$scratch timeout 300 "$codex_bin" \
  --dangerously-bypass-hook-trust --dangerously-bypass-approvals-and-sandbox \
  --enable multi_agent \
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
mapfile -t rollouts < <(find "$scratch/sessions" -name '*.jsonl' 2>/dev/null)
if ((${#rollouts[@]})); then
  grep -ho 'SS-MARKER\|UPS-MARKER\|PRE-MARKER\|POST-MARKER\|SUB-MARKER\|PRE-ADVICE-MARKER' "${rollouts[@]}" | sort | uniq -c ||
    echo "  none reached"
  echo "--- how Codex labelled them"
  jq -c 'select(tostring | contains("MARKER")) | .payload.internal_chat_message_metadata_passthrough.content_item_kinds' \
    "${rollouts[@]}" | sort -u
  if [[ $deny_probe == 1 ]]; then
    echo "--- deny+advice outcome"
    if grep -q 'PreToolUse Blocked' "$scratch/exec.err"; then
      echo "deny: acted on"
    else
      echo "deny: NOT acted on"
    fi
    printf 'advice marker hits: '
    # grep exits 1 on no match, which under pipefail/errexit would abort the
    # negative case this probe exists to measure; the count still prints 0.
    hits=$(grep -ho 'PRE-ADVICE-MARKER' "${rollouts[@]}" 2>/dev/null | wc -l) || true
    printf '%s\n' "$hits"
  fi
else
  echo "  no session rollout written"
fi

echo
echo "Expected on a working channel: one line per event that fired, each marker"
echo "present at least once, each labelled [\"hooks.additional_context\"]."
echo "Record the outcome beside this fixture as outcome-<version>-<polarity>.md."
