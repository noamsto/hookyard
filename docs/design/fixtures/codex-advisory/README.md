# Codex's advisory channel

Does Codex deliver `additionalContext` back to the model? Issue #87.

**ANSWERED: YES.** `outcome-0.154.0-positive.md` — on codex-cli 0.154.0 both
`SessionStart` and `UserPromptSubmit` delivered their hook's `additionalContext`
into the model-visible prompt input, each labelled by Codex itself as
`hooks.additional_context`.

The fixture stays, because it is now the *reproduction* rather than the
question: `run.sh` re-runs it headlessly against a future Codex, and the
capability table should be re-derived from that run rather than from Codex's
documentation.

## What was claimed

> Codex has no advisory channel on any event, full stop: this is a settled
> boundary, not a gap to fill later — no handler can get advice to Codex by any
> means this package offers. — `internal/verdict/capability.go`

> Codex has no advisory channel at all — a settled boundary, not an open
> question — Only fire-and-forget hooks observed deployed. —
> `docs/design/hookyard.md`

The stated evidence was an absence of observation. Codex's docs specified
`hookSpecificOutput.additionalContext` on `SessionStart`, `UserPromptSubmit`,
`PreToolUse`, `PostToolUse` and `SubagentStart`, and `codex-rs` carried
`UserPromptSubmitHookSpecificOutputWire { additional_context: Option<String> }`.

That table cell was stale in a second, unrelated way, worth recording because
nobody noticed for months: it said Codex's deny path and its undeclared default
timeout "were not verified this pass", but both had already been captured as
fixtures — `codex-pre_tool_use-DENY.json` (Codex reported `Blocked by hook`) and
`codex-user_prompt_submit-TICK.json` (ran 180 s unkilled).

## Files

| File | What it is |
| --- | --- |
| `outcome-0.154.0-positive.md` | the run, the evidence, what it settles and what it does not |
| `run.sh` | reproduces the probe headlessly; no credentials needed |
| `emit-advisory.sh` | the hook: answers an event with `additionalContext`, records the inbound payload |
| `hooks-both.json` | the exact config used, both events, one marker each |
| `hook-log-both.jsonl` | the two inbound payloads as received |
| `hook-lifecycle-both.txt` | Codex's own `hook:` lines — a rejection would show here |
| `marker-records-both.json` | the two `response_item` records that carry the markers into model input |

## Method

`CODEX_HOME=<scratch> codex --dangerously-bypass-hook-trust exec "<prompt>"`,
then read the markers out of the session rollout under
`$CODEX_HOME/sessions/`. No login: both hooks run *before* the turn reaches the
API, so a 401'd turn still produces the evidence. `emit-advisory.sh` takes its
marker as an **argument, not an environment variable**, because an engine may
not pass its own env to a hook subprocess — the lesson `capture-hook.sh` already
records — and derives `hookEventName` from the **inbound payload**, because a
mismatched event name is itself a shape error that would make Codex reject the
answer and the probe would then measure the wrong thing.

## Dead ends worth not repeating

- **`codex debug prompt-input` does not run `SessionStart` hooks.** It renders
  the prompt input list (an array of `developer`/`user` messages) and looks like
  the ideal no-auth wire-level probe. The hook never fires, so it silently
  measures nothing — the first attempt here produced a clean 29 KB render and
  zero evidence.
- **A `CODEX_HOME` under `/tmp` is degraded.** Codex warns *"Refusing to create
  helper binaries under temporary dir"* and continues with PATH aliases missing.
  `run.sh` uses `$XDG_CACHE_HOME` for that reason.
- **`--dangerously-bypass-hook-trust` is required**, and is Codex's documented
  path for automation whose hook sources are already vetted. Without it Codex
  skips an unreviewed hook, which is indistinguishable from a channel that does
  not deliver.
- **`codex features list` reports `hooks stable true`** — hooks are on by
  default, so a silent no-op is never the feature flag.

## Probing the remaining three events

`PreToolUse`, `PostToolUse` and `SubagentStart` are **unproven as of 0.154.0**.
All three are registered in `run.sh`, but none can fire unauthenticated: a 401'd
turn never executes a tool, so `PreToolUse`/`PostToolUse` need a completed turn
and `SubagentStart` needs a subagent. `run.sh` is written so that one command
covers all five — run it unauthenticated and you get the two that do not need a
turn, run it logged in and you get the rest. The prompt asks for one trivial
shell command, which is what makes the tool events reachable, and `-C` keeps the
turn inside a throwaway directory.

The docs specify the same channel on all three. That is documentation, not a
probe — which is the entire lesson of this directory.

Nothing here says an advisory *changes behaviour* on Codex, only that it arrives.
