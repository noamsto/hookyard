# hookyard

Register agent hooks once, route them to every coding agent.

Claude Code, Codex, Cursor and Pi each declare hooks in their own config
format, under their own event names, with their own payload shape — Pi has
no config-level hook at all, and gets a generated bridge extension instead
(more below). hookyard is a standalone static binary that holds one
declarative table of which handler runs on which event on which engine,
renders that table into each engine's native config, and — when an engine
fires a hook — decodes the payload into a normalized shape, runs the
matching handlers concurrently under a shared deadline, folds their
verdicts with a deny-wins consolidation, and renders the result in the
shape the calling engine accepts. All four engines have a confirmed deny
path, so a guard enforces on all four; a decision only has somewhere to
land on `pre_tool` (plus a handful of Cursor-scoped events), so a verdict on
any other event is recorded but not enforced, and an `allow` rendered to
Codex or Pi is recorded rather than enforced too — Codex rejects an explicit
allow by name, and Pi's decision channel has no allow wire form at all, so
on both nothing is printed and the engine's own default flow runs instead.
An `ask` is not one of those cases: on Codex and Pi, whose decision shapes
are binary, a consolidated `ask` degrades to an enforced `deny` with a
reason explaining why.

## How it works

Two passes. `hookyard install` writes the table down into Codex's, Cursor's
and Pi's config and hookyard's own state table; `hookyard emit` prints
Claude Code's hooks block as JSON on stdout, for Nix to place — Claude
Code's own destination is Nix-managed on this machine and unwriteable by
`install`'s usual rename-based strip (more below). `hookyard route` is what
an engine actually invokes when a hook fires.

Several repos' manifests fold into one `hookyard install` pass, out to
Codex's, Cursor's and Pi's native config plus hookyard's own state table,
and into one `hookyard emit` pass Nix runs at build time for Claude Code:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/diagrams/registration-dark.svg">
  <img alt="Registration: repo A's and repo B's hookyard.json manifests fold into one hookyard install pass, which writes into Codex's config.toml, Cursor's hooks.json, and Pi's two artifacts — a generated bridge extension file and an extensions[] entry in Pi's own settings.json — and records the installed handlers in hookyard's state table; the same manifests also feed a hookyard emit pass that Nix places into Claude Code's --settings overlay." src="docs/diagrams/registration.svg">
</picture>

An engine firing a hook decodes its native payload into one normalized
envelope, fans out to the matching handlers under a shared deadline, folds
their verdicts deny-wins, and renders the result for the calling engine
before the record is appended:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/diagrams/routing-dark.svg">
  <img alt="Routing: Claude Code, Codex, Cursor, and Pi each decode their own native hook payload into a normalized envelope; the matching handlers run concurrently under one 4.5s deadline; their verdicts fold deny-wins; the result renders for the calling engine, including Pi's binary block-or-nothing verdict; and the event record is appended on every path." src="docs/diagrams/routing.svg">
</picture>

