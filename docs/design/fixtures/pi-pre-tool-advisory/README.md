# Pi's `pre_tool` standalone advisory channel

LOCAL probes for issue #72's question — can a pi `tool_call` handler return
content alongside an allow, the way it can return `reason` alongside a
block? — plus the probes that ruled out `deliverAs: "steer"` as the delivery
channel and validated the mechanism that ships instead: the bridge appends
the advisory as a text block onto the tool's own `tool_result`, keyed by
`toolCallId`. Captured 2026-09-21 against Pi 0.86.1
(`@earendil-works/pi-coding-agent`, installed via this host's nix-config
`home/ai/pi`) and Claude Code 2.1.278.

## How they were made

Five throwaway pi extensions and one Claude Code hook, each built around a
`tool_call` (or `PreToolUse`) handler:

| File | Behaviour |
|---|---|
| `ext-allow-content.ts` | Allows the call, returning every content-bearing key a handler might plausibly reach for (`reason`, `content`, `message`, `additionalContext`, `systemMessage`), each with its own marker |
| `ext-block.ts` | Control: blocks with `reason: "PROBE_BLOCK_REASON"` — the channel pre_tool advice rides today on a deny |
| `ext-steer.ts` | Allows the call, and separately calls `pi.sendMessage({customType:"probe", content:"PROBE_STEER", display:false}, {deliverAs:"steer"})` from inside the `tool_call` handler |
| `ext-steer-parallel.ts` | Same as `ext-steer.ts`, but each steer's content is tagged with its own call's `toolCallId`, so a parallel batch's requests show which call each steer belongs to |
| `ext-tool-result-append.ts` | The shipped mechanism, reproduced directly against pi: stashes `"PROBE_APPEND " + toolCallId` at `tool_call` (allow), appends it as a text block onto that same call's `tool_result`, and clears the stash at `turn_end` |
| `claude-pretool-hook.sh` | A Claude Code `PreToolUse` hook that allows by abstaining and emits only `additionalContext` — the shape `git-commit-autostage-guard.sh` uses |

Method: a scripted OpenAI-compatible server (`fake-llm.py`) or Anthropic
Messages server (`fake-anthropic.py`) answers the first tool-bearing request
with either one bash tool call (`echo PROBE_EXECUTED`) or, with
`PROBE_PARALLEL=1`, two parallel bash tool calls (ids `PROBE-TOOLCALL-01` /
`-02`, commands `echo PROBE_EXECUTED_1` / `echo PROBE_EXECUTED_2`), and every
later request with plain text; the scripting for later requests stays text
regardless of `PROBE_PARALLEL`. Every request logs the exact conversation the
client sent — what reached the model is read off the wire, not inferred from
a model reply. Deterministic: no real model, no network egress, no
credentials.

Pi runs (`probe.sh`) the real non-wrapper binary `<libexec>/pi/pi` with an
isolated scratch `PI_CODING_AGENT_DIR` (seeded with a `models.json` pointing
at the local fake server and an empty `settings.json`), `PI_OFFLINE=1`,
`--no-session --print --mode json --no-extensions --no-context-files
--no-skills --no-prompt-templates --no-themes --tools bash -e <ext>`:

