# Result: Codex delivers `additionalContext` — POSITIVE

**codex-cli 0.154.0 · 2026-09-24 · halo · unauthenticated.** Both `SessionStart`
and `UserPromptSubmit` delivered their hook's `additionalContext` into the
model-visible prompt input. The claim below is **false**, and the capability
table must be re-derived from this run rather than from Codex's documentation.

## What was claimed

> Codex has no advisory channel on any event, full stop: this is a settled
> boundary, not a gap to fill later — no handler can get advice to Codex by any
> means this package offers. — `internal/verdict/capability.go`

> Codex has no advisory channel at all — a settled boundary, not an open
> question — Only fire-and-forget hooks observed deployed. —
> `docs/design/hookyard.md`

## What was run

One `hooks.json` in a throwaway `CODEX_HOME`, both events routed at
`emit-advisory.sh` with a distinct marker each, then a headless run:

```
CODEX_HOME=<scratch> codex --dangerously-bypass-hook-trust exec "say hi"
```

`hooks-both.json` is that config verbatim. No credentials were needed, and none
were present: the turn aborts at the API with a 401, but both hooks run *before*
that, which is why the probe is reproducible on any host with a Codex binary.

## Evidence

Both hooks fired and completed — no rejection of the output shape:

```
hook: SessionStart
hook: SessionStart Completed
hook: UserPromptSubmit
hook: UserPromptSubmit Completed
```

Both markers reached the model-visible input, each as its own `response_item`,
carrying Codex's own label for where it came from — `marker-records-both.json`:

```json
{"type":"response_item","role":"developer","text":"SS-MARKER","kinds":["hooks.additional_context"]}
{"type":"response_item","role":"developer","text":"UPS-MARKER","kinds":["hooks.additional_context"]}
```

`role: "developer"` is what the documentation promised ("added as extra
developer context"), `input_text` is a model input item rather than a log line,
and `content_item_kinds: ["hooks.additional_context"]` is Codex attributing the
text to the hook itself. That last field is what makes this wire-level evidence
rather than an inference from a model's reply: the marker was never in the
prompt, so its presence in the input list can only have come through the
channel, and Codex names the channel for us.

## What this settles

| event | fires | `additionalContext` reaches model input | label |
| --- | --- | --- | --- |
| `SessionStart` | ✅ | ✅ | `hooks.additional_context` |
| `UserPromptSubmit` | ✅ | ✅ | `hooks.additional_context` |

Those are exactly the two events the memory-layer proposal needed for its
index-at-session-start and ranked-retrieval-at-prompt tiers, and both work.

## What it does not settle

- **`PreToolUse` / `PostToolUse` / `SubagentStart`.** Not registered here, and
  they cannot be reached without credentials — a 401'd turn never executes a
  tool. The docs specify the same channel on all three; that is documentation,
  not a probe.
- **What the model does with it.** Delivery is not comprehension. Nothing here
  says an advisory *changes behaviour* on Codex, only that it arrives.
- **Anything about hookyard's render arm**, which does not emit for Codex at
  all today. A slot is not a fix.

## Dead ends worth not repeating

- **`codex debug prompt-input` does not run `SessionStart` hooks.** It renders
  the prompt input list (an array of `developer`/`user` messages) and looks like
  the ideal no-auth wire-level probe, but the hook never fires, so it silently
  measures nothing. `codex exec` plus the session rollout is the path that works.
- **A `CODEX_HOME` under `/tmp` is degraded.** Codex warns *"Refusing to create
  helper binaries under temporary dir"* and proceeds with PATH aliases missing.
  `run.sh` uses `$XDG_CACHE_HOME` for that reason.
- **`--dangerously-bypass-hook-trust` is required.** Without it Codex skips an
  unreviewed hook, which is indistinguishable from a channel that does not
  deliver.
- **`codex features list` reports `hooks stable true`** — hooks are on by
  default, so a silent no-op is never the feature flag.
