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
priors import [--dry-run] [--claude-dir D] [--session ID] [--external]
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

`import` copies Claude Code's native memories (the topic files under
`<claude-dir>/projects/*/memory/`) into the personal store (§4.9), each
through the same write path as `add`. `--claude-dir` replaces the whole
`~/.claude`, not a memory dir: pointed at a memory dir it finds no
`projects/` and imports nothing, exit 0 (the run still reports unreadable
store facts and commits the checkout, as every run does). The native files
are never changed. Each copy keeps the native `name` (slugified when it is
not a valid fact name), `description`, `type`, body and `modified`; the
importer sets `confidence: proposed`, `scope: repo` with the repo decoded
from the project dir, `verified: null`, `superseded_by: null`, `provenance`
and a `source` block carrying the `sha256` of the native bytes. It prints one
line per item, then exits 0, or 1 if any item failed (below):

- `published personal <path> <- <source>`: it passed every gate and was
  written into the checkout.
- `flagged personal <path> <- <source>: <reasons>`: a gate flagged it, so it
  waits, `proposed`, in the host-local layer.
- `unchanged <source>: <path>`: this source was imported before, unchanged.
- `duplicate <source>: <path>: same source bytes`: a fact already holds these
  exact bytes.
- `propose <source> -> <path>: <why>`: a near-duplicate (same name, same
  folded description, body overlap) or a changed source. Nothing is written;
  the owner merges by hand.
- `skipped <source>: <why>`: the item is not imported. The reasons:
  - not read: a symlink or other non-regular file, a file over 1 MiB, one
    that cannot be opened, or a memory dir that cannot be read or is not a
    directory;
  - not a fact: no or invalid frontmatter;
  - not routed to the personal store: no repo (the project dir decodes but
    is outside any git repo), a repo that is not a valid name, or a repo
    §4.2 routes to the work store or the quarantine. A project dir that does
    not decode to exactly one existing directory is unresolvable, so it
    routes to the quarantine on a personal host
    (`routes to the quarantine (...)`) and to the work store on a work host;
  - a name that cannot be slugified;
  - `refused: <why>`: the write path's policy said no (a secret match, a
    lint finding such as `wikilink-dangling`, a name already taken, a
    destination it will not write through), or the destination is a store
    file that cannot be parsed;
  - `failed: <why>`: the machinery broke (scanner unavailable, a file or
    index write, a filesystem error checking the destination), not a
    decision. A re-run may clear it. After printing every line the command
    exits 1 with a count on stderr.
- `skipped <path>: unreadable store fact: <err>`: a store file that cannot be
  parsed, so dedup cannot see it. It is reported on dry runs too, and an
  item whose destination is that file is refused rather than overwriting it.

A store the run cannot lock or read stops it before any item: `refused:
<err>` on stderr, exit 1.

A fact that would publish is refused (`wikilink-dangling`) when a wiki-link
names a fact of the same run that will not reach the checkout, because it is
refused or flagged into the local layer; otherwise the checkout would fail
the commit gate. A linker whose target only becomes publishable later in
planning is re-checked, so the dry run and the real run agree. Facts are
written link targets first. If a fact planned to publish is not published
(its write failed, or it landed flagged), a fact that links it (directly or
through another) is not kept: it is reported
`skipped <source>: failed: it links <name>, which was not written`, or
`failed: rolled back, ...` when a link cycle had it written first. So is a
fact that publishes only because a flag cleared mid-run while it links an
in-run fact that will not reach the checkout.

`MEMORY.md` is not imported, since each store generates its own; a line in
it that is not a link to a topic file in the same dir is reported as
`skipped <source>:<line>: <why>`. Run `priors import --dry-run` first: it
runs every check and writes nothing. Where a real run would publish or flag
it prints `would publish personal <path> <- <source>` or
`would flag personal <path> <- <source>: <reasons>`, and the same
`unchanged`, `duplicate`, `propose` and `skipped` lines. A real run holds
the personal store's lock throughout, so no other writer commits a
half-imported checkout, and commits what it published once, through the
store's commit gate. It commits on every run, so a commit held back last time
(`warning: ...` on stderr, exit still 0) is retried. Running it again is safe:
an imported source reports `unchanged`, by the sha256 of its bytes.

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
- `priors-guard` on `pre_tool` with no `match`, on the verdict lane with a
  1 s budget. It writes the shell markers before the command runs, which
  covers a call that fails (and so never reaches `post_tool`) and one that
  runs `priors add` itself. It gives up at its 750 ms deadline, and a call
  it gave up on is left unmarked: a lost mark fails as gate 2 describes. It
  is also the write guard (see below), and denies a call that would write
  what only priors or attestation writes.

### Write guard

The `pre_tool` write guard is a tripwire against an agent editing what it
must not: it denies a tool call that would write

- a store's attest entries (`.attest/`, and any path with an `.attest`
  element), its generated `MEMORY.md` index or its `.git` dir;
- a store fact so that it claims `confidence: reviewed`, judged on the
  content a Write sends and on the file as an Edit would leave it;