```
./probe.sh <pi-binary> ext-allow-content.ts sample-allow-content.jsonl
./probe.sh <pi-binary> ext-block.ts sample-block.jsonl
./probe.sh <pi-binary> ext-steer.ts sample-steer.jsonl
PROBE_PARALLEL=1 ./probe.sh <pi-binary> ext-steer-parallel.ts sample-steer-parallel.jsonl
PROBE_PARALLEL=1 ./probe.sh <pi-binary> ext-tool-result-append.ts sample-tool-result-append-parallel.jsonl
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
| `ext-steer.ts` (C) | Allowed, and separately queued a `deliverAs: "steer"` custom message | Request 2 in `sample-steer.jsonl` is `[user prompt, assistant tool_call, tool "PROBE_EXECUTED\n", user "PROBE_STEER"]` — the marker reaches the model as a **user-role** message, placed **after** the tool result, in the same request | Steer is pi's one live side channel into model context from inside `tool_call` for a single call; see "Rejected: steer" below for why it is not the shipped mechanism |
| `ext-steer-parallel.ts` (D) | Two parallel tool calls, each queuing its own tagged steer | `sample-steer-parallel.jsonl` is **three** requests, not two: request 2 carries both tool results but only `"PROBE_STEER PROBE-TOOLCALL-01"`; request 3 — issued *after* the model already answered `"done"` in request 2 — carries `"PROBE_STEER PROBE-TOOLCALL-02"` | The trickle, live: with N parallel calls, only the first queued steer drains per turn (`steeringMode: "one-at-a-time"`); the rest force one extra model call apiece, past the model's own final answer |
| `ext-tool-result-append.ts` (E) | Stashed `"PROBE_APPEND " + toolCallId` at `tool_call`, appended it onto that same call's `tool_result` | `sample-tool-result-append-parallel.jsonl` is **one** request: both tool results carry their own advisory, `"PROBE_EXECUTED_1\n\nPROBE_APPEND PROBE-TOOLCALL-01"` and `"PROBE_EXECUTED_2\n\nPROBE_APPEND PROBE-TOOLCALL-02"` | Mechanism B at the pi level: exactly one advisory per call, attached to the call, no extra request |
| `claude-pretool-hook.sh` (parity) | Allowed by abstaining, emitted only `hookSpecificOutput.additionalContext` | `sample-claude-pretool.jsonl` has two tool-bearing requests; the marker first appears in request 2 as a **system-role** message, `"PreToolUse:Bash hook additional context: PROBE_CLAUDE_ADDITIONALCONTEXT"`, placed **after** the tool_result block `"PROBE_EXECUTED"` | Claude Code's own `pre_tool` advisory also first reaches the model after the tool ran — alongside its result, not before, not deferred further |

## Rejected: steer

`deliverAs: "steer"` was the first candidate (probe C shows it works for one
call) and was rejected once probe D showed what it does under the shape
`pre_tool` advisory actually needs to survive — N parallel tool calls each
carrying advice. `sample-steer-parallel.jsonl` request 2 has *two* tool
results but only *one* steer; the second steer doesn't show up until request
3, and request 3 only exists because the model had already produced its
`"done"` answer in request 2 — the queued steer forced a further model call
past the model's own final turn.

That is not a probe-shape artifact; it follows directly from pi's own agent
loop (Pi 0.86.1, `<libexec>/pi/pi`, code-reading):

- `docs/settings.md` documents `steeringMode` defaulting to `"one-at-a-time"`.
- The embedded `PendingMessageQueue.drain()` honours that: with any mode
  other than `"all"` it returns only `this.messages[0]`, leaving the rest
  queued — `drain()` is called once per iteration of `runLoop`'s inner
  `while`, so N queued steers take N further iterations to drain, one call
  per iteration.
- That inner loop is `while (hasMoreToolCalls || pendingMessages.length > 0)`
  — a nonempty steering queue keeps the loop alive on its own, independent of
  `hasMoreToolCalls`. Concretely: `hasMoreToolCalls = !executedToolBatch.terminate`
  is set *before* the loop condition is re-checked, so a queued steer forces
  one more iteration — one more model call — even when a tool in the batch
  already asked to `terminate: true`. A blocked call's own termination can be
  overridden by an unrelated call's queued advisory.
- `clearQueue()` (the abort/dequeue path, reached from `Session.clearQueue()`
  → `Agent.clearAllQueues()`) drops both queues outright. An abort between a
  `tool_call` handler queuing a steer and that steer draining loses the
  advisory silently — while hookyard's own delivery record, computed at
  render time from the router's reply, would still say delivered.
- And the user's own steers (typed mid-turn) share the same queue and mode,
  so they queue behind whatever hookyard advisories are still pending drain.

None of this is reachable with a single tool call carrying advice — probe C
alone looked clean. It is reachable, and does trigger, the moment a turn
carries more than one advised call, which parallel tool execution (pi's
default `toolExecution: "parallel"`) makes routine.

## Mechanism B: parity with Claude, not deferral

Mechanism B avoids all of the above by never touching the steering queue.
The bridge's `tool_call` handler, on a non-block reply carrying advice,
stashes it keyed by `toolCallId` and allows; a bridge-owned `tool_result`
handler — registered before hookyard's per-entry handlers, so first in pi's
`tool_result` middleware chain — appends `"[hookyard advisory] " + advice` as
a text block onto *that call's own* tool result; a bridge-owned `turn_end`
handler clears the stash so a call aborted before it reaches `tool_result`
can't leave stale advice for a future call that reuses the id space.
`ext-tool-result-append.ts` (probe E) is that mechanism reproduced directly
against pi, and `sample-tool-result-append-parallel.jsonl` shows the result:
one request, two tool results, each carrying its own advisory — no trickle,
no extra model call, no terminate override, no queue-clear race.

This is not a degraded fallback relative to Claude Code — it is closer to
parity than steer ever was. No engine here makes an LLM call between a
pre-tool hook/handler returning and the tool executing, so no engine can put
pre-tool advisory *before* the tool result; the best either engine can do is
attach it to the result. `sample-claude-pretool.jsonl` shows exactly that:
Claude Code's own `additionalContext` from a `PreToolUse` hook that abstains
first appears as a system-role message immediately after the `tool_result`
block, in the very next request the tool result itself appears in. Mechanism
B puts pi's advisory in the same place — the very next request, beside that
call's own result — except folded into the tool message's own content rather
than a separate system-role message. Steer's fatal property was never its
attachment point (`ext-steer.ts` alone also lands one request later, same as
Claude); it was that a *queue* sits between the handler and delivery, and
queues drain one-at-a-time, can be cleared, and can outlive the batch that
filled them. Mechanism B has no queue: the advisory is data hung directly off
the one `tool_result` reply for its own call.

Real differences that remain, stated rather than smoothed over: it is text
appended inside the tool result's own content (role `tool`/`user`
`tool_result`), not a separate system-role message the way Claude's is; and a
foreign, non-hookyard extension loaded before the bridge that rewrites
`tool_result` content wholesale (replacing rather than appending, since
`tool_result` handlers chain like middleware over each other's patches)
could still drop hookyard's appended block — the same class of caveat §11.1
already states for a foreign extension racing a `tool_call` block.

## Redaction

Pi's `sample-*.jsonl` drop the system-role message from every logged
request (`fake-llm.py`'s `REQUEST_LOG` write): it is identical across
requests and names this machine's real working directory. The Claude fixture
takes the opposite approach because Claude Code's own preamble is
interleaved into user- and system-role text blocks rather than isolated in
one message: `fake-anthropic.py`'s `elide()` fails closed rather than
allow-listing individual keys to redact — it keeps a small allow-list of
structural keys verbatim (`type`, `id`, `name`, `input`, `tool_use_id`,
`is_error`, `cache_control`, `role`), recurses into any list-valued `text` or
`content` (so a nested `tool_result` content list, not just a top-level
block, gets the same treatment), reduces every other `text`/`content` string
to its PROBE-marked lines (substring match on `"PROBE"` or `"Run the
probe."`, not equality — a marker embedded mid-line still survives) or
`"[elided]"`, and replaces every other key's value with `"[elided]"`
outright. A block type this fixture doesn't special-case — `thinking`,
`redacted_thinking`, or anything else Claude Code might send — is elided by
construction rather than passed through raw, which the prior key-specific
version did not guarantee. Claude Code's environment preamble otherwise
lists this machine's paths, installed skills, and plugins. Tool call ids,
request ids, and model names in all samples are the fake servers' own fixed
placeholders (`PROBE-TOOLCALL-01`/`-02`, `chatcmpl-PROBE`, `msg_PROBE`,
`toolu_PROBE01`), not captured values, so no further id redaction was
needed.

## Cleanup

Every pi run pointed `PI_CODING_AGENT_DIR` at a scratch directory created by
`probe.sh` under `mktemp -d`, never at `~/.pi/agent`. Every Claude Code run
pointed `CLAUDE_CONFIG_DIR` at a scratch subdirectory of the same kind,
supplied `--settings` pointing only at that run's own one-hook file, and used
`--setting-sources ""` so no real plugin, hook, or setting from this machine
loaded. Nothing was installed or registered in either real config; both
scratch trees are removed by each script's own `trap ... EXIT` cleanup.
