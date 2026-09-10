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

All three engines refused to load hooks until their directory was trusted,
which is itself recorded in §8 as a uniform fail-open path.

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