- priors' state dir and its config (the config dir when it is named
  `priors`, else the config file);
- any dir holding one of these (`rm -rf ~`).

The stores are the ones the trust file pins, so the guard covers every
configured store. A shell command cannot have its content judged, so a shell
write anywhere in a store is denied, and so is any write while the command
names a store path, runs in a store, or changes into one. Paths resolve
against the call's cwd and every dir the command changes into, with `..` and
symlinks resolved as the kernel does, and also with `..` cleaned first, as
Claude Code and pi clean a file tool's path. A hard link elsewhere to any
store file (a fact, `.git/config`, the index, an attest entry; the walk covers
every store file, `.git/objects` included) or to the config file is caught: a
file tool's write through a link to a fact is judged for a reviewed claim like
a write to the fact itself, and a shell write through any such link is denied.
The config file is guarded itself, so the target of a symlinked config is
covered. A write through a path built at run time (`$(…)`, an unknown variable, xargs,
a pipe into a writer) is denied when the command names a guarded path
anywhere.

Every tool that is not a read or a shell (an MCP server's tool, say, and the
known file tools too) has the strings under keys the guard does not know
judged, at any depth, as paths relative to the call's cwd, up to 4096 bytes,
as given and trimmed. Those strings also count as content for the reviewed
check. A string under a content key is judged as a path too when the input
names no path under a path key. Keys that differ only in case, `_` or `-` all
count. A leading `@`, `~` and `~/`,
`file://` URIs and Unicode spaces in a path are read the way pi reads them.
An edit through pi's `edit` whose new text holds `reviewed`, `\`, `!` or `*`
in a store fact is denied, since pi matches fuzzily and applies every edit
against the original file. The file as each edit would leave it is also
judged, under Claude's sequential matching and under pi's all-at-once,
fuzzy-normalized matching.

In a shell command:

- a wrapper flag's value is not trusted as a reader's name (`env -u cat rm …`);
- `env -S` and `--split-string` read the split string as a command that may
  write; `env -C` and `--chdir`, and `sudo -D` and `--chdir`, change dir;
- git global options outside a known list, and abbreviated or bundled
  sed, git and fd options (`sed --in-pl`, `git grep -O…`, `fd -Hx`), count as
  writes; git's `--ext-diff` and `--textconv` count as running a program;
- `CDPATH` assigned in the command makes a relative `cd` or `pushd` a dir the
  guard cannot tell. With a `CDPATH` in the guard's own environment that
  leads into a store dir, a relative `cd` under it with a write is denied when
  the command names a guarded path;
- `pushd` and `popd` with `+N` or `-N` name a dir the guard cannot tell;
- naming `PATH`, `BASH_ENV`, `ENV`, `CDPATH` or `IFS` other than as an
  expansion (or in `let`, `(( ))`, `read`, `mapfile` or `printf -v`), or
  declaring a nameref, makes the command one that may write. While `IFS` may
  have changed, the words of an unquoted expansion cannot be told;
- assigning any `GIT_*` or `LD_*` variable is a write, like assigning `PATH`;
- bash `[!…]` globs are read as negated classes, a `]` first in a class as a
  member, and a `[` with no close as a literal (a jq filter `'.[] | …'` names
  no `.attest` dir);
- a sed script is read as a program: its `w`, `W`, `e`, `r`, `R`, `a`, `i`,
  `c` and `y` commands, an `s` flag `w` or `e`, a backslash-newline
  continuation, a bracket expression holding the delimiter or a backslash, an
  escape such as `\x`, or anything else the guard does not parse make sed a
  command that may write; those letters in a regex, a replacement or a label
  do not (`sed 's/refused: //'` reads);
- the message of `git commit` (`-m`, `--message`), and the `--body`,
  `--title`, `--message`, `--notes` and `--description` values (`-b`, `-t`,
  `-m`) of `gh` and `glab` `pr`, `issue`, `mr` or `release` with `create`,
  `edit`, `comment`, `review`, `merge`, `close`, `reopen` or `note`, hold
  data, so their values are not judged as paths: a commit heredoc that
  mentions `MEMORY.md` is allowed. Any other command's such option is judged,
  and a command run inside such a message is still judged.

`LSP` and an MCP tool are reads when the tool's last name segment starts with
`search` or `list_`, or is `get_file_contents`, `read_file` or
`read_text_file` or ends with one of them, and the segment holds no write verb
anywhere in it (replace, write, edit, create, delete, move, save, add, set,
start and the like). A read-shaped name that holds one, such as
`list_commits` or `list_assets`, is judged as an unknown tool.

What the guard does when it cannot load its inputs depends on what is
missing. A host with no trust file (`/etc/priors/trust.toml`) is inert, and
the guard allows every call. A missing user config loads the defaults, since
an agent can delete the config and a missing file must not open the guard. A
trust file that fails the ownership walk, or a trust file or user config
that cannot be read, parsed or validated (unknown keys, trust-file keys in
the user config, a bad state dir), denies every call that may write, naming
the load error. So do a command past the parser's budget, a call whose tool
input cannot be read, a guard that panics and a guard that runs past 750 ms.
It fails open only where hookyard abstains (a call it does not route, a
handler that crashes or runs past its budget) and on an envelope priors cannot
read within 800 ms, which fails open like a handler past its budget. Load on
the host can cause either.

