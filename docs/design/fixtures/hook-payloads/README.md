# Captured hook payloads

Real hook payloads, captured from live agent sessions on 2026-09-10,
2026-09-15 and 2026-09-17. These are the ground truth behind §7's inbound
field table; before them, that table was derived from each engine's
documentation.

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
| `pi-session_start.json` | Pi 0.85.1 | `session_start` | `reason` is `"startup"`, the only value reachable headlessly |
| `pi-input.json` | Pi 0.85.1 | `input` | Pi's `prompt_submit` equivalent; `source: "interactive"` |
| `pi-tool_call.json` | Pi 0.85.1 | `tool_call` | `tool_name` is `bash`, lowercase |
| `pi-tool_call-DENY.json` | Pi 0.85.1 | `tool_call` | denied a `touch`; the file did not exist afterward |
| `pi-tool_result.json` | Pi 0.85.1 | `tool_result` | does not fire at all on a denied call |
| `pi-turn_end.json` | Pi 0.85.1 | `turn_end` | carries `turn_index`, an integer, not a correlation id |
| `pi-session_shutdown.json` | Pi 0.85.1 | `session_shutdown` | `reason` is `"quit"`; no canonical counterpart, so a manifest names it `pi:session_shutdown` |
| `claude-SessionStart.json` | Claude Code 2.1.272 | `SessionStart` | no `prompt_id`/`effort`; the router's yard-mode fallback exists because of this (R-G) |
| `claude-UserPromptSubmit.json` | Claude Code 2.1.272 | `UserPromptSubmit` | carries the literal probe `prompt` |
| `claude-Stop.json` | Claude Code 2.1.272 | `Stop` | `last_assistant_message` is `"ok"`; `background_tasks`/`session_crons` are empty |
| `claude-SessionEnd.json` | Claude Code 2.1.272 | `SessionEnd` | `reason` is `"other"` |

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

`session_file` and `argv` are that choice at its plainest: neither is a
field Pi hands a handler, and both appear on every Pi event. `session_file`
is `ctx.sessionManager.getSessionFile()`, emitted as `""` when a
`--no-session` run has no transcript on disk. `argv` is
`process.argv.slice(2)` — the invoking command line, wrapper-injected flags
included, which is why the `-e`/`--skill` entries in these captures belong
to this machine's Nix wrapper rather than to the probe — with
secret-bearing flags dropped: Pi takes `--api-key <key>`, so the bridge
drops any `--…` flag whose name contains `key`, `token`, `secret` or
`password`, along with its value.

## How they were captured

`capture-hook.sh` in this directory is the hook that produced them, kept so a
future engine version can be re-checked rather than re-reasoned about. It
takes an engine label, an event label, a mode (`observe`, `deny`, `tick`) and
an output path, appends one record per firing, then answers per mode — exit 0
to observe, a per-engine deny verdict to test enforcement, or one tick a
second to measure an engine's undeclared timeout. Given a directory rather
than a file it writes the first firing of each event to
`<engine>-<hook_event_name>.json` instead, which is the fixture itself; that
is how Pi's are captured (below).

The 2026-09-10 captures ran one trivial prompt per engine — "Run the shell
command: echo hookyard-probe" — against a hook that appended its stdin
verbatim before answering. Nothing in the real configuration was touched:
Codex ran under a scratch `CODEX_HOME`, Claude Code under a scratch
`CLAUDE_CONFIG_DIR`, and Cursor from a scratch directory with its own
project-level `.cursor/hooks.json`. Authentication was reached by symlink,
never copied.

The four `claude-*.json` fixtures dated 2026-09-15 (`SessionStart`,
`UserPromptSubmit`, `Stop`, `SessionEnd`) were captured differently: unlike
the 2026-09-10 captures, these ran under the real `CLAUDE_CONFIG_DIR`, with a
scratch `--settings` overlay whose hooks dumped stdin, via `claude -p --model
haiku 'Reply with the single word ok.'`. `Notification` and `PreCompact`
don't fire in a headless `-p` run, so they were not captured this way.

Cursor, Codex and Claude Code all refused to load hooks until their
directory was trusted, which is recorded in §8. Pi does not belong in that
list: it does gate project-local resources behind its own trust store, but
that gate does not cover the **global** extensions directory hookyard
registers into, so it was never hit during capture. §8 corrects the
document's earlier claim that this was uniform across all engines.

