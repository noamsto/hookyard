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

No implementation yet. The design is specified in
[`docs/design/hookyard.md`](docs/design/hookyard.md).
