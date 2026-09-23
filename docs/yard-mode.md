# Yard mode in depth

The [README](../README.md) covers what hookyard is and how to start. This page
holds the detail: how verdicts land on each engine, how consumers share one
install, and the manifest reference.

## Verdicts per engine

Claude Code, Codex, Cursor and Pi each declare hooks in their own config
format, under their own event names, with their own payload shape — Pi has no
config-level hook at all, and gets a generated bridge extension instead.
hookyard holds one declarative table of which handler runs on which event on
which engine, renders that table into each engine's native config, and — when
an engine fires a hook — decodes the payload into a normalized shape, runs the
matching handlers concurrently under a shared deadline, folds their verdicts
with a deny-wins consolidation, and renders the result in the shape the calling
engine accepts.

All four engines have a confirmed deny path, so a guard enforces on all four; a
decision only has somewhere to land on `pre_tool` (plus a handful of
Cursor-scoped events), so a verdict on any other event is recorded but not
enforced, and an `allow` rendered to Codex or Pi is recorded rather than
enforced too — hookyard prints no allow to Codex (an older Codex binary was read
as rejecting one; current Codex docs describe `allow` with `updatedInput`), and
Pi's decision channel has no allow wire form at all, so on both nothing is
printed and the engine's own default flow runs instead. An `ask` is not one of
those cases: on Codex and Pi, whose decision shapes are binary, a consolidated
`ask` degrades to an enforced `deny` with a reason explaining why.

A few things worth calling out because they're not visible from the `hookyard`
label alone: each engine's writer strips only its own marker-tagged entries out
of a config file it shares with other writers, and refuses to touch a file it
cannot parse rather than clobbering it — on Codex that matters because
`config.toml` also holds the per-project trust store. Each engine's payload and
rendered verdict are genuinely different shapes, not the same JSON dressed up
four ways — Codex's `hookSpecificOutput` has no `ask` or advisory field at all,
Cursor folds a reason and any advice into one `user_message` string because it
has exactly one text slot, and Pi has no subprocess payload to dress up in the
first place: hookyard's own bridge extension authors what looks like one, and
renders its verdict as a bare `{block, reason}` return value rather than any
wire format Pi defines. And the record is appended on every path through
`route`, including a call that belongs to another engine's config entirely
(`route` and its `--registered-for` flag disagree, so nothing runs and the
record says `suppressed`) — the record is what makes a silently misfiring or
disabled guard recoverable after the fact, so it can't depend on anything
actually firing.

## The commands

`install` renders every manifest it is given into Codex's, Cursor's and Pi's
native config in one pass — it takes the full list, never one repo at a time,
because its strip is keyed on a marker that does not record which manifest
produced a row. `emit` covers Claude Code instead: it is Claude-Code-only
(`--engine claude-code` is required), takes no manifest at all, and prints a
`hooks` block with one route row per event in the fixed Claude Code event
catalog — `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`,
`PreCompact`, `Stop`, `claude-code:Notification` and `claude-code:SessionEnd`
— to stdout rather than writing a file. It's meant to run inside a Nix build
rather than at activation time — Nix is what places its output into the
`--settings` overlay, and it's the state table `install` writes that decides
which handlers actually run on each of those events.

`doctor` answers the question the event record cannot: every engine skips hooks
entirely in a directory the user has not trusted, and a handler that never runs
cannot report that it didn't.

## Consumer repos

`manifests` is the whole of a consumer's contribution, and it is a shared list:
every module that sets it contributes paths to the same list, rendered into
Codex's, Cursor's and Pi's native config, and into hookyard's own state table,
by one `hookyard install` invocation, owned by hookyard's own module and run
from `home.activation.hookyardInstall`. That state table is also what decides
which handlers run for Claude Code — `manifests` itself never reaches
`programs.hookyard.claudeOverlay.merged`, a build-time `hookyard emit`
derivation that varies only with hookyard's version, the router path, the
state dir and `claudeOverlay.base`. The consumer wires `merged` into Claude
Code's `--settings` overlay itself — Claude Code's destination is Nix-managed,
not something hookyard's own activation entry can reach. A consumer never
pins its own hookyard input and never adds its own activation entry that
calls `hookyard install` directly: the strip that removes hookyard's rows on
re-render is keyed on a marker that does not record which manifest produced
a row, so a second invocation would silently delete the first one's rows
rather than merge with them. One input, one binary, one rendering pass per
destination, and consumers contribute data to it rather than a second copy of
the mechanism.

A manifest's `exec` must be an absolute path that already exists at activation
time — `install` stats it, and a manifest that fails the stat aborts the whole
home-manager generation switch. That is deliberate: the alternative is
registering a handler that silently never runs, which is the same fail-open the
guard exists to prevent, just moved from hook-fire time to install time instead
of caught at all. The practical consequence is that a handler built by the same
flake needs a manifest generated with `pkgs.writeText`, embedding the handler's
own store path, rather than a checked-in JSON file naming a path Nix had no
chance to fill in.

### Turning hookyard off

