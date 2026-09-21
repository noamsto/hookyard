#!/usr/bin/env bash
# Runs one pi probe against fake-llm.py and prints the request log.
# Usage: probe.sh <pi-binary> <extension.ts> <out.jsonl>
set -euo pipefail

pi_bin=$1
ext=$(realpath "$2")
out=$(realpath -m "$3")
here=$(dirname "$(realpath "$0")")
port=${PROBE_PORT:-18765}

scratch=$(mktemp -d)
server_pid=
cleanup() {
  if [[ -n $server_pid ]]; then kill "$server_pid" 2>/dev/null || true; fi
  rm -rf "$scratch"
}
trap cleanup EXIT INT TERM

cat >"$scratch/models.json" <<JSON
{"providers":{"probe":{"baseUrl":"http://127.0.0.1:$port/v1","api":"openai-completions","apiKey":"probe",
 "compat":{"supportsDeveloperRole":false,"supportsReasoningEffort":false,"supportsUsageInStreaming":false},
 "models":[{"id":"probe"}]}}}
JSON
echo '{}' >"$scratch/settings.json"

: >"$out"
python3 "$here/fake-llm.py" "$port" "$out" &
server_pid=$!
for _ in $(seq 50); do
  if curl -sf "http://127.0.0.1:$port/v1/models" >/dev/null; then break; fi
  sleep 0.1
done

env PI_CODING_AGENT_DIR="$scratch" PI_OFFLINE=1 \
  "$pi_bin" --no-session --print --mode json \
  --provider probe --model probe \
  --no-extensions --no-context-files --no-skills --no-prompt-templates --no-themes \
  --tools bash -e "$ext" \
  "Run the probe." >"$scratch/pi.jsonl"
