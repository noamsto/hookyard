# Roadmap

Where hookyard is going, in the order it gets built. Each stage is useful on
its own, and none needs a server hookyard's author runs.

**What hookyard is for.** One local router that sees every hook call from
every engine on the machine, folds the handlers' verdicts deny-wins, and keeps
a record of what actually happened (`hookyard serve`), with `hookyard doctor`
to tell you whether the guards are live. "Write a hook once for every engine"
is how you use it; the guard layer and the record are why.

## 1. Runs without Nix

Yard mode is one static binary, and Nix is one way to install it, not a
requirement.

- [x] `install --claude-settings` writes Claude Code's catalog on hosts with
  no Nix overlay; `doctor` tells that registration apart from a stale one
  ([without Nix](yard-mode.md#without-nix)).
- [x] Release binaries for linux and darwin (amd64, arm64), cut on a `v*` tag.
- [ ] First tagged release, plus a Homebrew tap.
- [ ] A `version` subcommand, so `doctor` and bug reports can name the build.

## 2. Install and update hooks by reference

- [ ] `hookyard add <git-url>//<path>@<commit>`: fetch a manifest and its
  handlers, validate them, record the commit and a content hash, then run
  `install`.
- [ ] `hookyard update [--check]`: compare pinned hooks against their source.
  `--check` only reports. Applying an update shows the diff (code, events,
  engines) and waits for confirmation.
- [ ] A `session_start` handler hookyard installs for itself. It reads a cached
  `update --check` result and never touches the network while a hook is
  running. When updates exist, it tells the agent through the advisory channel
  ("2 hooks have updates; run `hookyard update`"). It never applies them:
  a hook that silently rewrites other hooks is the supply-chain path a guard
  layer exists to close, and it would defeat Codex's re-trust on hash change.

## 3. Team mode, served from git

For a company, the useful parts are one guard policy on every laptop and
every engine, and one audit trail. Both can be done without a hosted service.

- [ ] `install --from <git-url>@<tag>`: an org repo of manifests is the policy
  source. It is pinned, can be signature-checked, and is written into each
  engine's admin-managed scope where one exists.
- [ ] `hookyard export`: send the event record to OpenTelemetry, with redaction
  rules applied first, so audit data lands in the company's own stack. The
  record holds prompts, commands and paths, and it should not leave the
  machine unredacted.
- [ ] `doctor --json`, so device management or CI can assert "guard X is
  installed and live on this machine."
- [ ] Later, only if people ask for it: a self-hostable `serve --team` that
  reads records from many machines.

Whatever gets hosted, the router stays local. Every hook call runs under a
4.5s deadline and has to work offline, so no server is ever in that path.

## 4. A hook index, not a store

Deferred until build mode ([design §3.1](design/hookyard.md)) produces native
plugins for every engine:

- [ ] A `hookyard-hooks` repo of vetted, example hooks: secret blocking,
  destructive-command guards, lint on stop. They double as tests and
  documentation.
- [ ] CI runs `hookyard build` on each entry and publishes the result into the
  plugin marketplaces the engines already have. Users install hooks the way
  they install every other plugin.
- [ ] A static site renders the index read-only: what each hook does, which
  events and engines it takes, and which commit it was built from.
- [ ] Running hooks that other projects publish under hookyard's router, so it
  adds deny-wins folding and the record to hooks it did not write.

Submissions go through PR review, with pinned commits and each hook's events
and engines visible. A hook runs arbitrary code with a veto over every tool
call, so the index has to be curated rather than open-upload.

## 5. Memory, as a handler rather than part of the core

The cross-engine memory layer ([memory-layer.md](design/memory-layer.md)) is a
consumer of hookyard's advisory contract. It is not a feature of the router.
It ships as its own binary with its own manifest once its open trust and
secrets decisions are made. §11 of that document covers what that means for
hookyard.