A few things worth calling out because they're not visible from the
`hookyard` label alone: each engine's writer strips only its own
marker-tagged entries out of a config file it shares with other writers, and
refuses to touch a file it cannot parse rather than clobbering it — on Codex
that matters because `config.toml` also holds the per-project trust store.
Each engine's payload and rendered verdict are genuinely different shapes,
not the same JSON dressed up four ways — Codex's `hookSpecificOutput` has no
`ask` or advisory field at all, Cursor folds a reason and any advice into one
`user_message` string because it has exactly one text slot, and Pi has no
subprocess payload to dress up in the first place: hookyard's own bridge
extension authors what looks like one, and renders its verdict as a bare
`{block, reason}` return value rather than any wire format Pi defines. And the
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
hookyard emit     --engine claude-code --manifest path/to/hookyard.json [--manifest ...] --router-path <path> --state-dir <path> [--base <file>]
hookyard doctor
```

`install` renders every manifest it is given into Codex's, Cursor's and
Pi's native config in one pass — it takes the full list, never one repo at a
time, because its strip is keyed on a marker that does not record which
manifest produced a row. `emit` covers Claude Code instead: it is
Claude-Code-only (`--engine claude-code` is required), prints the merged
`hooks` block to stdout rather than writing a file, and is meant to run
inside a Nix build rather than at activation time — Nix is what places its
output into the `--settings` overlay.

`doctor` answers the question the event record cannot: every engine skips
hooks entirely in a directory the user has not trusted, and a handler that
never runs cannot report that it didn't.

## Wiring hookyard into a consumer repo

A consumer takes hookyard as a flake input and imports its home-manager
module:

```nix
{
  inputs.hookyard.url = "github:noamsto/hookyard";

  # inside the home-manager configuration:
  imports = [ inputs.hookyard.homeModules.default ];
  programs.hookyard = {
    enable = true;
    manifests = [ ./hookyard.json ];
  };
}
```

`manifests` is the whole of a consumer's contribution, and it is a shared
list: every module that sets it contributes paths to the same list, rendered
into Codex's, Cursor's and Pi's native config by one `hookyard install`
invocation, owned by hookyard's own module and run from
`home.activation.hookyardInstall`. The same list also feeds
`programs.hookyard.claudeOverlay.merged`, a build-time `hookyard emit`
derivation the consumer wires into Claude Code's `--settings` overlay itself
— Claude Code's destination is Nix-managed, not something hookyard's own
activation entry can reach (more below). A consumer never pins its own
hookyard input and never adds its own activation entry that calls `hookyard
install` directly: the strip that removes hookyard's rows on re-render is
keyed on a marker that does not record which manifest produced a row, so a
second invocation would silently delete the first one's rows rather than
merge with them. One input, one binary, one rendering pass per destination,
and consumers contribute data to it rather than a second copy of the
mechanism.

A manifest's `exec` must be an absolute path that already exists at
activation time — `install` stats it, and a manifest that fails the stat
aborts the whole home-manager generation switch. That is deliberate: the
alternative is registering a handler that silently never runs, which is the
same fail-open the guard exists to prevent, just moved from hook-fire time to
install time instead of caught at all. The practical consequence is that a
handler built by the same flake needs a manifest generated with
`pkgs.writeText`, embedding the handler's own store path, rather than a
checked-in JSON file naming a path Nix had no chance to fill in.

Turning hookyard off goes in a specific order for Codex, Cursor and Pi: empty
`manifests`, activate, *then* set `enable = false`. Flipping `enable` off
first removes the binary from the profile while those three engines' configs
still name it, so the path each config points at now fails at `exec` instead
of resolving — exactly the fail-open §9 of the design doc spends its argument
on. Emptying `manifests` first runs `install` with nothing registered, which
strips hookyard's rows from all three configs while the binary is still there
to do it; only then is it safe to drop the package itself. `hookyard doctor`'s
`router path` check is what catches a machine left in the wrong order — it
confirms the path each engine's config names is actually there to exec,
alongside the trust and confirmed-deny checks it already runs. This ordering
rule does not apply to Claude Code: it has no rows in a hookyard-owned file
to strip, since `emit` never writes one, so emptying `manifests` just
regenerates the overlay without hookyard's block and `enable = false` is safe
in any order.

The destination files hookyard writes into — Codex's `config.toml`, Cursor's
`hooks.json`, and Pi's `settings.json` — must be plain files that hookyard
itself owns, not symlinks placed by another Nix module. Pi also gets a
generated bridge extension file, written wholesale rather than merged into,
and that gets the same symlink refusal. `install` refuses to render into any
of these rather than replace it, because replacing it would silently detach
whatever manages the link with no warning at the next switch. The same
escape hatch exists for these three engines as `codexConfig`, `cursorHooks`,
and `piSettings`, each pointed at a file hookyard can own instead of the
default path.

Claude Code has no such option, and needs none: `programs.hookyard.claudeHooks`
is the package holding hookyard's own emitted block, and
`programs.hookyard.claudeOverlay.merged` is that block merged into an
optional `claudeOverlay.base` the consumer already places as the `--settings`
overlay — both read-only outputs of `emit`, not files `install` writes, so
there is no destination for a symlink to collide with. On this machine
`~/.claude/settings.json`
is itself a home-manager-managed symlink, which is exactly why Claude Code's
registration goes through the overlay rather than through a destination
option at all (#29).

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
      "lane": "verdict",
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
- `lane` is `"verdict"` (the default, safe to omit) or `"fire_and_forget"`,
  for a handler with no verdict to give (design doc §4). A fire-and-forget
  handler can never guard, so `validate` refuses one declared on a decision
  event or carrying a non-zero `timeout_ms`. Using `lane` at all needs the
  hookyard version that introduced it — an older binary silently drops the
  field and runs the entry in the verdict lane instead.
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
payloads for all four engines are in
[`docs/design/fixtures/hook-payloads/`](docs/design/fixtures/hook-payloads/) —
Pi's are hookyard's own bridge output rather than a native payload, and the
fixtures' own README says so.
