# Captured hook payloads

Real hook payloads, captured from live agent sessions on 2026-09-10. These are
the ground truth behind §7's inbound field table; before them, that table was
derived from each engine's documentation.

| File | Engine | Event | Notes |
|---|---|---|---|
| `claude-PreToolUse.json` | Claude Code 2.1.263 | `PreToolUse` | |
| `claude-PostToolUse.json` | Claude Code 2.1.263 | `PostToolUse` | `tool_response` is a real object |
| `claude-PreToolUse-DENY.json` | Claude Code 2.1.263 | `PreToolUse` | the call this denied never ran |
| `codex-pre_tool_use.json` | Codex 0.153.4 | `PreToolUse` | `tool_name` is `Bash`, not `apply_patch` |
| `codex-pre_tool_use-DENY.json` | Codex 0.153.4 | `PreToolUse` | Codex reported `Blocked by hook` |
| `codex-user_prompt_submit-TICK.json` | Codex 0.153.4 | `UserPromptSubmit` | the entry declaring no `timeout`; ran 180 s unkilled |
| `cursor-preToolUse.json` | Cursor 2026.09.08 | `preToolUse` | `tool_name` is `Shell`; `cwd` is empty |
| `cursor-beforeShellExecution.json` | Cursor 2026.09.08 | `beforeShellExecution` | no `tool_name`; top-level `command` + `sandbox` |
| `cursor-postToolUse.json` | Cursor 2026.09.08 | `postToolUse` | `tool_output` is a JSON *string*, not an object |
| `cursor-preToolUse-DENY.json` | Cursor 2026.09.08 | `preToolUse` | denying here short-circuits `beforeShellExecution` |
| `pi-session_start.json` | Pi 0.85.1 | `session_start` | |
| `pi-input.json` | Pi 0.85.1 | `input` | Pi's `prompt_submit` equivalent; `source: "interactive"` |
| `pi-tool_call.json` | Pi 0.85.1 | `tool_call` | `tool_name` is `bash`, lowercase |
| `pi-tool_call-DENY.json` | Pi 0.85.1 | `tool_call` | denied a `touch`; the file did not exist afterward |
| `pi-tool_result.json` | Pi 0.85.1 | `tool_result` | does not fire at all on a denied call |
| `pi-turn_end.json` | Pi 0.85.1 | `turn_end` | carries `turn_index`, an integer, not a correlation id |

Pi's payloads are not like the other ten. Pi has no subprocess hook
protocol of its own — it fires in-process TypeScript extension callbacks,
not a JSON blob on stdin — so there is no "Pi's payload" to capture in the
sense the other three engines' rows mean. What's committed here is
**hookyard's own bridge extension's output**: the bridge reads Pi's live
`event`/`ctx` objects and serializes the fields it finds into this shape,
the same shape `docs/design/hookyard.md` §7 documents as the inbound
envelope's Pi source. `pi_version` is the clearest instance of the
difference — it is a field hookyard's bridge **injects**, mirroring
`cursor_version`'s role as a discriminator, not a field Pi sends on its own.
Every other engine's fixture is a transcript of what the engine actually
put on the wire; Pi's is a transcript of what hookyard's own code chose to
write down.

## How they were captured

`capture-hook.sh` in this directory is the hook that produced them, kept so a
future engine version can be re-checked rather than re-reasoned about. It
takes an engine label, an event label, a mode (`observe`, `deny`, `tick`) and
an output path, appends one record per firing, then answers per mode — exit 0
to observe, a per-engine deny verdict to test enforcement, or one tick a
second to measure an engine's undeclared timeout.

Each engine ran one trivial prompt — "Run the shell command: echo
hookyard-probe" — against a hook that appended its stdin verbatim before
answering. Nothing in the real configuration was touched: Codex ran under a
scratch `CODEX_HOME`, Claude Code under a scratch `CLAUDE_CONFIG_DIR`, and
Cursor from a scratch directory with its own project-level
`.cursor/hooks.json`. Authentication was reached by symlink, never copied.

Cursor, Codex and Claude Code all refused to load hooks until their
directory was trusted, which is recorded in §8. Pi does not belong in that
list: it does gate project-local resources behind its own trust store, but
that gate does not cover the **global** extensions directory hookyard
registers into, so it was never hit during capture. §8 corrects the
document's earlier claim that this was uniform across all engines.

Pi's captures used a different scratch setup, because its hook surface is an
in-process extension rather than a subprocess protocol (above). `pi`'s own
config dir was scratched via `PI_CODING_AGENT_DIR`, `PI_OFFLINE=1` and
`PI_AGENT_HOOKS=` (the last empties the Nix wrapper's own guard list so it
no-ops during the probe), and a probe extension was loaded with `-e` that
piped each event straight into this directory's own `capture-hook.sh`. The
model was a local Lemonade server (`http://127.0.0.1:13305/v1`,
Qwen3-Coder-30B) rather than a hosted one — no credentials involved and no
API cost — running the same probe prompt as the other three engines.

## What was changed in these files

Only two things, both mechanical:

- `user_email` (Cursor only) is replaced with `<REDACTED-EMAIL>`.
- The throwaway probe directory prefix is replaced with `<PROBE>`.

Session, turn and tool-call identifiers are left as captured. They identify
sessions that no longer exist and are the shape of the fields, which is the
point of keeping them.

## Reading them

The three engines agree more than §7 originally assumed — every one sends
`session_id` — and disagree in two places that matter: Cursor's `cwd` is the
empty string with the real path in `workspace_roots[0]`, and each engine's
second identifier has a different name (`prompt_id`, `turn_id`,
`generation_id`). No payload says which config file registered the hook,
which is what decided §8's cross-registration suppression rule; every payload
does identify its own engine unambiguously (`cursor_version`,
`prompt_id`/`effort`, `turn_id`).

Pi sends `session_id` too — stable across all five of its captures — but has
no second, narrower identifier at all; `turn_index` on `turn_end` is a plain
integer, not a correlation key. Its `cwd` is populated on every event, the
same as Claude Code and Codex, so it needs no `workspace_roots[0]`-style
fallback. And unlike the other three, its discriminator (`pi_version`) is
not evidence Pi chose to send — it is a value hookyard's own bridge writes
into a payload hookyard itself authors, which is the honest reading of every
row in the Pi section of the table above.
