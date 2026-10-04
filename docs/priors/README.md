# priors v0

`priors` is the memory layer's command-line tool
([design](../design/memory-layer.md), workstream 1 of §10). It keeps
agent-learned facts as plain markdown, one fact per file, in two git
checkouts — a personal store and a work store — and serves them back to
Claude Code, Codex and Pi at `session_start`, and to any engine by search.

It lives in this repo as `cmd/priors`, but it is not router code: it is an
`exec` handler that speaks hookyard's envelope as JSON and imports no
`internal/` package (§5, decision 4).

## Configure

`$PRIORS_CONFIG`, else `$XDG_CONFIG_HOME/priors/config.toml`:

```toml
profile        = "work"            # the host kind: "work" or "personal"
personal_store = "~/memory/personal"
work_store     = "~/memory/work"   # read only on a work-profile host
work_orgs      = ["github.com/your-work-org"]   # host/owner
personal_orgs  = ["github.com/you"]
# optional:
# work_names   = ["build.corp.internal"]  # also rejected in the personal store
# state_dir    = ""     # default $XDG_STATE_HOME/priors
# event_record = ""     # hookyard's state dir; default follows hookyard's own
# rules        = ""     # redaction rule set; "" = the built-in one
# scanner      = ""     # default betterleaks, then gitleaks, on PATH
# ssh_config   = ""     # passed to `ssh -G -F` when resolving host aliases
# commit       = true   # commit published facts into the checkout
# push         = false  # needs an upstream set once (git push -u); after a failed push, git push by hand to resume
```

`work_orgs` is required on every host. `personal_store`, `work_store` and
`state_dir` must be absolute once `~/` is expanded, and none may be the same
as, or nested inside, another (the default state dir included). Symlinks in
them are resolved first, so a store behind a symlink behaves as the directory
it names, and the nesting check sees the real paths.

A session's store is picked from its repo's `origin` org (§4.2): a work-org
repo reads both stores and writes work; any other repo reads the personal
store only.

## Commands

```
priors add --name N --description D --type project|reference|feedback|user \
           [--scope repo|global] [--repo R ...] [--session ID] [--external] --stdin
priors list [--repo R] [--type T] [--store S] [--stale] [--flagged] [--all]
priors show <name>
priors search <terms...> [--repo R] [--any-repo] [--type T] [--scope S] [--all]
priors lint [--store S] [--move-flagged] | priors lint --dir D --kind K [--work-org O ...]
priors index [--text]        # session_start output (envelope on stdin)
priors index --write         # regenerate every MEMORY.md
```

`add` refuses a fact that matches a secret pattern or the scanner, or fails
the lint, and exits 1. Otherwise it prints one line and exits 0:

- `published <store> <path>`: the fact passed every gate and was committed
  into the store's checkout.
- `flagged <store> <path>: <reasons>`: a gate flagged it, so it waits,
  `proposed`, in the unsynced host-local layer
  (`$XDG_STATE_HOME/priors/local/<store>/`). It is injected only into
  sessions of the repo it was learned in, on this host.
- `quarantined <path>: <why>`: §4.2's write rule sent it to the host-local
  quarantine (for example a work repo on a personal host).

The flagging gates are provenance (the session fetched web or MCP content,
ingested an issue, PR or URL through a shell, or its tool calls are not in
hookyard's event record or the shell watcher never saw it), content (URLs,
`curl | sh`, hook bypasses, shell commands, "always/never" aimed at tools,
override phrasing) and a size cap of 8 KiB. Gate 2 learns the session from
the engine when the engine provides it (inside Claude Code, `CLAUDECODE=1`
with `CLAUDE_CODE_SESSION_ID`), and that id and engine always win; a
`--engine`, `--session`, `PRIORS_ENGINE` or `PRIORS_SESSION` that disagrees
with them does not replace them, and flags the fact
`provenance:asserted-identity`. Elsewhere the session is `--session`, else
`PRIORS_SESSION`; with no session a fact is flagged. This binds the id to the
engine's environment, not to the agent: an agent can still set those
variables in its own command; only Claude's id is verified, so elsewhere
`priors add` trusts the id it is given.

