# Cursor plugin-root probe (issue #45)

Evidence for whether `${CURSOR_PLUGIN_ROOT}` (or another fire-time path form)
reaches a Cursor plugin's own hook commands. See `docs/design/hookyard.md`
§12 and §3.1 for how this is used.

## `probe-plugin/`

A minimal local plugin (`.cursor-plugin/plugin.json` + `hooks/hooks.json`,
the manifest shape §3.1's per-engine table already documents), meant to be
loaded with `cursor-agent --plugin-dir <path to probe-plugin>` — no
marketplace registration needed, so nothing is written into the real
`~/.cursor` config. Its `beforeShellExecution` hook shells out to
`/bin/sh -c '...'` and appends `$0`, `$CURSOR_PLUGIN_ROOT`,
`$CLAUDE_PLUGIN_ROOT`, `pwd`, and the full environment to
`/tmp/hookyard-probe-out.txt`.

**This was not run to completion.** `cursor-agent` (2026.09.10-fd3934a, the
version installed on this host) requires `cursor-agent login` before it will
run any prompt at all, interactive or `--print`, even under a scratch `HOME`
— there is no anonymous/local-only mode to fire a hook without a live agent
turn. That login is an interactive OAuth-style flow; it was not run here to
avoid touching the account and because it cannot be scripted headlessly.

To get LOCAL-grade evidence (upgrading the code-reading finding below),
a human can run:

```
cursor-agent login   # interactive, once
cd docs/design/fixtures/cursor-plugin-root
rm -f /tmp/hookyard-probe-out.txt
cursor-agent --plugin-dir ./probe-plugin --trust --force -p \
  --output-format text --workspace /tmp "run: echo hi"
cat /tmp/hookyard-probe-out.txt
```

## `code-reading-snippet.txt`

What was actually used to answer #45: `cursor-agent`'s own shipped bundle
(Nix store path and byte offsets inside it are in the file) sets both
`CURSOR_PLUGIN_ROOT` and `CLAUDE_PLUGIN_ROOT` to the firing plugin's install
path, as a real exported env var, for any hook whose source is a plugin
(`pluginHooks`) — never for project/user/team/enterprise hooks. This is
code-reading, not LOCAL: the file is a proprietary minified bundle with no
public source repo to permalink into, so the citation is the installed
version string plus a Nix store path (content-addressed, stable) plus byte
offsets, extracted with the two Python one-liners below (kept here so the
next re-check does not have to re-derive the search):

```python
import re
data = open('<nix-store-path>/190.index.js').read()
for m in re.finditer('CURSOR_PLUGIN_ROOT', data):
    print(data[max(0, m.start()-300):m.end()+300])
```

Grep entry point, if the store path or chunk numbering changes on a future
`cursor-agent` version:

```
grep -o "[A-Za-z_]*PLUGIN_ROOT[A-Za-z_]*" <nix-store-path>/*.js | sort -u
```
