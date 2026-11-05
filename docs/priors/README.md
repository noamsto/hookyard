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
refuses every pinned store, and every unpinned store inside a repo. With no `remote`, a store dir inside a repo that
has a named remote, a `branch.*.remote`, `branch.*.pushRemote` or
`remote.pushDefault` entry (a URL included), or a legacy `.git/remotes` or
`.git/branches` file is refused and nothing is pushed.
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
priors index [--text] [--engine claude-code]   # session_start output (envelope on stdin)
priors index --write         # regenerate every MEMORY.md
priors verify --store ID --credential KEY   # the verdict job for one store, as priors-verify
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
  exact bytes. Byte-identical copies from two source dirs routing to one repo
  import once: the later copy reports `duplicate` against the earlier
  (`<path>` is the earlier copy's source followed by ` (this run)`), whatever
  the earlier copy's fate, and a
  copy of a store fact is reported against the store first.
- `propose <source> -> <path>: <why>`: a near-duplicate (same name, same
  folded description, body overlap) or a changed source. Nothing is written;
  the owner merges by hand.
- `propose <source> -> <path>: waits for [[t]], which is held back (flagged,
  refused or set aside)`: the fact links `t`, a fact held back (flagged,
  refused, set aside, or in the local layer awaiting review), so it is set
  aside rather than published with a dangling link. `<path>` is `t`'s file in
  the local layer (or where it would land flagged), or, for a target refused
  or set aside in this run, its source file. Nothing is written; it lands on a
  later run once `t` is in the checkout. A link to an in-run item that will never land (a byte
  duplicate of a store fact under another name) is set aside every run. A
  link to a name that exists nowhere, or to an item routed outside the
  personal store, is still refused as `wikilink-dangling`.
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
    (`routes to the quarantine (...)`) and to the work store on a work host
    (the quarantine if the work clone is missing);
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

A fact that would publish never leaves a dangling link in the checkout, which
would fail the commit gate. When a wiki-link names a fact held back (refused
or flagged into the local layer in this run, or already in the local layer),
the linker is set aside with the `waits for` line above instead of refused. A
linker whose target only becomes publishable later in planning is re-checked,
so the dry run and the real run agree (barring a flag that clears mid-run,
below). Facts that link each other (a cycle) publish in one run when each
passes its gates. When one member of a cycle is flagged, it lands in the
local layer for review and the members that would publish and link it are set
aside, so nothing of the cycle reaches the checkout. `priors lint` reports
`wikilink-dangling` for the flagged fact while it waits in the local layer
(lint resolves links within one root), and in the checkout once it is moved
there, until a re-run of `priors import` lands its linkers; re-run it right
after the move, since the checkout's commit gate refuses until then. Facts are
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

### priors verify

The verdict job of [§4.4](../design/memory-layer.md#44-read-path-three-tiers-one-budget):
it fetches one store's remote, walks the new commits, and writes the verdict
file that the index-time checks read. It runs only on a host whose trust file
classifies as `separate`, as the build-pinned `priors-verify` uid, and only for
a store the trust file lists with a pinned SSH remote, so a personal host has
no work job. Gating also needs at least one allowlist key that passes the
evidence check (so with the module's empty allowlist the job exits 1), an
absolute `--credential`, an existing `/var/lib/priors/<id>/` and a `private/`
of mode 0700. Anything else exits 1 before any fetch or write.

- Inputs: the trust file (never `config.toml` or the caller's environment),
  the `--credential` SSH deploy key, and its own state. HTTPS credentials are
  not supported. Only the ssh transport is allowed
  (`GIT_ALLOW_PROTOCOL=ssh`), so a non-SSH pin, including a
  `<transport>::<address>` one, is refused.
- State, under `/var/lib/priors/<store id>/` (which deployment creates): the
  `verdicts` file (0644, written atomically) and `private/` (0700) holding
  `mirror.git`, `record.json` (0600, the per-name record that never shrinks)
  and `lock` (a second run exits 1).
- Budget per run: 300 s for the whole run, 20 s of it reserved for the
  checkpoint and the verdict write; 50,000 newly walked commits; a 2 GiB
  mirror. Leftover `tmp_pack` and incomplete objects are deleted each run.
  A walk cut short saves its prefix and the next run resumes.
- Status, one of `ok`, `stale-remote` (fetch failed before authentication and
  the record's last tip is fully walked: verdicts are recomputed over the
  record), `behind` and `oversize`. A fetch failure counts as before
  authentication only when ssh (run with `LogLevel=VERBOSE`) never logged
  `Authenticated to` and stderr matches a fixed ssh pre-auth message; anything
  else is `behind`, so text in pushed objects cannot fake a refused
  connection. `behind` and `oversize` write a header-only verdicts file once a
  fetch has ever succeeded; before that, or with an unreadable record, any old
  verdicts file is deleted and none is written. Any failure after gating ends
  the run `behind`, so pushed content cannot keep an old `reviewed` by making
  the job error out.
- Crash safety: a run leaves a `private/running` marker until it finishes, and
  the next run first deletes the verdicts file, so a run that died never leaves
  old verdicts standing for 24 h. An error writing the record or verdicts also
  deletes the old verdicts file. Fact blobs larger than the maximum fact size
  never hold for check 8 (the attestation lapses); the job never reads them
  whole.
- `fetch.fsckObjects` is on, so a pushed malformed object fails the fetch and
  keeps the store `behind` until the remote is rewritten (residual (d)).
- Output: one line, `<id>: <status>[: reason]`, never a fact name or path.
  Exit 1 for gating failures and for a failure to write the state dir,
  otherwise 0.
- Trigger: systemd socket activation (`Accept=no`). The job accepts and closes
  pending connections without reading any byte, runs, and runs once more if
  any arrived meanwhile, so at most one run is queued. Each queued run
  re-reads the trust file and re-runs gating, so a trust-file change poked
  during a run takes effect on the queued run. It prints one line per run and
  the exit code is the last run's. Without the socket environment it runs
  once. launchd is deferred to
  [#191](https://github.com/noamsto/hookyard/issues/191).
- `priors trust check` is a stub until
  [#240](https://github.com/noamsto/hookyard/issues/240):
  `verify.CheckEvidence` accepts every parsed allowlist key, and the nix module
  writes an empty allowlist until it can run the check.

Deployment (the account, units, socket and credentials) is #191.

### Reading the session-start block

On Claude Code the block groups facts by type (`feedback`, `project`,
`reference`, `user`, `other`) under a one-line pointer to `priors search`,
`priors show <name>` and `priors list --flagged`. Codex and Pi get the flat
list, unchanged. The layout follows `--engine` if given, else the hook
envelope's `engine`, else whether `priors` runs inside Claude Code
(`CLAUDECODE`); a plain-terminal `priors index --text` shows the
flat list.

A fact whose topic file the project's native `MEMORY.md` links is left out of
the block. A fact imported from Claude memory matches only by its recorded
source path; any other fact matches by `name`. In both cases the native
topic's description must equal the fact's description, and the topic must be
linked from the first 200 lines / 25 KB of `MEMORY.md`, since that is all
Claude loads; a same-named file nothing links, or one with a different
description, does not hide a fact. The source-path match uses `~/…` paths, so
across hosts it holds only if the repo's absolute path is the same under
`HOME`.

Markers in the injected block:

- `## flagged, unreviewed`: the local layer's facts, `proposed`, because a gate
  flagged them. Nobody has cleared them. They come last.
- `N facts also in MEMORY.md`: how many facts the dedup left out.
- `… N entries omitted (group, group) …`: a cut to the budget, on its own line
  right after the pointer line, before the groups, naming the groups that lost entries.

Markers only in `priors list` rows:

- `flags:`: why a gate flagged the fact.
- `verified never`: `verified` is null, as an import leaves it. `priors add`
  stamps today's date.
- `reviewed · `: the fact is attested.
- stale: `priors list --stale` shows facts never verified or not verified for
  90 days.

To check that priors works, run `priors list --flagged`, `priors list --stale`
and `priors lint`; the first two print nothing when there is nothing to show,
and `priors lint` prints a finding per line and exits 1, or nothing and exits
0. Run `priors list --flagged` inside the repo: it shows only local-layer
facts learned in the current repo (`_norepo` outside one), whatever `--repo`
says, so empty output elsewhere does not mean nothing waits.

`priors` has no review subcommand and no promote step. Read a flagged fact
with `priors show <name>`; its `list --flagged` row ends in its `flags:`. To
drop it, `gtrash put` its file under the local layer
(`$XDG_STATE_HOME/priors/local/<store>/<repo>/`). To keep it, there is no
supported publish-after-review path: a fresh `priors add` runs every gate
again and publishes only if they pass, and a fact claims `reviewed` only
through attestation (`trust_root = "separate"`), which the write guard keeps
agents from writing. `priors lint --move-flagged` goes the other way: it
moves checkout facts that trip a content or size gate into the local layer.

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
- naming `PATH`, `BASH_ENV`, `ENV`, `CDPATH`, `IFS`, `BASH_CMDS` or
  `BASH_ALIASES` other than as an expansion, or declaring a nameref (`-n` of
  `declare`, `typeset` or `local`; `export -n` unexports, and `readonly -n`
  declares none), makes the command one that may write; assigning any `GIT_*` or `LD_*` variable is a
  write, like assigning `PATH`. The name counts in any spelling (quoted,
  escaped, brace-expanded, ANSI-C-quoted, or built at run time) in a setter's
  name position (`declare`, `typeset`, `local`, `export` and `readonly`,
  naked names and a `-n` target included; `read`, `mapfile` and `readarray`,
  `printf -v`, `getopts`, `wait -p`, `unset`, and a `{var}` redirect), in a
  rendered word (`PATH`, `BASH_ENV`, `ENV`, `CDPATH` and `IFS` only), and
  inside arithmetic (`(( ))`, `let`, an index, an expansion's index or slice:
  `${a[i]}`, `${s:i}`, `${#a[i]}`). Once arithmetic is evaluated anywhere in
  the command, it also counts in every value the command assigns, not only a
  name's first (`x='I'"FS=5"; (( x ))`): each assignment, an append joined to
  the name's last value, a `declare`-family `NAME=value`, what `printf -v`
  prints, and the heredoc or here-string `read` reads. An opaque word inside
  arithmetic whose text holds `=`, `++` or `--` (`let "${v^^}FS=0"`) may set
  any name. While `IFS` may have changed, the words of an unquoted expansion
  cannot be told;
- `eval` always reads its arguments as a script, and so does a `sh -c`-style
  string (`bash`, `sh`, `zsh`, `dash`, `ksh` or `ash` with `-c`). `source`, `.`,
  an opaque command word and an opaque setter name (`read -r "$name"`) are a
  setter the guard cannot tell: any protected name may have been set. A
  variable a setter re-sets is unknown afterwards, so a later `"$n"` after
  `read n` is an opaque word (for a `declare`-family name only after a
  value-changing option: `-n`, `-l`, `-u`, `-c`, `-i`, `-a` or `-A`);
- a command whose `IFS` or `CDPATH` changes anywhere in it is read as if it
  had changed from the start, so a function body or a trap that runs a word
  after the setter is read under it. The command is read again until neither
  changes (at most three reads), within the same parse budget;
- the value of a writer's option is an operand: `--name=value`, the suffixes
  of a short-option cluster (a `-name=value` is one, and its `value` counts
  too), and the word after an option (`env -vC dir`, `sudo -iD dir`, `env --ch
  dir`) are judged like the writer's other operands. A cluster of more than
  16 letters is read by the suffixes after its first and last 16, and is also
  an opaque write and a dir that cannot be told. A value holding shell text is read
  as a script (`env -u X --split-string='rm -rf .'`, `foo -c'rm -rf .'`), and
  a value is a dir that the command's relative writes resolve against, from
  the cwd and every dir the command changes into; only relative writes are
  judged against it;
- in a tool's input, a string is also judged by its path tokens, split as
  the shell reader splits a field; every field of a string under a path key
  counts (`a,notes`); a JSON document inside a string under a path or unknown
  key is read as input, with a budget of its own (a content or edit text is a
  file's own text, not input); past that budget, the string is judged by its
  first 256 path tokens; a
  `file:/abs` path is a path; and the elements of an array under a content or
  unknown key are also joined for the `reviewed` check, so a claim split
  across them is seen;
- a POSIX bracket class (`[[:alpha:]]`) in a glob matches any one character;
- a substitution inside arithmetic is read as the statements it runs, like
  any other. A setter's subscript, and once arithmetic is evaluated an
  assigned value holding `$(`, `${` or a backtick, is parsed as an arithmetic
  expression and read the same way; one that does not parse may set any name,
  is an opaque write, and the substitutions a scan of its text finds are read
  as scripts. A `$(( … ))` is arithmetic, not a substitution;
- a heredoc or here-string to a writer is read per value when it holds a loop
  variable; the names `claude`, `gh`, `glab` and `crew` as data takers are
  matched as globs (`~/bin/g?` may be `gh`); an output redirect to a run-time
  target withdraws data positions;
- bash `[!…]` globs are read as negated classes, a `]` first in a class as a
  member, and a `[` with no close as a literal (a jq filter `'.[] | …'` names
  no `.attest` dir);
- a sed script that holds `w`, `W` or `e` anywhere is read as a program: its
  `w`, `W`, `e`, `r`, `R`, `a`, `i`, `c` and `y` commands, an `s` flag `w` or
  `e`, a backslash-newline continuation, a bracket expression holding the
  delimiter or a backslash, an escape such as `\x`, or anything else the
  guard does not parse make sed a command that may write; a script without
  those letters only reads, and those letters in a regex, a replacement or a
  label do not (`sed 's/refused: //'` reads);
- a sed script is still judged through its operand, so `sed -i 's/^  x = 100;$/  x = 30;/' f`
  is refused: the guard reads the script's `/` fragments as a possible write to
  `/`, even though the script cannot write a path. This is a known residual
  (#268): use the editor tool or a script file (`sed -f`) instead. A sed script
  with a `w`, `W` or `e` command, and any other writer, is refused as before.
  A write denial names the argument it judged, e.g. `... inside the personal
  store (from the argument "-C/")`.
- the message of `git commit` (`-m`, `--message`), and the `--body`,
  `--title`, `--message`, `--notes` and `--description` values (`-b`, `-t`,
  `-m`) of `gh` and `glab` `pr`, `issue`, `mr` or `release` with `create`,
  `edit`, `comment`, `review`, `merge`, `close`, `reopen` or `note`, hold
  data, so their values are not judged as paths: a commit heredoc that
  mentions `MEMORY.md` is allowed. Any other command's such option is judged,
  and a command run inside such a message is still judged;
- some values are data, so a lone `.` or a `/` in one is not read as a path or
  as shell text: the `--jq` and `--template` values of `gh` and `glab` (and
  `-q` for `gh` only) on a JSON-output subcommand (`api`, `pr`, `issue`, `mr`,
  `repo`, `run`, `release`, `search`, `workflow`, `gist`, `cache`, `label`,
  `project`, `ruleset`, `secret`, `variable`, `status`, `org`, `ssh-key`,
  `gpg-key`, `codespace`); the `claude` prompt, the word after a
  `-p` or `--print` that is its first argument; and the body of `crew reply`
  and `crew msg` (the word after the recipient, or after sender and
  recipient, a bare `.` or `/` included, with `--crew <id>` not counted
  wherever it sits). A command substitution in one is still judged. Every
  other position is judged: any other `claude` argument (`--settings`,
  `--mcp-config`, `--append-system-prompt`, `--agents`, `--add-dir`,
  `--debug-file`, a flag's value), `claude mcp`, a `gh` or `glab` subcommand
  off that list (an alias or an extension), and anything after `--`;
- those positions apply only when the program is found through bare wrapper
  names alone (`env`, `timeout 5`, `env -u X`, `env -C dir`, an unquoted
  `NAME=value` prefix the parser strips before the command word, or a
  `NAME=value` after `env` or `sudo`). A path-invoked wrapper (`./nice`,
  `/x/env`), a skipped wrapper flag (`exec -a name`, `sudo -u user`, `env -S`),
  a leading quoted `"NAME=value"` (a program the shell runs under that literal
  name), and a bare number or `NAME=value` word after any other wrapper (`env 5
  claude …`) make the resolution unclean, and the command is judged like an
  unknown program;
- a command that can redefine how a name resolves loses every data position,
  wherever in the command the redefinition sits, since a `trap`, a loop or a
  function can run it before or after: a function, `alias`, `hash`, `enable`,
  `source`, `.`, `eval` or `trap`; naming `BASH_CMDS`, `BASH_ALIASES`, `PATH`,
  `BASH_ENV` or `ENV` other than as an expansion, even quote-split or
  ANSI-C-quoted; an opaque command word, or an opaque word in a write
  position, except one opaque only through the variable of a `for` loop (not
  `select`) whose name starts with a lowercase letter, over listed plain
  values (literal or quoted text with no glob, no expansion and no empty
  value, at most 64 values counting brace expansion, at most 64 combinations
  per call): that word is judged once per value, and every value must leave
  the call's data positions in place (`for n in 1 2; do gh issue view $n --jq '.a // .b'; done`),
  unless the command also sets the variable (any other assignment, `read`,
  `mapfile`, `printf -v`, arithmetic, a `{name}` redirect) or mentions it in
  any other word, declares a nameref, uses arithmetic, an array subscript, an
  integer test or `declare -i` anywhere, or holds any other word whose value
  the text does not tell. The exception also needs every command in the call
  to be one that sets no shell variable (a closed list of readers plus `gh`,
  `glab`, `claude`, `crew`, `git`, `sed`, `rg`, `fd`, `find`, and `printf`
  with a literal format), with no assignment, declaration, `[[ ]]`,
  arithmetic or `{var}` redirect anywhere, and loop values holding no glob
  character (`*`, `?`, `[`), even quoted; and a word,
  redirect target or path named `claude`, `gh`, `glab` or `crew` that is not
  a call's own command word (`ln -s /bin/rm ~/bin/crew`, `cat /bin/rm >
  ~/bin/gh`). A renamed data taker cannot hide a write: its words are judged
  as an unknown program's. A `for` variable over a single item that expands
  (a glob or a brace) is read as unknown, not as that item's text.

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
(including one set up by an earlier call of a persistent shell), a `claude`,
`gh`, `glab` or `crew` that an earlier call of a persistent shell replaced or
shadowed, with nothing in this command showing it (its data positions are
skipped, and the program may run those words as code), a relative
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

- arithmetic on run-time data (`read x; (( x ))`, `(( $n ))` with `n`
  unknown, an opaque word whose own text holds no `=`, `++` or `--`), which
  can name a protected variable;
- a runner that takes its dir as a positional word (`chroot sub rm ../notes`),
  and a dir that comes from a config or env file;
- an opaque option value (`--wd=$X`): it is an opaque write word, not a dir;
- an opaque word before `printf`'s `-v` or `wait`'s `-p`
  (`printf "$(echo -v)" IFS Q`), and a glob as a setter's name operand
  (`read I?? <<< Q`);
- the `.` in a string that does not parse (`watch 'rm -rf . ('`);
- an unknown-key string longer than 4096 bytes, which is not judged at all, so
  a packed list padded past it passes;
- a data taker's name in another case on a case-insensitive file system
  (`~/bin/GH` on macOS), and variables an earlier call of a persistent shell
  set;
- a value read as arithmetic that the command builds from pieces
  (`a='a[$'; b='(rm -rf ../notes/facts)]'; x=$a$b; (( x ))`) or appends to an
  array element or to a value a `declare`-family word, `read` or `printf -v`
  set (`x=('a[$' b); x[0]+='(…)]'; (( x ))`), and a value that does not parse
  as arithmetic whose substitutions a quoted paren hides
  (`x='a[$(echo "(") + $(cd ..; rm -rf no*s)] + 1 a'; (( x ))`): its write is
  an opaque one, denied only when the command names a guarded path;
- a JSON document in a tool-input string past its own budget whose store path
  hides past 256 path tokens, behind JSON escapes (`\/`), or relative to a
  store cwd;
- a dir glued after more than 32 letters of a short-option cluster, past the
  suffixes after its first and last 16 letters, when its name does not name a
  store as a whole element (`env -v…v…Clonglink rm -rf facts`, a long link
  into a store).

It over-denies:

- a shell command that reads a store path while it writes elsewhere, a shell
  write to a store fact, and a write that may name a dir holding a store
  (`cmake ..` from a dir directly under one, `foo .` or `cp x .` from `~`),
  including such a word inside an unknown program's argument that holds a
  space, since the argument is read as a script (`some-tool "check ."` from
  `~`);
- a `claude` prompt, a `gh` or `glab` jq filter or template, or a `crew`
  message holding a lone `.` or ` / ` whose data position is withdrawn: behind
  an unclean wrapper (`sudo -u bob claude -p .`), or in a command that also
  redefines a name or mentions a word named `claude`, `gh`, `glab` or `crew`
  that is no call's command word (`echo gh`);
- such a data position in a command whose write word is opaque through a
  `for` variable, when the variable is mentioned elsewhere in the command
  (`echo "n=$n"`, a jq filter `.n` read by `jq`), a `$(…)` or unknown
  variable sits beside the loop, the command uses arithmetic anywhere
  (`$((…))`, `[[ $n -gt 1 ]]`, `${a[i]}`), the loop runs over more than 64 values or
  a glob, or the loop sits beside any other command or assignment (`test`,
  `read`, `x=1`);
- a reader run behind a flagged wrapper (`nice -n 5 git status`) in a store or
  naming one;
- assigning or exporting `PATH`, `BASH_ENV`, `ENV`, `GIT_*` or `LD_*`, or
  `printf -v`, in front of a reader in a store or naming one (`PATH=$PATH:/x
  git status` in a store);
- a command that mentions `PATH`, `IFS` and the like as a word (`echo "set
  PATH"`) while it names a store or runs in one, and a field whose printf
  decoding names one; a word inside arithmetic, or (once arithmetic is
  evaluated) any assigned value, that spells a protected name, `GIT_*` and
  `LD_*` included, counts as setting it;
- after `source`, `.`, an `eval` with an opaque argument, `eval ''`,
  `mapfile -C`, an opaque command word, an opaque setter name
  (`read -r "$name"`) or an opaque or empty `getopts` option string, unquoted
  expansions and relative `cd`s are unknown;
- a writer's option value or glued short-option suffix that names a store, `.`
  or a dir holding one relative to the cwd (`rsync --exclude=notes`, `foo -o.`
  from `~`), or that holds shell text, which is read as a script; past 256
  dirs from option values (in one call or in all) or 32 dirs the command
  changes into, the relative writes of a command that names a guarded path
  are unknown, and so is a glued value long enough to pass the parse budget;
  option dirs never push a later `cd` out; a writer's short-option cluster
  of more than 16 letters (`-DVERYLONGMACRONAME`) is an opaque write and a
  dir that cannot be told; each relative write is judged against every option
  dir, so a command with a few hundred option values (`rsync` with 250
  `--exclude=`s) can run past the guard's time bound and is denied;
- a write tool's text under an unknown key (or a content key when the input
  has no path key) that holds a path token naming a store, a dir holding one
  or an `.attest` element (an issue comment body naming `~/notes/MEMORY.md`),
  and a path-key string any token of which names one (`a,notes` from `~`);
  a store-fact write whose array elements, joined, hold `reviewed`;
- a glob basename that could match `claude`, `gh`, `glab` or `crew`
  (`rm -f *`), an option value whose basename is one, and an output redirect
  to a run-time target withdraw data positions; a writer's heredoc or
  here-string holding a substitution or an unknown variable is an opaque
  write;
- arithmetic anywhere in a command that assigns a value holding `$(`, `${`
  or a backtick, which may then write, and may set any name when the value
  does not parse as arithmetic (`x='echo ${y}'`); a setter's subscript that
  does not parse as arithmetic (`unset 'm[a b]'`), which may set any name;
- an opaque word inside arithmetic whose text holds `=`, `++` or `--`
  (`let "i=$1+1"`), which may set any name;
- a POSIX class in a glob that names a guarded path matches any one
  character;
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
- a JSON document under a path or unknown key past 16384 nodes or 16 levels
  whose path tokens name a store or a dir holding one (`"/"`), since the
  string is judged by its tokens instead;
- an abbreviation of a harmless sed or fd long option.

## Store repositories

Each store is its own private git repo. Run `priors lint` there as a
required check: [`store-ci.example.yml`](store-ci.example.yml) is a
starting point.
