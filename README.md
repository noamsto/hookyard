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
consolidates their verdicts — is not implemented yet; `hookyard route` currently
allows the call and says so.

The design, and the verification behind it, is in
[`docs/design/hookyard.md`](docs/design/hookyard.md). Real captured hook
payloads for all three engines are in
[`docs/design/fixtures/hook-payloads/`](docs/design/fixtures/hook-payloads/).
