# Codex's advisory channel

Does Codex deliver `additionalContext` back to the model? Issue #87.

**ANSWERED: YES, on all five documented events.** `outcome-0.154.0-positive.md`
proved it for `SessionStart` and `UserPromptSubmit` at codex-cli 0.154.0;
`outcome-0.156.1-positive.md` extends it to `PreToolUse`, `PostToolUse` and
`SubagentStart` at 0.156.1. Every marker arrives as a `developer`-role model
input Codex itself labels `hooks.additional_context`.

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
| `outcome-0.154.0-positive.md` | the 0.154.0 run, its evidence, and what it left unprobed |
| `outcome-0.156.1-positive.md` | the 0.156.1 authenticated run: all five events delivered, plus the PreToolUse deny+advice shape |
| `run.sh` | reproduces the probe headlessly; `CODEX_ADVISORY_AUTH=1` for the authenticated half, `CODEX_ADVISORY_DENY_PROBE=1` for the deny+advice shape |
| `emit-advisory.sh` | the hook: answers an event with `additionalContext`, records the inbound payload |
| `emit-deny-advice.sh` | the deny+advice hook used by `CODEX_ADVISORY_DENY_PROBE=1` |
| `hooks-both.json` | the exact config used, both events, one marker each |
| `hook-log-both.jsonl` | the two inbound payloads as received |
| `hook-lifecycle-both.txt` | Codex's own `hook:` lines — a rejection would show here |
| `marker-records-both.json` | the two `response_item` records that carry the markers into model input |
| `hooks-all5.json` | the 0.156.1 config, all five events, one marker each |
| `hook-log-all5.jsonl` | the five inbound payloads as received |
| `hook-lifecycle-all5.txt` | Codex's own `hook:` lines for that run |
| `marker-records-all5.json` | the `response_item` records carrying all five markers into model input |
| `hook-stdout-all5.txt` | each hook's reply, replayed from the recorded payloads |
| `deny-*.txt`, `deny-hook-log.jsonl` | the `PreToolUse` deny+advice shape probe's raw output |

## Method

`CODEX_HOME=<scratch> codex --dangerously-bypass-hook-trust exec "<prompt>"`,
then read the markers out of the session rollouts under
`$CODEX_HOME/sessions/`. Unauthenticated only `SessionStart` and
`UserPromptSubmit` run — they fire *before* the turn reaches the API, so a 401'd
turn still produces that much evidence. The remaining three need a completed
turn, so `CODEX_ADVISORY_AUTH=1` symlinks the logged-in `~/.codex/auth.json`
into the scratch home. `emit-advisory.sh` takes its
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
- **`SubagentStart` only fires when the model actually spawns a subagent.**
  A run that never spawns one is indistinguishable from a missing channel, so
  the default prompt asks for a subagent explicitly; `run.sh` passes
  `--enable multi_agent`.

## Probing the remaining three events

`PreToolUse`, `PostToolUse` and `SubagentStart` are **proven as of
0.156.1**. All three need a completed turn — a 401'd turn never executes a
tool — and `SubagentStart` additionally needs the model to spawn a subagent, so
`run.sh`'s default prompt asks for exactly that. `CODEX_ADVISORY_AUTH=1`
symlinks the logged-in `~/.codex/auth.json` into the scratch `CODEX_HOME`; the
file's contents are never read, copied or printed. `run.sh` is one command for
all five events, and the deny+advice companion (`CODEX_ADVISORY_DENY_PROBE=1`)
answers the decision-slot shape question.

Nothing here says an advisory *changes behaviour* on Codex, only that it arrives.