It is not a sandbox. It does not see a path computed at run time from
nothing the command names (`base64 -d`, a file's content, a script file), a
path an interpreter assembles from pieces, an archive or patch unpacked from
a cwd that holds a store, a command that shadows a reader's name on `PATH`
(including one set up by an earlier call of a persistent shell), a relative
`cd` that `CDPATH` leads into a store when the command names no guarded path,
and a `CDPATH` exported by an earlier call of a persistent shell, code run
from config an agent can write (git's global `core.hooksPath`, Makefiles) or
a variable a shell profile sets, `/proc/<pid>/cwd`, a bind mount, or macOS
Unicode normalization. It does not see an MCP tool named like a read, with no
write verb, that writes, nor an MCP tool that runs a command under a key
other than `command` (a process-interaction tool's `input`). It does not see
`git add`, `git commit`, `git fetch` or `git push` run in or on a store (git's
own index, objects and refs count as git reading). It does not see a fact
edited through a hard link created after the check. It does not see a call
hookyard does not route, a call after hookyard abstains for a handler that
crashes or runs past its budget, or a race that swaps a path between the check
and the write.

It also does not see these open classes:

- a variable that changes how a word resolves or splits, or which program
  runs (`PATH`, `IFS`, `CDPATH`, `GIT_*`, `LD_*`), set under a spelled name:
  quoted, escaped or brace-expanded, through `eval`, a `{var}>` redirect, or a
  nameref reached indirectly;
- a wrapper's dir option in a spelling the guard does not parse (a
  short-option cluster, an abbreviated long option, inside `env -S`, behind a
  wrapper it does not list), and another runner's glued relative dir
  (`unshare --wd=dir`, `--directory=dir`, `--prefix=dir`);
- paths packed into one string: a comma- or newline-separated list, or a JSON
  document in a string;
- a `file:` URI with a single slash;
- a reviewed claim split across the elements of a line array;
- a program the guard does not list as a writer (an unlisted delete tool or
  interpreter) that writes its `.` operand from a dir holding a store, or its
  `/` operand. A listed writer (`rm`, `cp`, `tar`, `find`, …), a listed shell
  or interpreter, a path-invoked program, or an unknown wrapper that names a
  listed writer (`watch rm -rf .`) is still denied, as is a write naming `..`,
  `~` or another dir that holds a store.

It over-denies:

- a shell command that reads a store path while it writes elsewhere, a shell
  write to a store fact, and a write that may name a dir holding a store
  (`cmake ..` from a dir directly under one, `cp x .` from `~`);
- an unlisted program's `.` or `/` operand when its own words also name a
  listed writer (a `claude -p` prompt run from `~` whose prose mentions `git`
  or `make` beside a lone `.`), or behind a flagged wrapper (`env -u X prog
  "a / b"`);
- a reader run behind a flagged wrapper (`nice -n 5 git status`) in a store or
  naming one;
- assigning or exporting `PATH`, `BASH_ENV`, `ENV`, `GIT_*` or `LD_*`, or
  `printf -v`, in front of a reader in a store or naming one (`PATH=$PATH:/x
  git status` in a store);
- a command that mentions `PATH`, `IFS` and the like as a word (`echo "set
  PATH"`) while it names a store or runs in one;
- with `CDPATH` set on the host, a relative `cd` and relative write in a
  command that mentions `MEMORY.md`, `.attest` or a store's name;
- an unknown tool whose input holds `.`, `..`, `/`, or a word that names a
  store or a dir holding one relative to the cwd (`{"repo":"notes"}` from
  `~`);
- a file tool writing a store fact whose other fields (an instructions or
  description field) hold the word `reviewed`;
- from a cwd inside a store, an unknown tool whose input holds the word
  `reviewed` anywhere, since each short string resolves to a store file;
- an MCP read tool outside the list that names a store's `.git`
  (`read_multiple_files`);
- a pi edit to a fact whose new text holds `reviewed`, `\`, `!` or `*`,
  markdown emphasis included;
- a commit message that names a guarded path in a command that also writes
  through a path built at run time;
- a `gh` or `glab` body built by a command (`--body "$(cat <<'EOF' …)"`)
  that names an `.attest` element or a store path, since `gh` may write and
  the command's own words are judged;
- a git short-option cluster holding `O` (`git log -SOops`);
- a git long option that is a prefix of one that runs a program (`git diff
  --text`, a prefix of `--textconv`);
- prose or patch bodies under an unknown or content key (a PR body, a chat
  message) that hold a path with an `.attest` element, since each short string
  is judged as a path;
- an abbreviation of a harmless sed or fd long option.

## Store repositories

Each store is its own private git repo. Run `priors lint` there as a
required check: [`store-ci.example.yml`](store-ci.example.yml) is a
starting point.