Web and MCP use is read from tool names in hookyard's event record. Shell
ingestion is not: the record carries no command text, so priors inspects each
shell command, on `pre_tool` before it runs and on `post_tool` after, and,
when it looks like it reads an issue, PR or comment (`gh issue view`, `gh
api`) or fetches a URL (`curl`, any `https://` in the command), creates an
empty per-session marker under
`provenance/` in priors' state dir. That flags the session
`provenance:shell`; no command text is stored. Any ingestion flags, owner's
or not. A session the record covers but the watcher never saw, or whose
markers cannot be read, flags `provenance:no-ingest-record`. The command is
parsed as bash and every simple command in it is judged on its expanded
words (quotes, `$'…'`, brace expansion and `${X:-…}` defaults resolved);
every word is also re-read as a script, so a script quoted, glued
to `--opt=` or nested in `sh -c` is judged too. A command that does not parse
falls back to a token scan, and one nested past the bound (depth 8, 1 MiB)
flags. It misses what the command text does not show (`git fetch` or `git
pull` content without a URL; aliases, shell functions, scripts on disk; an
unlisted fetcher with no URL literal), a command word built at run time
(`$CMD`, `$(printf g)h`, `eval "$X"`, `/usr/bin/g[h]`), a script another program
decodes before a shell runs it (`base64 -d | sh`, `rev`, `xxd -r`), an
unquoted `rg --pre curl …` (inert search commands such as `grep` and `rg`
hide a listed name in their arguments, and `--pre` runs a preprocessor), a
lost ingest write in an already-seen session, and, on Codex, any `post_tool`
shape mismatch (only `pre_tool` is fixture-backed). It over-flags any URL
anywhere, a listed name in a quoted string, a listed name as an argument of
any non-inert command (`git log --grep curl`, `man curl`), `echo gh issue
view`, an inert command after any wrapper flag (`xargs -0 grep curl`,
`sudo -u bob grep curl`: the flag may take it as its value), a path argument
whose last element is a listed name (`go test ./internal/http`), `gh` / `glab`
with no group anywhere but as the first word of its command (`xargs gh`,
`sudo gh`: its arguments may come from stdin or a placeholder), a listed name
later in a `gh` / `glab` write or local segment (`gh pr comment 1 --body
curl`), a listed name glued to `--opt=`, `KEY=` or a fused short option
(`--title=curl`, `rsync -avxh`, whose `xh` is a fetcher), a command the
parser cannot read, a printf / echo escape inside a quoted message (`git
commit -m 'gh auth status\nand gh issue view'`), `gh -R o/r pr create` and a
shell call denied or rejected after `pre_tool` (a guard's deny, a declined
permission prompt): the marker is written before the decision.

`list`, `show` and `search` fence what they print as reference data, with a
delimiter drawn per call.

## Wire it into hookyard

[`cmd/priors/hookyard.json`](../../cmd/priors/hookyard.json) is an example
manifest; point every `exec` path at the installed binary (`nix build
.#priors`). It registers three handlers:

- `priors-index` on `session_start`, which injects the tier-1 index. It
  always exits 0 and prints nothing when anything is missing or slow
  (800 ms), so a broken store leaves the session as it was.
- `priors-record` on `post_tool` with no `match`, on the fire-and-forget
  lane. It still answers nothing. It makes every tool call reach hookyard's
  event record, which gate 2 reads, and writes gate 2's shell markers.
- `priors-provenance` on `pre_tool`, matching `Bash`, on the verdict lane
  with a 1 s budget. It answers nothing, so it never blocks a call; it writes
  the shell markers before the command runs, which covers a call that fails
  (and so never reaches `post_tool`) and one that runs `priors add` itself.

## Store repositories

Each store is its own private git repo. Run `priors lint` there as a
required check: [`store-ci.example.yml`](store-ci.example.yml) is a
starting point.
