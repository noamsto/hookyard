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

### Trust file

`/etc/priors/trust.toml` holds the host's profile, org lists, work-name floor,
`trust_root` and each store's id, path and remote. The path is fixed in the build: no flag, env
var or user config overrides it. The nix-config module writes it via
`environment.etc`.

```toml
profile       = "work"                        # the host kind: "work" or "personal"
work_orgs     = ["github.com/your-work-org"]  # host/owner; required on every host
personal_orgs = ["github.com/you"]
work_names    = ["build.corp.internal"]       # floor for the work-name scan; config.toml can only add
trust_root    = "owner-admin"                 # only "separate" turns attestation on
[stores.personal]
id     = "you-priors"                         # [a-z0-9-]{1,64}
path   = "/home/you/memory/personal"          # absolute; no ~/ expansion
remote = "git@github.com:you/priors.git"      # optional: the only URL this checkout may have and be pushed to
[stores.work]
id     = "work-priors"                        # only on a work profile
path   = "/home/you/memory/work"
remote = "git@github.com:your-work-org/priors.git"
```

Before reading, `priors` walks the path as traversed, symlinks and their
targets included. Every component must be owned by root, each directory
writable by root only or sticky, and the file a regular file writable by root
only. A trust file that is missing, fails the walk or does not parse (unknown
keys included) is a missing config: `priors` writes nothing, injects nothing
and reports it. The trust file and `priors` must come from the same hookyard
revision, since an unknown trust key fails closed.

`priors` refuses a checkout (reads, `add`, `index --write`, `lint` and the
push) unless its `origin` url, fetch url and push url each equal the
pinned `remote`, `origin` is its only remote, and every `branch.*.remote`,
`branch.*.pushRemote` and `remote.pushDefault` is `origin`. The fetch and push
urls have git's rewrites (`insteadOf`, `pushInsteadOf`, `pushurl`) applied,
under the user's git config, so the pin must be the post-rewrite URL and the
checkout's raw origin must equal it too. Because the check reads the user's
git config, a user-global `remote.pushDefault` or a globally defined remote
refuses every pinned store. With no `remote`, a store dir inside a repo that
has a named remote is refused and nothing is pushed; a remote reached only
through a URL in `branch.*.remote`, `branch.*.pushRemote` or
`remote.pushDefault`, or through a legacy `.git/remotes` or `.git/branches`
file, is not yet caught for an unpinned store (a follow-up).
A checkout git cannot read (a corrupt `.git/config`, say) is
refused, not published with a warning. A refused checkout also blocks
`priors add` to that store's local layer; reads still show the local layer.

The `remote` must be a real-host URL, not an ssh alias: the push drops
`~/.ssh/config`. The default identities or the ssh-agent must be able to
authenticate it.

### User config

`$PRIORS_CONFIG`, else `$XDG_CONFIG_HOME/priors/config.toml`:

```toml
# optional:
# work_names   = ["build.corp.internal"]  # added to the trust file's work_names; also rejected in the personal store
# state_dir    = ""     # default $XDG_STATE_HOME/priors
# event_record = ""     # hookyard's state dir; default follows hookyard's own
# rules        = ""     # extra redaction rules, added to the built-in set; a bad file fails closed
# ssh_config   = ""     # passed to `ssh -G -F` when resolving host aliases; plays no part in the push
# commit       = true   # commit published facts into the checkout
# push         = false  # needs an upstream on origin set once (git push -u origin <branch>); after a failed push, git push by hand to resume
```

`profile`, `work_orgs`, `personal_orgs`, `trust_root`, `stores`,
`personal_store` or `work_store` in `config.toml` is an error naming the trust
file.

`git`, `ssh`, `rg` and the secret scanner (betterleaks) are pinned at build time
(`nix build .#priors`) and never looked up on `PATH`, so the `scanner` key is gone;
a plain `go build` binary refuses every subcommand.

The store paths in the trust file must be absolute (no `~/` expansion), and so
must `state_dir` once `~/` is expanded. None of the three may be the same as,
or nested inside, another (the default state dir included). Symlinks in them
are resolved first, so a store behind a symlink behaves as the directory
it names, and the nesting check sees the real paths.

The push goes to the pinned URL from a build-pinned, empty, read-only bare git
dir in the Nix store (`GIT_DIR`), reading the checkout's objects through
`GIT_OBJECT_DIRECTORY`, so neither the checkout's config nor any
owner-writable config reaches it. Its environment is built from nothing:
`LC_ALL`, `GIT_TERMINAL_PROMPT`, `GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_CONFIG_NOSYSTEM`, a pinned `GIT_SSH_COMMAND` (with `-F` and
`UserKnownHostsFile` pointing at a build-pinned ssh config and known_hosts),
`GIT_SSH_VARIANT` and `SSH_AUTH_SOCK` passed through. Nothing else the caller
exports (`BASH_ENV`, loader, TLS or proxy variables) reaches git or ssh, and
`~/.ssh/config` and the user's `known_hosts` play no part. It refuses when
`git ls-remote --get-url <pin>` differs from the pin, and for a checkout that
is not sha1.

The pinned known_hosts covers github.com and `ssh.github.com:443`, so a store
that must use port 443 pins `ssh://git@ssh.github.com:443/<owner>/<repo>.git`
as both its origin and its `remote`.

Residuals: a sync outside `priors` follows the checkout's own config.
`priors` itself is dynamically linked (cgo, via `os/user`), so a loader
variable such as `LD_PRELOAD` in the environment that runs it reaches `priors`
itself; out of scope here.

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
parsed as bash and every simple command in it is judged on its words,
rendered without evaluating anything (quotes, `$'…'`, brace expansion,
`${X:-…}` operands, `${IFS}` and a variable's first literal assignment
resolved; arithmetic left to the token scan); every word, and a command's
arguments joined back together, is also re-read as a script, so a script
quoted, glued to `--opt=` or nested in `sh -c` is judged too. Statements
before a syntax error are still judged, and the text then also gets a token
scan; a command past a bound (every rendered byte charged to a 1 MiB budget
before it is written, re-read depth 8, 128 KiB per text, bracket nesting 256)
flags. It misses what the command text does not show (`git fetch` or `git
pull` content without a URL; aliases, shell functions, scripts on disk; an
unlisted fetcher with no URL literal), a command word built at run time or
from a variable's value beyond its first literal assignment before use
(`$CMD`, `$(printf g)h`, `eval "$X"`, `${0/bas/g}`, `X=xgh; ${X#x}`,
`${X@E}`, `/usr/bin/g[h]`), a quoted heredoc body's backslash escapes, a
script another program decodes before a shell runs it (`base64 -d | sh`,
`rev`, `xxd -r`), statements after syntax the parser rejects beyond what the
token scan sees, an
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
(`--title=curl`, `GIT_PAGER=curl`, `rsync -avxh`, whose `xh` is a fetcher),
a command the parser cannot read, a command past a bound (any over 128
KiB), a word starting `!` or `=`, a listed name as an array element, loop
item or `<<<` word, a printf / echo escape inside a quoted message (`git
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
