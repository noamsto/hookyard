# hookyard

Register agent hooks once, route them to every coding agent.

Claude Code, Codex and Cursor each declare hooks in their own config format,
under their own event names, with their own payload shape. hookyard is a
standalone static binary that holds one declarative table of which handler
runs on which event on which engine, renders that table into each engine's
native config, and — when an engine fires a hook — translates the payload,
runs the matching handlers concurrently, and consolidates their verdicts into
one allow/deny/ask decision, rendered in the shape that engine accepts. All
three engines have a confirmed deny path, so a guard enforces on all three;
where a consolidated verdict has no slot in the target engine — an `ask` sent
to Codex, which takes only `deny` — it is recorded rather than enforced.

## What works today

Registration, and the checks that tell you whether it took effect:

```
hookyard validate --manifest path/to/hookyard.json
hookyard install  --manifest path/to/hookyard.json [--manifest ...]
hookyard doctor
```

`install` renders every manifest it is given into all three engines' native
config in one pass — it takes the full list, never one repo at a time, because
its strip is keyed on a marker that does not record which manifest produced a
row. Each writer coexists with whatever else writes that file: it strips only
its own entries, refuses a file it cannot parse rather than clobbering it, and
lands its result through a single rename. On Codex that discipline is
load-bearing rather than tidy, because `config.toml` also holds the per-project
and per-hook trust stores.

`doctor` answers the question the event record cannot: every engine skips hooks
entirely in a directory the user has not trusted, and a handler that never runs
cannot report that it didn't.

The router — the event path that translates a payload, fans out to handlers and
consolidates their verdicts — is implemented: `hookyard route` decodes an
engine's payload into one envelope, runs every handler the table matches
concurrently, and consolidates their verdicts deny-wins into a single
allow/deny/ask decision, rendered back in the shape the calling engine accepts.

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
into all three engines' native config by one `hookyard install` invocation,
owned by hookyard's own module and run from `home.activation.hookyardInstall`.
A consumer never pins its own hookyard input and never adds its own
activation entry that calls `hookyard install` directly: the strip that
removes hookyard's rows on re-render is keyed on a marker that does not
record which manifest produced a row, so a second invocation would silently
delete the first one's rows rather than merge with them. One input, one
binary, one rendering pass, and consumers contribute data to it rather than a
second copy of the mechanism.

A manifest's `exec` must be an absolute path that already exists at
activation time — `install` stats it, and a manifest that fails the stat
aborts the whole home-manager generation switch. That is deliberate: the
alternative is registering a handler that silently never runs, which is the
same fail-open the guard exists to prevent, just moved from hook-fire time to
install time instead of caught at all. The practical consequence is that a
handler built by the same flake needs a manifest generated with
`pkgs.writeText`, embedding the handler's own store path, rather than a
checked-in JSON file naming a path Nix had no chance to fill in.

Turning hookyard off goes in a specific order: empty `manifests`, activate,
*then* set `enable = false`. Flipping `enable` off first removes the binary
from the profile while the three engines' configs still name it, so the path
each config points at now fails at `exec` instead of resolving — exactly the
fail-open §9 of the design doc spends its argument on. Emptying
`manifests` first runs `install` with nothing registered, which strips
hookyard's rows from all three configs while the binary is still there to do
it; only then is it safe to drop the package itself. `hookyard doctor`'s
`router path` check is what catches a machine left in the wrong order — it
confirms the path each engine's config names is actually there to exec,
alongside the trust and confirmed-deny checks it already runs.

The three destination files — Claude Code's `settings.json`, Codex's
`config.toml`, Cursor's `hooks.json` — must be plain files that hookyard
itself owns, not symlinks placed by another Nix module. `install` refuses to
render into one rather than replace it, because replacing it would silently
detach whatever manages the link with no warning at the next switch. On this
machine `~/.claude/settings.json` is itself a home-manager-managed symlink
today, so registering Claude Code hooks needs one of the two ways out:
`programs.hookyard.claudeSettings` pointed at a file hookyard can own, or the
module that currently manages that symlink stepping aside for hookyard. The
same option exists for the other two engines as `codexConfig` and
`cursorHooks`.

The design, and the verification behind it, is in
[`docs/design/hookyard.md`](docs/design/hookyard.md). Real captured hook
payloads for all three engines are in
[`docs/design/fixtures/hook-payloads/`](docs/design/fixtures/hook-payloads/).
