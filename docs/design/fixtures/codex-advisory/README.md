# Codex's advisory channel

Does Codex deliver `additionalContext` back to the model? Issue #87. **UNRUN —
nothing here has been executed.** See *Why this is unrun* below.

## The claim under test

Two places in this repo assert that Codex cannot receive advice on any event,
and both rest on not having looked:

> Codex has no advisory channel on any event, full stop: this is a settled
> boundary, not a gap to fill later — no handler can get advice to Codex by any
> means this package offers. — `internal/verdict/capability.go:70-72`

> Codex has no advisory channel at all — a settled boundary, not an open
> question — Only fire-and-forget hooks observed deployed; Codex's deny path and
> its default timeout when an entry declares none were not verified this pass.
> — `docs/design/hookyard.md:2334`

The stated evidence is an absence of observation. Codex's own documentation and
source point the other way: `SessionStart` accepts
`hookSpecificOutput.additionalContext` and its docs say that text *"is added as
extra developer context"*; `UserPromptSubmit` carries it as
`UserPromptSubmitHookSpecificOutputWire { additional_context: Option<String> }`
(`codex-rs/hooks/src/schema.rs`); `PreToolUse` documents it *"to add
model-visible context without blocking"*; `PostToolUse` and `SubagentStart`
carry it too. `additionalContextLimit` — default ~2500 tokens, spilling to disk
above it — exists because that channel gets used.

That table cell is stale in a second, separate way, which is worth fixing
whether or not the probe is ever run: it says Codex's deny path and its
undeclared default timeout "were not verified this pass", but both have since
been captured as fixtures — `codex-pre_tool_use-DENY.json` (Codex reported
`Blocked by hook`) and `codex-user_prompt_submit-TICK.json` (ran 180 s
unkilled). The row was never updated when the fixtures landed.

## Method

`run.sh` creates a throwaway `CODEX_HOME`, writes a `hooks.json` that answers
`SessionStart` and `UserPromptSubmit` with `additionalContext`, and prints the
launch line and the prompt to type. It does not launch Codex — see below.

The hook (`emit-advisory.sh`) takes its marker as an **argument, not an
environment variable**, because an engine may not pass its own env to a hook
subprocess — the lesson `capture-hook.sh` already records. It derives
`hookEventName` from the **inbound payload** rather than an argument, because a
mismatched event name is itself a shape error that would make Codex reject the
answer, and the probe would then measure the wrong thing.

`--dangerously-bypass-hook-trust` is required, and is Codex's documented path
for one-off automation whose hook sources are already vetted. Without it Codex
skips an unreviewed hook, and the probe would measure nothing while looking
exactly like a negative result.

## Two evidence levels

| Level | Question | How |
|---|---|---|
| **L1 — acceptance** | Does Codex accept the output shape without failing the hook run? | the hook log records each firing, and Codex reports a failed hook run as an error. A fired hook plus no reported failure is acceptance |
| **L2 — delivery** | Does the marker reach the model's context? | the marker is in the hook's output and was never in the prompt, so a reply containing it can only have come through the channel. Read off the transcript |

L1 alone does **not** settle the claim — a field can parse and be discarded.
L2 does.

The stronger form of both levels is the house method used in
`pi-pre-tool-advisory/`: point the engine at a fake OpenAI-compatible server and
read what reached the model **off the wire**, deterministically, with no
credentials. That is the right way to run L2 if someone is willing to wire up
Codex's `model_providers` config; it is not attempted here because the config
surface could not be verified from this host (see below), and a fake-server
fixture that has never answered a request is worse than none — it would present
an untested harness as a working one.

## Why this is unrun

Codex is not installed on the personal profile — `codex` is absent from `PATH` on
the machine this fixture was written on, and this fleet runs the non-Claude
engines on the work profile only. There is no Codex host reachable from here, so
the probe could not be falsified by its author. That is the whole reason this
directory is a fixture and not a result: the defect it descends from is a claim
made without a probe, and answering it with a second unverified claim would
repeat it in the opposite direction.

## Recording the result

Add a result file here — `result-<codex-version>-<positive|negative>.md` — with:

- the Codex version (`codex --version`),
- the marker used, and whether it appeared in the reply,
- the raw hook log, and the raw L1 error if Codex rejected the shape,
- anything surprising about which events fired.

Then, per the outcome:

- **Marker arrives.** `HasAdvisorySlot` gains Codex for the confirmed events,
  `render` gains the Codex advisory arm, and both quoted passages above are
  rewritten from the run rather than from the docs. `hookyard.md` gains the
  outbound-delivery evidence that `pre_tool`'s advisory already has on Claude
  Code.
- **Marker does not arrive, hook accepted.** The boundary stands, and now rests
  on a measurement — rewrite the passages to say *that*, instead of resting on
  an absence of observation.
- **Hook rejected.** Record the exact rejection. This is the one outcome that
  would justify the word "solely" in the current claim, and it should carry the
  error text.