Turning hookyard off goes in a specific order for Codex, Cursor and Pi: empty
`manifests`, activate, *then* set `enable = false`. Flipping `enable` off first
drops hookyard from `home.packages`, and once that generation's own build is
garbage-collected the router symlink each engine's config names — the
stateDir link `install` maintains, see the design doc's §9.1 — is left
pointing at a target that no longer exists. Those configs still name the
symlink itself, so the failure lands one step later than it used to: not at
the next activation, but whenever the old generation's store path is swept.
Nothing removes the link when hookyard is disabled, so a host that is
disabled and never installs again is left with a permanently dangling
symlink and no automatic cleanup. Emptying `manifests` first runs `install`
with nothing registered, which strips hookyard's rows from all three configs
while the binary is still there to do it; only then is it safe to drop the
package itself. `hookyard doctor`'s `router path` check is what catches a
machine left in the wrong order — it confirms the path each engine's config
names is actually there to exec, now distinguishing a dangling symlink from
one that's simply missing, alongside the trust and confirmed-deny checks it
already runs. This ordering rule does not apply to Claude Code: `emit` never
reads `manifests` in the first place, so emptying the list leaves the overlay
untouched, and `enable = false` yields `claudeOverlay.base` verbatim — or
`{\n}\n` when there is no base — without building hookyard at all, so it's
safe in any order.

### Destination files

The destination files hookyard writes into — Codex's `config.toml`, Cursor's
`hooks.json`, and Pi's `settings.json` — must be plain files that hookyard
itself owns, not symlinks placed by another Nix module. Pi also gets a
generated bridge extension file, written wholesale rather than merged into, and
that gets the same symlink refusal. `install` refuses to render into any of
these rather than replace it, because replacing it would silently detach
whatever manages the link with no warning at the next switch. The same escape
hatch exists for these three engines as `codexConfig`, `cursorHooks`, and
`piSettings`, each pointed at a file hookyard can own instead of the default
path. `piSettings` differs from the other two in being list-valued rather than
single-valued: it names one or more settings.json files, and hookyard installs
one bridge per entry.

Claude Code has no such option, and needs none: `programs.hookyard.claudeHooks`
is the package holding hookyard's own emitted block — the fixed event catalog,
not anything derived from `manifests` — and
`programs.hookyard.claudeOverlay.merged` is that block merged into an optional
`claudeOverlay.base` the consumer already places as the `--settings` overlay —
both read-only outputs of `emit`, not files `install` writes, so there is no
destination for a symlink to collide with. On this machine
`~/.claude/settings.json` is itself a home-manager-managed symlink, which is
exactly why Claude Code's registration goes through the overlay rather than
through a destination option at all (#29).

## Manifest fields

- `exec` must be an absolute path to an executable file — it's resolved at
  hook-fire time against the agent's own working directory and `PATH`, not the
  installer's, so a relative path or a bare name would let whatever happens to
  sit there stand in.
- `events` are one of the six canonical events, or `engine:NativeName` for an
  event only one engine has. For `claude-code:NativeName`, `NativeName` must
  be one of the eight events in Claude Code's catalog — `SessionStart`,
  `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PreCompact`, `Stop`,
  `Notification`, `SessionEnd` — the same eight `emit` renders; anything else
  fails validation with a message naming the catalog, whatever the handler's
  `engines` say.
- `engines` is any of `claude-code`, `codex`, `cursor`, `pi`.
- `lane` is `"verdict"` (the default, safe to omit) or `"fire_and_forget"`, for
  a handler with no verdict to give (design doc §4). A fire-and-forget handler
  can never guard, so `validate` refuses one declared on a decision event or
  carrying a non-zero `timeout_ms`. Using `lane` at all needs the hookyard
  version that introduced it — an older binary silently drops the field and
  runs the entry in the verdict lane instead.
- `match` filters by normalized tool name; an empty list matches every tool. A
  tool with no equivalent on a claimed engine — Codex has no `Grep` or `Glob`,
  Cursor has no `Glob` — fails validation rather than installing a handler that
  would never fire there.
- `timeout_ms` is optional and capped at 4300ms (`manifest.MaxHandlerTimeoutMS`):
  the router's own 4.5s deadline minus the margin it needs to consolidate and
  return before the engine's own timeout.

## Vocabulary

hookyard translates between its own normalized names and each engine's native
ones; see [`internal/vocab`](../internal/vocab) for the full tables. The six
canonical events are `session_start`, `prompt_submit`, `pre_tool`, `post_tool`,
`pre_compact` and `turn_end`. The normalized tool names are `Read`, `Write`,
`Bash`, `Grep` and `Glob`.

An event one engine has and the others do not is subscribed to as
`engine:NativeName`. The lifecycle signals a dashboard reads are these
engine-scoped events, because no canonical session-end exists:

- `pi:agent_settled` — pi's terminal "run settled, idle on the human" signal
  (fires after retries, compaction retries and queued follow-ups, and on an
  aborted run). `pi:session_shutdown` is the session-teardown event beside it.
- `codex:SessionEnd` — codex's session-end hook. Its payload carries no
  `turn_id`, so it routes only in yard mode, where the entry's own
  `--registered-for` supplies the engine.
- `cursor:sessionEnd` — cursor's session-end hook.

Codex and Cursor accept any `engine:NativeName` scoped to their own engine;
Pi and Claude Code additionally validate the native half against a fixed
catalog.

## Event record details

The stream lives at `$HOOKYARD_STATE_DIR/stream/YYYY-MM-DD.jsonl` (falling back
to `$XDG_STATE_HOME/hookyard`, then `~/.local/state/hookyard`), one file per
UTC day, written at `0600`. Files older than 14 days are swept on the first
write of a new day.