Pi's captures use a different scratch setup, because its hook surface is an
in-process extension rather than a subprocess protocol (above). All seven
`pi-*.json` files were re-captured on 2026-09-17 against pi 0.85.1, and
through hookyard's own installed bridge rather than through a probe
extension: what is committed has to be the bridge's output, and a probe
extension would only be a second author of the same shape. `pi`'s config dir
was scratched via `PI_CODING_AGENT_DIR`, holding a settings.json naming the
model, a copy of the public `models-store.json` catalogue (without it pi
silently falls back to its built-in default model) and
`~/.pi/agent/auth.json` reached by symlink, never copied and never read.
`hookyard install --pi-settings` pointed at that settings.json with
`--router-path` naming a two-line script that execs `capture-hook.sh` in
directory mode, and the probe was `pi -p --approve` from a scratch project
directory, under `PI_AGENT_HOOKS=` (which empties the Nix wrapper's own guard
list so it no-ops) and with `PI_OFFLINE` unset, since the model is hosted.
Six of the seven came from one run of the same "Run the shell command: echo
hookyard-probe" prompt the other engines were captured with, so they differ
from the files they replace only where the bridge itself changed.
`pi-tool_call-DENY.json` came from a second run, prompted to `touch
SIDE-EFFECT.txt` and nothing else, whose router answered
`{"block":true,…}`: the file did not exist afterward, and no `tool_result`
fired.

The model was `openrouter/deepseek-v4.1-flash`. The 2026-09-10 captures used
a local Lemonade server (`http://127.0.0.1:13305/v1`, Qwen3-Coder-30B); it is
deliberately not used here, because it saturates the machine these captures
run on.

### What a live probe settled

Facts about pi 0.85.1 that the bridge depends on and no fixture shows, each
confirmed by running the code inside an extension pi loaded, not read out of
documentation:

| Question | Answer |
|---|---|
| Does `import.meta.url` resolve under jiti? | Yes — `file:///…/probe.ts`; `fileURLToPath` and `dirname` both work |
| What is `process.argv`? | `["bun", "/$bunfs/root/pi", …every arg…]`, so `slice(2)` is the invoking argv — including flags a wrapper injected, not only the caller's own |
| Is pi's version reachable from node builtins? | Yes — `resolve(dirname(process.execPath), "package.json")`. `process.argv[1]` is the useless `/$bunfs/root/pi` |
| `ctx.sessionManager.getSessionFile()`? | An absolute path under `<agent dir>/sessions/`; `undefined` under `--no-session` |
| Does a `before_agent_start` `{message:{customType,content,display:false}}` reach the model? | Yes — persisted into the session jsonl verbatim. It fires once per prompt, not once per session |
| Does a `tool_result` `{content:[…]}` return reach the model? | Yes, proven end to end; the tool's original block survives alongside it |
| Is `--api-key` a real pi flag? | Yes (`pi --help`) — the concrete reason `argv` is sanitised |

Only `startup` is reachable for `session_start`'s `reason`. `pi -p -c`,
`pi -p --fork <id>` and `pi -p --session <id>` all report `reason: "startup"`;
`new`, `resume`, `fork` and `reload` come only from in-process session
replacement (`/new`, `/resume`, `/fork`, `ctx.newSession()`), which `-p` never
reaches and which hung until the harness timeout when driven from an
extension. So there is no `pi-session_start-<reason>.json` — writing one by
hand would make it the only file here that is reasoning rather than
transcript. `session_shutdown`'s `reason` values other than `"quit"` and its
`targetSessionFile` share that limit, and `session_before_compact` remains
inferred rather than captured.

## What was changed in these files

This repo is public, and these fixtures are published — so beyond the probe
scaffolding, anything that identifies the machine or account that captured
them is redacted too. Three things, all mechanical:

- `user_email` (Cursor only) is replaced with `<REDACTED-EMAIL>`.
- The throwaway probe directory prefix is replaced with `<PROBE>`. Pi's
  `session_file` carries that prefix twice — once as a path and once inside
  the session directory's name, where pi flattens the project path by
  replacing every `/` with `-` — and both spellings are the same
  substitution.
- The capturing account's home directory is replaced with
  `/home/<REDACTED-USER>`, keeping the path shape a real home path has
  rather than collapsing it to a bare token — these fixtures are the payload
  spec the bridge is checked against, so a redaction that changes a field's
  shape would break the thing it's pinning. This shows up in three places:
  Pi's `argv` (`--skill`/`--prompt-template` point at the real
  `~/.claude/skills` and `~/.claude/commands`, which are not scratched by
  the probe setup), the `pi-multi-extension-consolidation` sample
  transcripts' `cwd`, and — flattened, `/` replaced with `-` the same way
  `session_file` is — inside a `transcript_path` project-directory segment
  that both Claude Code and Cursor derive from the real cwd rather than the
  scratched probe root, so the earlier `<PROBE>` substitution alone didn't
  catch it.

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

Pi sends `session_id` too — stable across every event of one run — but has
no second, narrower identifier at all; `turn_index` on `turn_end` is a plain
integer, not a correlation key. Its `cwd` is populated on every event, the
same as Claude Code and Codex, so it needs no `workspace_roots[0]`-style
fallback. And unlike the other three, its discriminator (`pi_version`) is
not evidence Pi chose to send — it is a value hookyard's own bridge writes
into a payload hookyard itself authors, which is the honest reading of every
row in the Pi section of the table above.
