# Pi's `pre_tool` standalone advisory channel

LOCAL probes answering issue #72's question — can a pi `tool_call` handler
return content alongside an allow, the way it can return `reason` alongside a
block? — plus a Claude Code parity fixture for the same abstain-with-advice
shape. Captured 2026-09-21 against Pi 0.86.1
(`@earendil-works/pi-coding-agent`, installed via this host's nix-config
`home/ai/pi`) and Claude Code 2.1.278.

## How they were made

Three throwaway pi extensions and one Claude Code hook, each a single
`tool_call` (or `PreToolUse`) handler:

| File | Behaviour |
|---|---|
| `ext-allow-content.ts` | Allows the call, returning every content-bearing key a handler might plausibly reach for (`reason`, `content`, `message`, `additionalContext`, `systemMessage`), each with its own marker |
| `ext-block.ts` | Control: blocks with `reason: "PROBE_BLOCK_REASON"` — the channel pre_tool advice rides today |
| `ext-steer.ts` | Allows the call, and separately calls `pi.sendMessage({customType:"probe", content:"PROBE_STEER", display:false}, {deliverAs:"steer"})` from inside the `tool_call` handler |
| `claude-pretool-hook.sh` | A Claude Code `PreToolUse` hook that allows by abstaining and emits only `additionalContext` — the shape `git-commit-autostage-guard.sh` uses |

Method: a scripted OpenAI-compatible server (`fake-llm.py`) or Anthropic
Messages server (`fake-anthropic.py`) answers the first tool-bearing request
with one bash tool call (`echo PROBE_EXECUTED`) and every later request with
plain text, logging the exact conversation the client sent on each request —
what reached the model is read off the wire, not inferred from a model
reply. Deterministic: no real model, no network egress, no credentials.

Pi runs (`probe.sh`) the real non-wrapper binary `<libexec>/pi/pi` with an
isolated scratch `PI_CODING_AGENT_DIR` (seeded with a `models.json` pointing
at the local fake server and an empty `settings.json`), `PI_OFFLINE=1`,
`--no-session --print --mode json --no-extensions --no-context-files
--no-skills --no-prompt-templates --no-themes --tools bash -e <ext>`:

```
./probe.sh <pi-binary> ext-allow-content.ts sample-allow-content.jsonl
./probe.sh <pi-binary> ext-block.ts sample-block.jsonl
./probe.sh <pi-binary> ext-steer.ts sample-steer.jsonl
```

Claude Code runs (`probe-claude.sh`) the real `claude` binary with an
isolated `CLAUDE_CONFIG_DIR`, `--settings` supplying only the one
`PreToolUse` hook under test, and `--setting-sources ""` to keep the user's
own hooks, plugins, and settings out of the run:

```
./probe-claude.sh <claude-binary> sample-claude-pretool.jsonl
```

`--tools bash` / `--allowedTools Bash` keeps each run to the one tool under
test. `--mode json` (pi) streams one JSON event per line; both fake servers
log the full message list of every request that carries the tool to their
respective `sample-*.jsonl`.

## Results

| Probe | What the handler did | What reached the model | Reading |
|---|---|---|---|
| `ext-allow-content.ts` (A) | Allowed, returning `reason`/`content`/`message`/`additionalContext`/`systemMessage`, each marked | `PROBE_EXECUTED\n` as the tool result; none of the five markers appear anywhere in `sample-allow-content.jsonl` | An allow-side return is fully discarded by pi's agent loop — confirms the platform limit issue #72 asks about, not a probe-shape artifact |
| `ext-block.ts` (B, control) | Blocked with `reason: "PROBE_BLOCK_REASON"` | `"PROBE_BLOCK_REASON"` as the tool result content; no `PROBE_EXECUTED` anywhere — the command never ran | Confirms the harness surfaces a marker when pi does forward one: probe A's silence is pi discarding the content, not the fixture failing to log it |
| `ext-steer.ts` (C) | Allowed, and separately queued a `deliverAs: "steer"` custom message | Request 2 in `sample-steer.jsonl` is `[user prompt, assistant tool_call, tool "PROBE_EXECUTED\n", user "PROBE_STEER"]` — the marker reaches the model as a **user-role** message, placed **after** the tool result, in the same request | Steer is pi's one live side channel into model context from inside `tool_call`; it lands one request later than the call, not attached to it |
| `claude-pretool-hook.sh` (parity) | Allowed by abstaining, emitted only `hookSpecificOutput.additionalContext` | `sample-claude-pretool.jsonl` has two tool-bearing requests; the marker first appears in request 2 as a **system-role** message, `"PreToolUse:Bash hook additional context: PROBE_CLAUDE_ADDITIONALCONTEXT"`, placed **after** the tool_result block `"PROBE_EXECUTED"` | Claude Code's own `pre_tool` advisory also first reaches the model after the tool ran — not before, not in the call's own request |

## Claude parity

The point of the parity fixture is not that pi and Claude render advisory
content identically — they don't (role, attachment point, and delivery
mechanism all differ; see `docs/design/hookyard.md` §11.1's itemised list) —
it's that **both first surface pre-tool advisory to the model only at the
request after the one that issued the tool call**. Neither engine makes an
LLM call between a pre-tool hook/handler returning and the tool executing, so
context queued during that handler necessarily waits for the model's next
turn regardless of engine. `sample-steer.jsonl` and
`sample-claude-pretool.jsonl` show the same structural shape — advisory
marker after the tool result, in the request the tool result also appears
in — which is why pi's steer-based delivery is timing parity with Claude's
`additionalContext`, not a degraded fallback.

## Redaction

Pi's `sample-*.jsonl` drop the system-role message from every logged
request (`fake-llm.py`'s `REQUEST_LOG` write): it is identical across
requests and names this machine's real working directory. The Claude fixture
takes the opposite approach because Claude Code's own preamble is
interleaved into user- and system-role text blocks rather than isolated in
one message: `fake-anthropic.py`'s `elide()` keeps only PROBE-marked lines
(and the literal prompt `"Run the probe."`) out of every text block and
replaces the rest with `"[elided]"` — Claude Code's environment preamble
otherwise lists this machine's paths, installed skills, and plugins. Tool
call ids, request ids, and model names in all four samples are the fake
servers' own fixed placeholders (`PROBE-TOOLCALL-01`, `chatcmpl-PROBE`,
`msg_PROBE`, `toolu_PROBE01`), not captured values, so no further id
redaction was needed.

## Cleanup

Every pi run pointed `PI_CODING_AGENT_DIR` at a scratch directory created by
`probe.sh` under `mktemp -d`, never at `~/.pi/agent`. Every Claude Code run
pointed `CLAUDE_CONFIG_DIR` at a scratch subdirectory of the same kind,
supplied `--settings` pointing only at that run's own one-hook file, and used
`--setting-sources ""` so no real plugin, hook, or setting from this machine
loaded. Nothing was installed or registered in either real config; both
scratch trees are removed by each script's own `trap ... EXIT` cleanup.
