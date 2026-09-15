# hookyard

**Write an agent hook once. Run it in Claude Code, Codex, Cursor and Pi.**

Every coding agent has its own hook config, its own event names and its own
payload shape. hookyard is one static binary that sits between them and your
handlers: declare each handler once in a manifest, and hookyard registers it
with every engine, translates each hook call into one normalized envelope, and
answers each engine in the format it expects.

- **One manifest, four engines.** No per-agent copies of the same guard.
- **Deny wins.** Handlers run concurrently and their verdicts fold
  `deny > ask > allow`; a handler with nothing to say never overrides one that
  objects.
- **Bounded.** Every call shares one 4.5s deadline, so a slow or broken handler
  abstains instead of hanging the agent.
- **On the record.** Every routed call is appended to a local event log, so a
  guard that quietly stopped firing shows up after the fact.
- **`hookyard doctor`** checks trust, registration and router paths per engine.

## Status

**Yard mode** ships today: machine-wide install, routing and the event record,
aimed at Nix users and at guards that need machine-wide enforcement.
**Build mode**, generating per-engine plugin hooks so end users never see
hookyard, is the planned open-source default and not yet implemented
([design §3.1](docs/design/hookyard.md)).

## How it works

Manifests from any number of repos fold into one `hookyard install` pass, out
to Codex's, Cursor's and Pi's native config plus hookyard's own state table.
`hookyard emit` is a separate pass Nix runs at build time for Claude Code: it
reads no manifests, and instead renders one route row per event in a fixed,
eight-event catalog — it's the state table, the same one `install` writes,
that decides which handlers actually run for Claude Code, same as for the
other three engines:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/diagrams/registration-dark.svg">
  <img alt="Registration: repo A's and repo B's hookyard.json manifests fold into one hookyard install pass, which writes into Codex's config.toml, Cursor's hooks.json, and Pi's two artifacts — a generated bridge extension file and an extensions[] entry in Pi's own settings.json — and records the installed handlers in hookyard's state table; a separate hookyard emit pass, reading no manifests, renders a fixed Claude event catalog that Nix places into Claude Code's --settings overlay." src="docs/diagrams/registration.svg">
</picture>

When a hook fires, `hookyard route` normalizes the payload, runs the matching
handlers, folds their verdicts and renders the answer for the calling engine:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/diagrams/routing-dark.svg">
  <img alt="Routing: Claude Code, Codex, Cursor, and Pi each decode their own native hook payload into a normalized envelope; the matching handlers run concurrently under one 4.5s deadline; their verdicts fold deny-wins; the result renders for the calling engine, including Pi's binary block-or-nothing verdict; and the event record is appended on every path." src="docs/diagrams/routing.svg">
</picture>

## Quick start

```
nix build .#default   # -> result/bin/hookyard
nix develop            # devShell with the Go toolchain and formatters
```

Write a manifest:

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

A handler reads hookyard's envelope on stdin, with the engine's own payload
under `.native`, and, to object, prints a `hookSpecificOutput` with a
`permissionDecision`. Then:

```
hookyard validate --manifest hookyard.json
hookyard install  --manifest hookyard.json [--manifest ...]
hookyard emit     --engine claude-code --router-path <path> --state-dir <path> [--base <file>]
hookyard doctor
```

## With Nix

Take hookyard as a flake input and contribute manifests to its home-manager
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

Every module adds to the same `manifests` list, and hookyard's own module runs
the single `install` pass. Wire `programs.hookyard.claudeOverlay.merged` into
Claude Code's `--settings` overlay — it's a build-time `hookyard emit`
derivation, not something `manifests` reaches. Before relying on it, read
[yard mode in depth](docs/yard-mode.md#consumer-repos): it covers why a
consumer must never run its own `install`, the order for turning hookyard off,
and which destination files it refuses to write.

## The manifest

Each handler names its `events`, `engines`, an optional tool `match` and a
`timeout_ms` (capped at 4300ms). Handlers that return no verdict can run in the
`fire_and_forget` lane. The full field reference and the normalized event and
tool names are in [yard mode in depth](docs/yard-mode.md#manifest-fields).

## The event record

Every call through `hookyard route` appends one JSON line to
`~/.local/state/hookyard/stream/`, whatever the verdict, and keeps 14 days.
`doctor` tells you whether hooks are wired up right now; the record tells you
what actually happened.

## More

- [Yard mode in depth](docs/yard-mode.md): per-engine verdict rendering,
  consumer wiring, manifest reference.
- [Design doc](docs/design/hookyard.md): the design and the verification
  behind it.
- [Captured hook payloads](docs/design/fixtures/hook-payloads/) for all four
  engines.
