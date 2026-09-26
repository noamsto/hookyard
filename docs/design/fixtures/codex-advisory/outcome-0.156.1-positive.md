# Result: Codex delivers `additionalContext` on all five advisory events — POSITIVE

**codex-cli 0.156.1 · 2026-09-26 · thinkpad-p14s-g5 · authenticated
(`codex login status`: "Logged in using ChatGPT").** Every event Codex documents
an advisory channel on delivered its hook's `additionalContext` into the
model-visible prompt input, each labelled by Codex itself as
`hooks.additional_context`: `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `SubagentStart`.

This closes the three events `outcome-0.154.0-positive.md` left unprobed. The
two handoff questions it named — whether a completed turn reaches the tool
events, and whether a spawned subagent reaches `SubagentStart` — are both
answered yes.

## What was run

`run.sh` registers all five events in a throwaway `CODEX_HOME`, one marker each,
then runs one headless turn. The default prompt asks the model to **spawn a
subagent that runs one shell command**, so a single run reaches every event.
`CODEX_ADVISORY_AUTH=1` symlinks the logged-in `~/.codex/auth.json` into the
scratch home; its contents are never read, copied, or printed.

```
CODEX_ADVISORY_AUTH=1 docs/design/fixtures/codex-advisory/run.sh
```

`hooks-all5.json` is that config verbatim; `hook-log-all5.jsonl` is the five
inbound payloads; `hook-stdout-all5.txt` is each hook's reply, reconstructed by
replaying the recorded payloads through `emit-advisory.sh`. The run also fires a
second, companion probe for the deny+advice shape (below).

## Evidence

All five hooks fired — no rejection of the output shape, and no
`PreToolUse`/`PostToolUse`/`SubagentStart` failure line:

```
hook: SessionStart
hook: SessionStart Completed
hook: UserPromptSubmit
hook: UserPromptSubmit Completed
```

Each marker reached the model-visible input, as a `developer`-role
`response_item` carrying Codex's own `hooks.additional_context` label —
`marker-records-all5.json`:

```json
{"type":"response_item","role":"developer","text":"SS-MARKER","kinds":["hooks.additional_context"]}
{"type":"response_item","role":"developer","text":"UPS-MARKER","kinds":["hooks.additional_context"]}
{"type":"response_item","role":"developer","text":"SUB-MARKER","kinds":["hooks.additional_context"]}
{"type":"response_item","role":"developer","text":"PRE-MARKER","kinds":["hooks.additional_context"]}
{"type":"response_item","role":"developer","text":"POST-MARKER","kinds":["hooks.additional_context"]}
```

`SS-MARKER` and `UPS-MARKER` appear twice across the two rollouts because the
spawned child thread carries the parent's `SessionStart`/`UserPromptSubmit`
context as well as its own `SubagentStart`/`PreToolUse`/`PostToolUse` ones; the
labelled-line check above is the signal, not a raw occurrence count. The model
reply confirms the round trip — it never saw the marker text in the prompt:

> The subagent ran `echo probe-tool` and reported: `probe-tool`

## Per-event disposition

| event | fires | `additionalContext` reaches model input | label | disposition |
| --- | --- | --- | --- | --- |
| `SessionStart` | ✅ | ✅ | `hooks.additional_context` | delivered |
| `UserPromptSubmit` | ✅ | ✅ | `hooks.additional_context` | delivered |
| `PreToolUse` | ✅ | ✅ | `hooks.additional_context` | delivered |
| `PostToolUse` | ✅ | ✅ | `hooks.additional_context` | delivered |
| `SubagentStart` | ✅ | ✅ | `hooks.additional_context` | delivered |

No event was rejected or unreached. `SubagentStart` is only reached by a prompt
that actually spawns a subagent (`multi_agent`), which is why the 0.154.0
unauthenticated run could not see it.

## The deny+advice shape (`PreToolUse`)

`PreToolUse` is also a decision slot, so hookyard renders a deny there. The
question the wiring needs answered is whether a deny may carry
`additionalContext` in the same payload. `CODEX_ADVISORY_DENY_PROBE=1 run.sh`
answers it: the hook denies the `Bash` call and carries an advisory.

```
hook: PreToolUse
hook: PreToolUse Blocked
ERROR codex_core::tools::router: error=Command blocked by PreToolUse hook: PROBE-DENY-REASON. Command: echo probe-tool
```

Codex acted on the deny, and the advice marker still reached the model input
(`PRE-ADVICE-MARKER` in `marker-records`, labelled `hooks.additional_context`).
The model reply shows the deny reason landed but not the advice verbatim:

> The command was blocked by the PreToolUse hook (`PROBE-DENY-REASON`), so I
> couldn't run it or confirm completion.

So the accepted `PreToolUse` shape is the full
`permissionDecision` + `permissionDecisionReason` + `additionalContext` object.
`renderCodexDeny` carries advice in that third field; an abstain/allow with
advice emits `additionalContext` alone with no decision, which is the same
advisory-only shape `SessionStart`/`UserPromptSubmit` use and which the
advisory probe above already proved accepted.

Artifacts: `deny-hook-log.jsonl`, `deny-stdout.txt`, `deny-lifecycle.txt`,
`deny-error.txt`, `deny-exec.out`.

## What this settles

- Codex's advisory set is all five documented events, not two. `HasAdvisorySlot`
  and the Codex render arm are wired for all five as of this PR.
- A `PreToolUse` deny may carry an advisory in the same response, so a denied
  call's advice is delivered rather than dropped.
- `SubagentStart` has no canonical counterpart in hookyard's six-event
  vocabulary, so it is reached as the engine-scoped `codex:SubagentStart` and
  resolved from the payload's native `hook_event_name`.

## What it does not settle

- **What the model does with it.** Delivery is not comprehension. Nothing here
  says an advisory *changes behaviour* on Codex, only that it arrives.
- **SubagentStart context semantics.** Codex's own `codex-rs` test
  `subagent_start_replaces_session_start_and_injects_context` shows a
  `SubagentStart` injection replaces the child thread's `SessionStart` context
  rather than adding to it. The child rollout carrying both labels here is
  consistent with the parent's context being inherited before that replacement;
  it is not evidence about what a child sees once its own hooks run.
- **`codex:SessionEnd`.** Out of scope here (#81 tracks the session-end signal),
  and not part of the advisory set.
