# Pi multi-extension `tool_call` consolidation

LOCAL probes answering §12's "Pi's multi-extension block consolidation" item,
captured on 2026-09-14 against Pi 0.85.1 (`@earendil-works/pi-coding-agent`,
installed via this host's nix-config `home/ai/pi`).

## How they were made

Five throwaway extensions in this directory, each a single `tool_call`
handler:

| File | Behaviour |
|---|---|
| `ext-logger.ts` | Logs, returns nothing (allow) |
| `ext-blocker.ts` | Logs, returns `{ block: true, reason: "BLOCKER_DENY" }` for `bash` |
| `ext-blocker2.ts` | Same shape, `reason: "BLOCKER2_DENY"` — a second, distinct blocker |
| `ext-thrower.ts` | Logs, throws `Error("THROWER_BOOM")` for `bash` |
| `ext-mutator.ts` | Rewrites `event.input.command` to `"echo MUTATED"` in place, returns nothing |

Each logs `"<NAME> saw <toolName>"` to `/tmp/pi-probe-order.log` before acting,
so the log's line order across two loaded extensions shows which handlers
actually ran and in what order — a handler that never runs leaves no line.

Run with the real (non-Nix-wrapper) `pi` binary, an isolated
`PI_CODING_AGENT_DIR` seeded with only `models.json`/`settings.json`
pointing at a local Lemonade server (`http://127.0.0.1:13305`, no API key,
no network egress — `PI_OFFLINE=1`), `--no-extensions` (disables
auto-discovery so only the explicit `-e` pair loads), and `--tools bash`
(keeps the small local model from wandering into other tools once bash
starts getting blocked):

```
env PI_CODING_AGENT_DIR=<scratch dir> PI_OFFLINE=1 \
  <libexec>/pi/pi --no-session --print --mode json \
  --provider Lemonade --model Qwen3-Coder-30B-A3B-Instruct-GGUF \
  --no-extensions --no-context-files --no-skills --no-prompt-templates --no-themes \
  --tools bash \
  -e ext-A.ts -e ext-B.ts \
  "Call the bash tool once to run: echo hi"
```

Load order is the `-e` flag order. `--mode json` streams one JSON event per
line; the `tool_execution_end` event carries the exact text shown to the
model, which is what "whose reason is shown" (question 2) reads off.

`sample-block-then-allow.jsonl` (blocker loaded first, logger second) and
`sample-mutate-then-block.jsonl` (mutator loaded first, blocker second) are
two full run transcripts kept as concrete artifacts; the table below is the
same evidence for the other five scenarios, read off `order.log` and each
run's `tool_execution_end` line rather than committed in full — the other
runs' transcripts are near-identical modulo the tool-call id and are mostly
streaming-delta noise. Both committed transcripts have their opaque
`toolCallId`/`id`/`responseId` values replaced with stable
`PROBE-TOOLCALL-NN` / `chatcmpl-PROBE-NN` placeholders (same id gets the
same placeholder everywhere it recurs, preserving correlation) — Pi's own
ids are base62, not hex, so left as captured they trip this repo's `typos`
pre-commit hook on essentially every line. The session `id` (a uuid) is
left untouched; it's hex and already covered by `_typos.toml`'s existing
ignore pattern.

An early unrestricted-tools run (no `--tools bash`) is *not* reproduced
here: without a tool allowlist, a blocked `bash` call sent the small local
model wandering into `read`/`write` instead, and it wrote a stray file into
the actual worktree being verified — caught and reverted, not a Pi
consolidation finding. `--tools bash` above is what keeps a probe run
contained to the extension being tested.

## Results

| Scenario | Load order | `order.log` | Tool result shown | Reading |
|---|---|---|---|---|
| allow, then block | logger, blocker | `LOGGER`, `BLOCKER` | `BLOCKER_DENY`, `isError:true` | Both ran; a later block still blocks |
| block, then allow | blocker, logger | `BLOCKER` only | `BLOCKER_DENY`, `isError:true` | Logger never runs — first block short-circuits |
| two blockers, A first | blocker, blocker2 | `BLOCKER` only | `BLOCKER_DENY` | First-registered blocker wins; second never runs |
| two blockers, B first | blocker2, blocker | `BLOCKER2` only | `BLOCKER2_DENY` | Confirms it's load order, not extension identity — reversing `-e` reverses the winner |
| throw, then allow | thrower, logger | `THROWER` only | `THROWER_BOOM`, `isError:true` | A throw short-circuits exactly like an explicit block; fails closed |
| allow, then throw | logger, thrower | `LOGGER`, `THROWER` | `THROWER_BOOM`, `isError:true` | The earlier allow's own effects stand, but the call still ends up blocked — throw-anywhere-in-the-chain is deny-wins |
| mutate, then allow | mutator, logger | `MUTATOR`, `LOGGER` | `"MUTATED\n"` (real echo output) | The mutation from the first handler is what actually executed — no re-validation, later handlers and execution both see it |
| mutate, then block | mutator, blocker | `MUTATOR`, `BLOCKER` | `BLOCKER_DENY`, `isError:true` | Mutator still mutates `event.input` in place before the blocker runs, but the call never executes, so there is no executed output to observe either way — `tool_execution_start`'s `args` (captured before handlers run) shows the original, unmutated command in every attempt |

Every scenario above was re-run to a single clean tool call (`--tools bash`
plus a "call it once" prompt) after the unrestricted-tools run above went
off the rails; `order.log` and the `tool_execution_end` grep shown in each
run are the evidence for the table.

## Reading against the §12 questions

1. **Order and stop-at-first-block.** `tool_call` handlers run sequentially
   in extension load order (`-e` order here; the general case is each
   engine's/registration's own load order), not in parallel — and Pi stops
   at the first handler that returns `{ block: true }`. Confirmed by
   "block, then allow" (logger never runs) and by reversing the two
   blockers' load order and watching the winner reverse with it.
2. **Mixed block/non-block.** The call is blocked whenever *any* handler up
   to the first block returns one — order doesn't change that, only which
   reason is shown. The reason shown is the first blocking handler's own
   `reason` string, verbatim, as the tool result content with `isError:
   true`.
3. **Throwing/timing-out extension.** A thrown error inside a `tool_call`
   handler fails **closed**: the call is blocked, the error's message is
   the content shown to the model (`isError: true`), and it short-circuits
   the remaining handler chain exactly like an explicit `block: true` —
   confirmed LOCAL, and matches the bundled docs' own statement (below).
   Handlers that already ran before the throw keep whatever they already
   did (their own log lines stand); only the tool call itself is blocked.
   Timeout behaviour was not probed — no timeout knob is documented for
   `tool_call` handlers the way one exists for hookyard's own dispatch, so
   this stays open unless raised as its own §12 item.
4. **Input mutation.** `event.input` is mutable; the mutating handler's
   change is what the tool actually executes, with no re-validation —
   confirmed by the "mutate, then allow" run's real `"MUTATED\n"` output
   (the model asked for `echo hi`, Pi ran `echo MUTATED`). Probed the
   combination directly rather than inferring it: "mutate, then block"
   still mutates `event.input` in place before the blocker runs, but
   because the call never executes, the mutation has no separately
   observable effect — there is no "does the mutation survive the block"
   question to answer once execution itself doesn't happen. The
   `updatedInput`-style interaction §12 worries about for Codex (an
   allow-and-rewrite from one handler racing a deny from another) doesn't
   have a Pi analogue for this reason: mutation only ever matters through
   the tool actually running, and a block prevents that outright.
5. **Consequence for §3.1.** Pi's rule is deny-wins in the sense that
   matters for build mode's cross-plugin case: any subscribed extension
   that blocks (by return value or by throwing) wins over every other
   extension regardless of what they returned, with only the winning
   reason surfaced. It differs from Claude Code's and Cursor's documented
   deny-wins in mechanism (short-circuit-on-first-block through a
   sequential chain, not a fold over all handlers' verdicts), but the
   externally observable guarantee — one plugin's deny is never overridden
   by another plugin's allow — holds. See the §3.1/§12 edit this task makes
   for the resulting wording.

## Corroborating doc-grade source

Pi's own bundled `docs/extensions.md` (shipped inside the 0.85.1 package,
`<libexec>/pi/docs/extensions.md`) states, independent of this probe:

- "`tool_call` … **Can block.**" and "Return values from `tool_call` control
  blocking via `{ block: true, reason?: string, terminate?: boolean }`"
  (lines ~780-792).
- "Later `tool_call` handlers see mutations made by earlier handlers" (line
  790) — consistent with sequential, load-ordered execution.
- Under "Error Handling": "`tool_call` errors block the tool (fail-safe)"
  (line 2925) — the documented fail-closed behaviour this probe confirms
  LOCAL.

It does not itself state the stop-at-first-block short-circuit or which
reason wins under multiple blockers; those two are LOCAL-only findings from
this probe. The compiled `pi` binary is a single ~112 MB bundled executable
on this host, not readable source, so no source-line permalink is cited for
the dispatcher itself; `earendil-works/pi` on GitHub is the upstream repo
(from `package.json`'s `repository` field) if a future pass wants to pin an
exact source permalink instead of the bundled-docs citation used here.

## Cleanup

Every probe ran with `PI_CODING_AGENT_DIR` pointed at a scratch directory
under this session's scratchpad, never at `~/.pi/agent`. Nothing was
installed or registered in the real Pi config. The one file a runaway probe
wrote into the actual worktree (`pi-extension-consolidation.md`, from the
unrestricted-tools run described above) was trashed with `gtrash put`, not
committed.
