# hookyard

Register agent hooks once, route them to every coding agent.

Claude Code, Codex and Cursor each declare hooks in their own config format,
under their own event names, with their own payload shape. hookyard is a
standalone static binary that holds one declarative table of which handler
runs on which event on which engine, renders that table into each engine's
native config, and — when an engine fires a hook — decodes the payload into a
normalized shape, runs the matching handlers concurrently under a shared
deadline, folds their verdicts with a deny-wins consolidation, and renders the
result in the shape the calling engine accepts. All three engines have a
confirmed deny path, so a guard enforces on all three; a decision only has
somewhere to land on `pre_tool` (plus a handful of Cursor-scoped events), so a
verdict on any other event is recorded but not enforced, and an `allow`
rendered to Codex is recorded rather than enforced too — Codex rejects an
explicit allow by name, so nothing is printed and its own permission flow
runs instead. An `ask` is not one of those cases: on Codex, whose decision
shape is binary, a consolidated `ask` degrades to an enforced `deny` with a
reason explaining why.

## How it works

Two passes. `hookyard install` writes the table down into every engine's
config; `hookyard route` is what an engine actually invokes when a hook
fires.

```mermaid
flowchart TD
    subgraph reg["hookyard install"]
        direction LR
        M1["repo A's hookyard.json"] --> INSTALL["hookyard install"]
        M2["repo B's hookyard.json"] --> INSTALL
        INSTALL -->|marker-scoped strip| CCCFG["~/.claude/settings.json"]
        INSTALL -->|marker-scoped strip| CXCFG["~/.codex/config.toml"]
        INSTALL -->|marker-scoped strip| CUCFG["~/.cursor/hooks.json"]
        INSTALL --> TABLE[("table.json\nin the state dir")]
    end

    subgraph route["hookyard route, invoked by each engine's native config"]
        direction TB
        CCFIRE["Claude Code fires a hook"] --> CCPAY["native payload\n(prompt_id discriminator)"]
        CXFIRE["Codex fires a hook"] --> CXPAY["native payload\n(turn_id discriminator)"]
        CUFIRE["Cursor fires a hook"] --> CUPAY["native payload\n(cursor_version discriminator)"]
        CCPAY --> ENV["normalized envelope\n(envelope.Decode)"]
        CXPAY --> ENV
        CUPAY --> ENV
        TABLE --> SELECT["select the handlers\nthis payload matches"]
        ENV --> SELECT
        SELECT --> FANOUT["run them concurrently,\none 4.5s deadline"]
        FANOUT --> FOLD["fold verdicts,\ndeny wins"]
        FOLD --> RENDER["render for the\ncalling engine"]
        RENDER --> CCOUT["Claude Code:\npermissionDecision +\nadditionalContext"]
        RENDER --> CXOUT["Codex:\npermissionDecision only,\nask degrades to deny"]
        RENDER --> CUOUT["Cursor:\npermission +\nuser_message"]
        CCOUT --> RECORD["event record appended\n— always, every path"]
        CXOUT --> RECORD
        CUOUT --> RECORD
    end
```

A few things worth calling out because they're not visible from the
`hookyard` label alone: each engine's writer strips only its own
marker-tagged entries out of a config file it shares with other writers, and
refuses to touch a file it cannot parse rather than clobbering it — on Codex
that matters because `config.toml` also holds the per-project trust store.
Each engine's payload and rendered verdict are genuinely different shapes,
not the same JSON dressed up three ways — Codex's `hookSpecificOutput` has no
`ask` or advisory field at all, and Cursor folds a reason and any advice into
one `user_message` string because it has exactly one text slot. And the
record is appended on every path through `route`, including a call that
belongs to another engine's config entirely (`route` and its `--registered-for`
flag disagree, so nothing runs and the record says `suppressed`) — the record
is what makes a silently misfiring or disabled guard recoverable after the
fact, so it can't depend on anything actually firing.

## Build and install

```
nix build .#default   # -> result/bin/hookyard
nix develop            # devShell with the Go toolchain and formatters
```

Once you have a binary on `PATH`:

```
hookyard validate --manifest path/to/hookyard.json
hookyard install  --manifest path/to/hookyard.json [--manifest ...]
hookyard doctor
```

`install` renders every manifest it is given into all three engines' native
config in one pass — it takes the full list, never one repo at a time,
because its strip is keyed on a marker that does not record which manifest
produced a row.

`doctor` answers the question the event record cannot: every engine skips
hooks entirely in a directory the user has not trusted, and a handler that
never runs cannot report that it didn't.

## The manifest

A manifest is a JSON file with a list of handlers. Each handler declares
which events it wants, on which engines, and — optionally — which normalized
tool names to filter on:

```json
{
  "handlers": [
    {
      "id": "block-secrets",
      "exec": "/home/you/bin/guard-secrets",
      "events": ["pre_tool"],
      "engines": ["claude-code", "codex", "cursor"],
      "match": ["Bash"],
      "timeout_ms": 2000
    }
  ]
}
```

- `exec` must be an absolute path to an executable file — it's resolved at
  hook-fire time against the agent's own working directory and `PATH`, not
  the installer's, so a relative path or a bare name would let whatever
  happens to sit there stand in.
- `events` are one of the six canonical events, or `engine:NativeName` for an
  event only one engine has.
- `engines` is any of `claude-code`, `codex`, `cursor`.
- `match` filters by normalized tool name; an empty list matches every tool.
  A tool with no equivalent on a claimed engine — Codex has no `Grep` or
  `Glob`, Cursor has no `Glob` — fails validation rather than installing a
  handler that would never fire there.
- `timeout_ms` is optional and capped at 4300ms (`manifest.MaxHandlerTimeoutMS`):
  the router's own 4.5s deadline minus the margin it needs to consolidate and
  return before the engine's own timeout.

This exact manifest, with `exec` pointed at a real executable, validates
clean:

```
$ hookyard validate --manifest hookyard.json
1 manifests, 1 handlers, no problems found
```

## The event record

Every call routed through `hookyard route` appends one JSON line to an
append-only stream, whether or not anything is subscribed to it and
regardless of what the verdict was. It lives at
`$HOOKYARD_STATE_DIR/stream/YYYY-MM-DD.jsonl` (falling back to
`$XDG_STATE_HOME/hookyard`, then `~/.local/state/hookyard`), one file per UTC
day, written at `0600`. Files older than 14 days are swept on the first write
of a new day. This is the thing that makes a silently disabled or misfiring
guard recoverable: `doctor` tells you whether hooks are wired up right now,
and the record tells you what actually happened over the last two weeks even
if they weren't.

## Vocabulary

hookyard translates between its own normalized names and each engine's
native ones; see [`internal/vocab`](internal/vocab) for the full tables.
The six canonical events are `session_start`, `prompt_submit`, `pre_tool`,
`post_tool`, `pre_compact` and `turn_end`. The normalized tool names are
`Read`, `Write`, `Bash`, `Grep` and `Glob`.

## More

The design, and the verification behind it, is in
[`docs/design/hookyard.md`](docs/design/hookyard.md). Real captured hook
payloads for all three engines are in
[`docs/design/fixtures/hook-payloads/`](docs/design/fixtures/hook-payloads/).
