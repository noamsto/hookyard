#!/usr/bin/env bash
# Runs the Claude Code parity probe against fake-anthropic.py.
# Usage: probe-claude.sh <claude-binary> <out.jsonl>
set -euo pipefail

claude_bin=$1
out=$(realpath -m "$2")
here=$(dirname "$(realpath "$0")")
port=${PROBE_PORT:-18766}

scratch=$(mktemp -d)
server_pid=
cleanup() {
  if [[ -n $server_pid ]]; then kill "$server_pid" 2>/dev/null || true; fi
  rm -rf "$scratch"
}
trap cleanup EXIT INT TERM

cat >"$scratch/settings.json" <<JSON
{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"$here/claude-pretool-hook.sh"}]}]}}
JSON

: >"$out"
python3 "$here/fake-anthropic.py" "$port" "$out" &
server_pid=$!
for _ in $(seq 50); do
  if curl -s -o /dev/null "http://127.0.0.1:$port/"; then break; fi
  sleep 0.1
done

# An empty config dir keeps the user's own hooks, plugins and settings out of
# the run; --settings supplies the one hook under test.
env CLAUDE_CONFIG_DIR="$scratch/config" ANTHROPIC_BASE_URL="http://127.0.0.1:$port" \
  ANTHROPIC_API_KEY=probe DISABLE_TELEMETRY=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  "$claude_bin" -p --settings "$scratch/settings.json" --setting-sources "" \
  --allowedTools Bash --model claude-sonnet-5 \
  "Run the probe." >"$scratch/claude.out"
