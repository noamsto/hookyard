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
runs in a store or changes into one. A write the guard cannot resolve is also
denied while a store is in reach (below). Paths resolve
against the call's cwd and every dir the command changes into, with `..` and
symlinks resolved as the kernel does, and also with `..` cleaned first, as
Claude Code and pi clean a file tool's path. A hard link elsewhere to any
store file (a fact, `.git/config`, the index, an attest entry; the walk covers
every store file, `.git/objects` included) or to the config file is caught: a
file tool's write through a link to a fact is judged for a reviewed claim like
a write to the fact itself, and a shell write through any such link is denied.
The config file is guarded itself, so the target of a symlinked config is
covered.

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

A shell command is judged one simple command at a time, in every text the
reader parses: each stage of a pipeline, each command of a loop, a `&&` list,
a function body, an `xargs` inner command, a command substitution, a heredoc
read as a script (`bash <<EOF`) and a literal `-c` script. A word is rendered
statically: literal text, quotes, ANSI-C quotes, brace expansion, `~`, globs
kept as written, and a plain host variable the command cannot set. A `$NAME`
bash assigns itself never renders, and neither does one bash assigns only
when unset (`PATH`, `SHELL`, `TERM`, `MAILCHECK`, `HISTFILE`, `HISTSIZE`,
`HISTFILESIZE`, `POSIXLY_CORRECT`) while the host leaves it unset. Bash
expands `~` after the `=` of a word shaped `name=…` and after each `:` in it
(`of=~/x`, `PREFIX=~/notes`, `PATH=~/bin:…`), and the guard renders it the same
way; `--opt=~/x` stays literal. An unknown program may expand a `~` bash
leaves to it, so each field of its operands, and of an environment value
(below), that starts with `~` (`~`, `~/…`, and `~user/…`, that user's home
from the password database) is judged both as written and with the home in
its place (`foo a["x"]=~/notes/MEMORY.md`, `foo '~/notes'`,
`foo '~alice/notes/MEMORY.md'`); `~+`, `~-` and `~N` stay literal, as no
program expands them. A short cluster's glued suffix and an option value that
start with `~` are read the same way (`foo -o~/notes/MEMORY.md`,
`ssh -M -S~/notes/x host`, `foo -C'~/notes' build`, the last also naming its
option dir with the home in place); under a `HOME` the command may set, such
a field is an unresolved write. Anything
else (a variable the command assigns, `$(…)`, `$((…))`, a positional
parameter) is unresolved, with its literal prefix kept. The guard tracks no
values in a word: `d=/tmp/x; rm -rf "$d"` is unresolved.

Each simple command, after its wrappers, is one of three kinds:

- read-only: it writes nothing but its redirects, and its words are never
  targets (`cat`, `grep`, `git log`);
- modeled writer: its targets are exactly the words its table names (`rm`,
  `cp`, `mv`, `tee`, `sed -i`, …);
- unknown program (`make`, `python3 -c …`, `curl -o`, anything with a `/` in
  its name): it writes its operands and its cwd, so it is denied when it runs
  inside a store (check 4), not when its cwd merely holds one. Each operand with no script
  byte (whitespace, quotes, `\`, `$`, a backtick, `;&|()<>{}`) is a target as
  a whole word, and so is each path field of it (`key=/abs`, `out:/abs`,
  `a,/abs`, `of=/`), a long option's value and a short cluster's glued
  suffix. An operand that holds a script byte (inline code, prose) is an
  unresolved write. So is any operand that does not render, and stdin from a
  pipe, heredoc or here-string. An operand that does not render is also
  judged by its literal start: its complete path fields are targets, and its
  literal prefix and the part of it after the last `=`, `:`, `,` or other
  path break are target prefixes under check 6 (`foo GIT_DIR=../no$(true)tes`
  from a sibling of `~/notes`). Its relative operands also resolve against
  each of its own option values that names an existing dir (at most 32 per
  call; past that the command is denied as writing from a dir the guard
  cannot tell).

A target is a whole word in an output position. A regex, a replacement,
quoted prose or a data argument of a read-only command is never one, unless
the allowlist is withdrawn, which turns that command into an unknown program.
`build/` is the target `<cwd>/build/`, never `/`. A bare `/` is a target only
as a whole word (`rm -rf /`, `> /`, `cp x /`).

The guard denies when any of these holds, checked in this order:

1. the command is past the parse budget, or the reader failed;
2. the command runs shell code the guard cannot read (opaque code, below):
   always denied;
3. the guard's own paths cannot be told (a broken config) and the command
   writes;
4. the command writes and its cwd, or a dir it changes into, is inside a
   store, the state dir or the config;
5. a resolved target is inside one of those, is a dir holding one
   (`rm -rf ~`), names an `.attest` element (a glob counts when it can match
   one), or is a hard link to a guarded file;
6. an unresolved target's literal prefix ends in a dir inside one, or in a
   dir whose next element the prefix can start (`~/$x` from anywhere, `"$x"`
   from `~`);
7. some write is unresolved and a store is in reach.

A command with no unresolved write and no opaque code is judged by checks 1
to 6 only, target by target. An unresolved write is an unresolved target
word of a writer, unknown program or output redirect; an unknown program's
script-byte operand or fed stdin; a modeled writer behind `xargs` (its
operands come from stdin); a relative write target, or an unknown program,
while a dir change is unresolved; `find`
or `fd` with an action (`-delete`, `-exec`, `-ok`, `-fprint`, `-fls`, `-x`,
`-X`); `sed -f`, or a sed script the guard cannot prove read-only; a shell run
on a script file (`bash x.sh`, `sh < f`); `source` and `.`; and a call made an
unknown program by withdrawal.

A store is in reach of a command when any of these holds:

- R1: a dir change the guard cannot resolve (an opaque `cd` or `pushd`
  operand, `cd -`, `popd`, `pushd +N`, a relative `cd` while `CDPATH` is set
  on the host or by the command, a `cd NAME` while the command binds `NAME` a
  value the guard cannot render (below), a `cd NAME` where `NAME` is a
  variable bash assigns, an unrecognised flag of any wrapper,
  a command word holding `=`, an opaque `-C` of git, make or tar, more than 32
  dirs changed into, or more than 32 option values naming dirs in one call);
- R2: a reach name resolves inside a store, the state dir or the config, or
  to a dir holding one. Reach names are every word anywhere in the command
  (read-only commands, assignment values, option values, redirect targets,
  heredoc and here-string bodies), as rendered or, when unresolved, by their
  literal prefix, plus the path tokens inside them and the glued suffix of a
  short-option cluster. They resolve against the cwd and every dir the
  command changes into, with `..` and symlinks resolved both ways;
- R3: the call's text, as given, printf-decoded and with quotes deleted,
  names `.attest`, `MEMORY.md`, the last two elements of a store root, of the
  state dir or of the config path, or a store's name as a whole path element.

For an unresolved write, every path field of that call's own words counts
toward reach: operands, inline code, option values, a sed script, its
heredoc or here-string body or its stdin script. That includes a bare `/`,
`.` or `..`, and a name that is a hard link to a guarded file. The prose and
regexes in other commands' arguments (read-only commands, data) never count a
bare `/`, unless the allowlist is withdrawn, which turns that command into an
unknown program; a `/` inside a longer word (`A / B`, `build/`) never counts. A cwd
above a store is not reach by itself.

A `cd` or `pushd` operand that is a variable name may also change into that
variable's value, as under `cdable_vars`, whether or not anything visibly sets
it (`shopt`, a shell's `-O`, the host's `BASHOPTS`, however spelled): its
host value and every value the command binds to the name (an assignment, a
prefix, an `env` or `sudo` word, a `for` item, a declare-family operand,
`${NAME:=v}`) are dirs changed into too, so `bash -O cdable_vars -c 'cd HOME;
echo x > notes/MEMORY.md'` is denied. A value bound at run time (`read NAME`)
or appended to is a dir the guard cannot tell (R1). A name bash assigns
itself loses the dir (R1), as its run-time value is not the host's: `_`,
every `BASH*`, `COMP_*` and `READLINE_*` name, and bash's own variables
(`PWD`, `OLDPWD`, `PIPESTATUS`, `DIRSTACK`, `REPLY`, `OPTARG`, …), so
`shopt -s cdable_vars; ls ~/notes; cd _ && rm facts/a.md`, `cd BASH_REMATCH`
and `cd OLDPWD` are denied; so is a name bash assigns only when unset, while
the host leaves it unset. `HOME` is the host's home.

Opaque code is shell code the guard cannot read, so it may compute any path.
It is always denied, wherever the command runs. It is:

- `eval` with a non-literal argument (a literal one is read as a script);
- `bash`, `sh`, `zsh`, `dash`, `ksh` or `ash` with `-c` and a non-literal
  script, reading its script from a pipe, or a heredoc or here-string that is
  not literal (a literal `-c` script, heredoc or here-string is read as a
  nested script);
- a command word that does not render and holds no literal `/`, or is a glob,
  since it may name a builtin or a function;
- `trap` or `alias` text that is not literal, and `env -S` with a non-literal
  value;
- an `env` or `sudo` assignment whose name is not an identifier
  (`BASH_FUNC_cat%%=…`);
- an arithmetic context (`(( ))`, `$(( ))`, `let`, a C-style `for`, the
  integer tests of `[[ ]]`, an array subscript, a substring offset or length)
  that holds a name whose value may be set at run time, or holds a command
  substitution (`read x; (( x ))`,
  `n=$(wc -l <f); echo $((n+1))`, `while read i; do echo $((i+1)); done`).
  A name set only from literal digits and operators stays readable
  (`for i in 1 2 3; do echo $((i*2)); done`, `n=0; ((n++))`). Any setter or
  positional source counts, and a nameref (`declare -n`, `typeset -n`,
  `local -n`; `export -n` un-exports and declares none) makes every name
  suspect;
- another way to run a value as code, while the command sets a name at run
  time: `declare -i`, `${!x}`, `${x@P}`, `[[ -v $x ]]` and `test -v` with a
  non-literal operand, a setter's name operand holding `[` (its subscript is
  arithmetic), an assignment to `PS4`; and always a name operand holding both
  `[` and `$` or a backtick (`read 'a[$(…)]'`);
- text the parser cannot parse. The statements before the error are still
  judged.

A call withdraws the read-only and modeled classes, so that every command in
it is an unknown program, when anywhere in it there is a function
definition, `eval` (literal or not), `alias`, `hash`, `enable`, `source` or
`.`; a name exported to a
command (`NAME=v` prefix, `env NAME=v`, `sudo NAME=v`, `export`,
`declare -x`, `set -a` or `set -o allexport`, and a shell's `-a` or
`-o allexport`, clusters included, read as `set` reads them) or assigned
while the host environment has it, unless
the name is `LANG`, `LANGUAGE`, `LC_*`, `TZ`, `NO_COLOR`, `CLICOLOR`,
`FORCE_COLOR`, `TERM`, `COLUMNS` or `LINES`; an assignment to `PATH`, `HOME`,
`IFS`, `CDPATH`, `PS4`, `ENV`, `BASH_ENV`, `BASH_CMDS`, `BASH_ALIASES`,
`GLOBIGNORE`, `EXECIGNORE`, `BASH_XTRACEFD`, `SHELLOPTS`, `BASHOPTS`,
`POSIXLY_CORRECT`, `BASH_COMPAT`, `BASH_LOADABLES_PATH` or `FUNCNEST`; a
computed setter name; or a target whose basename is or may match (as a glob)
an allowlisted command name, or an environment value with a dir part that
does (`ln -s /bin/rm ~/bin/gh`, `rm -f *`). A target the
guard cannot resolve withdraws only when the part of its last element before
the unresolved part could start an allowlisted program's name (`> "/tmp/out-$n"`
does not, `> "$d"` and `cp x "$d"` do). A `NAME=v` prefix, `env NAME=v` or `sudo NAME=v`
before an external program exports to that program alone, so it withdraws
only that call: `CREW_ID=x crew roster | jq .` leaves `jq` read-only. Before a
builtin, a function or a shell it withdraws the whole command.

An environment value, other than a safe-set name's, is judged like an
operand of an unknown program. A `NAME=v` prefix, `env NAME=v` or
`sudo NAME=v` is one for its own call (a prefix to `:` or `shift` too, which
outlasts them in POSIX mode). Once the command runs an unknown program or
withdraws the allowlist, every value it binds to a name is one for the whole
command, exported or not, as the guard does not track which names end up
exported: an assignment, an array element, a `for` item, `${NAME:=v}` and a
declare-family operand. A value read at run time (`read`, `getopts`,
`printf -v`, `select`) or appended (`NAME+=v`) is an unresolved write under
check 7; one that does not render is too, and is judged by its literal start
like an unknown program's operand that does not render
(`GIT_WORK_TREE=../no$(true)tes git checkout -- .` from a sibling of
`~/notes` is denied); an empty literal start adds nothing past check 7. A
value with a dir part (a `/`) whose basename is or may match an allowlisted
command name withdraws the allowlist, as it may name a file a program writes
and a later call runs (`X=~/bin/cat make; cat ~/notes/MEMORY.md`); a bare
name (`PAGER=cat`) does not. So `GIT_WORK_TREE=~/notes git checkout -- .`,
`DESTDIR=~/notes make install`,
`export GIT_WORK_TREE; for GIT_WORK_TREE in ~/notes; do git checkout -- .; done`
and `bash -a -c 'GIT_WORK_TREE=~/notes; git checkout -- .'` are denied, while
`PAGER=cat make build && grep x ~/notes/MEMORY.md` and
`GH_TOKEN=$(gh auth token) gh pr list` from `~` are not.

The wrappers `env`, `sudo`, `doas`, `nice`, `nohup`, `timeout`, `stdbuf`,
`time`, `command`, `builtin`, `exec` and `xargs` are skipped by a closed flag
table, and a flag that takes a value consumes it. `env -C` and `--chdir`, and
`sudo -D` and `--chdir`, change dir. Every word holding `=` between `env` or
`sudo` and the command is an environment assignment, whatever its name. An
unrecognised flag, a path-invoked wrapper (`./nice`) or a quoted `NAME=value`
in command position makes the call an unknown program over all its words,
with a dir the guard cannot tell. A command word, after the wrappers, that
holds `=` is an unknown program, a write the guard cannot resolve and a dir it
cannot tell; one holding a script byte (a path with a space) is an ordinary
unknown program. An `xargs` over a read-only command is
read-only; over a modeled writer, its literal operands are targets and its
stdin is an unresolved write; over anything else it is an unresolved write.
`xargs -I R` makes every word holding `R` unresolved.

In a tool's input, a JSON document inside a string under a path or unknown
key is read as input, with a budget of its own. Past that budget the call is
denied as past what the guard reads, and the string is no longer judged by a
prefix of its tokens. A content or edit text is a file's own text, not input.

Read-only commands, by name and argument shape. A command is read-only only
when its flags pass its entry: `rg`, `fd`, `find`, `sort`, `uniq` and `awk`
take only listed flags; `git`, `gh`/`glab`, `date` and `diff` take any flag
except the listed ones. A flag that fails makes it an unknown program, and a
long option abbreviation that could also be a disallowed one is disallowed.

- No flag of these writes a file or runs a program, so every flag is
  allowed: `cat head tail wc ls stat readlink realpath basename dirname cmp
  du df md5sum sha1sum sha224sum sha256sum sha384sum sha512sum b2sum cksum
  echo printf test [ true false : pwd which whereis type nl od hexdump
  strings cut tr comm join paste fold rev tac id whoami uname column grep
  egrep fgrep jq`, `priors` and `crew`. `cd`, `pushd` and `popd` are
  read-only dir changes.
- `date` without `-s`/`--set`; `diff` without `-l`/`--paginate`; `sort`
  with its listed flags, where `-o`/`--output` makes it a modeled writer and
  `-T`, `--temporary-directory`, `--compress-program` or an unlisted flag
  makes it an unknown program; `uniq` with its listed flags and one operand
  (a second operand is a target).
- `sed` without `-i`, `--in-place` (any abbreviation or cluster) and `-f`,
  with a script the guard proves read-only: no `w`, `W`, `e`, `r`, `R`, `a`,
  `i`, `c` or `y` command, no `s` flag `w` or `e`, and nothing it does not
  parse. Those letters in a regex, replacement or label do not count.
- `awk`, `gawk`, `mawk` and `nawk` with only `-F`, `--field-separator`, `-v`,
  `--assign` and `--`, and a program holding none of `>`, `|`, `@`, `system`,
  `getline`, `close`, `fflush`.
- `rg`, `fd` and `find`, each with a closed list of flags (rg without
  `--pre`, `--pre-glob`, `--hostname-bin`, `-z`, `--search-zip`; fd without
  `-x`, `--exec`, `-X`, `--exec-batch`, `-l`; find with no action and no
  `-files0-from`). Anything unlisted is an unknown program.
- `git` readers on the closed list of global options and reader
  subcommands, with `-c`, `--exec-path`, `--ext-diff`, `--textconv`, `-O` and
  `--output` handled as running a program or writing; and
  `worktree list`, `worktree repair` and `worktree prune`. `git -C`,
  `--work-tree` and `--git-dir` change dir.
- `gh` and `glab` on a closed list of groups and verbs that write no local
  file: for `gh`, `pr`, `issue`, `repo view|list|create|edit|…` (`create`
  without `--clone`, `-c`, `--source`, `--remote`, `--push`), `api`, `run`,
  `release`, `search`, `workflow`, `label`, `status`, `cache`, `secret`,
  `variable`, `ruleset`, `gist`, `auth status`; for `glab`, `mr`, `issue`,
  `api`, `repo view|list`, `ci`, `release list|view`. Any `-O`, `--output`,
  `-D` or `--dir`, and anything unlisted (an extension, `repo clone`, `pr
  checkout`, `release download`, `alias`, `config`, `codespace`, `browse`),
  is an unknown program.
- the builtins that write no file: `set shift exit return break continue
  export local declare typeset readonly unset read mapfile readarray getopts
  let shopt wait umask sleep`. `mapfile -C` runs a callback and is opaque
  code.

Modeled writers, whose every operand is a target (sources included, so
`cp ~/notes/x /tmp` is denied), and whose option values are too for `touch`,
`truncate`, `cp`, `mv`, `ln`, `install`, `chmod`, `chown` and `chgrp` only: `rm rmdir mkdir touch tee
truncate cp mv ln install chmod chown chgrp`, `dd` (the `of=` and `if=`
values), `sort -o`, the second operand of `uniq`, and `sed -i`, whose file
operands are the targets while its script is judged as for read-only `sed`.
Each has a table of value-taking letters to split short clusters (`-tDIR`,
`-m755`); a letter not in it makes the call an unknown program. A glued
`sed -i` suffix holding `/` or `*` is an unresolved write. Output redirects
(`>`, `>>`, `>|`, `&>`, `<>`, `{n}>`) to a file are targets on any command.
`cp --target-directory=~/x` keeps the `~` literal, as bash does.

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

It is not a sandbox. It does not see:

- a store path the command computes (`${HOME%%h*}`, `dirname`, `base64 -d`, a
  file's content, a script file) through an unresolved write run from a cwd
  outside every store;
- an unknown program's own behaviour (Makefiles, scripts, config it reads), a
  path an interpreter assembles from pieces, an archive or patch unpacked
  from a cwd that holds a store, and an opaque option value that is a dir
  (`--wd=$X`: it is not read as one);
- state an earlier call of a persistent shell left (`PATH`, functions,
  aliases, variables, `CDPATH`, the dir stack, a replaced or shadowed
  allowlisted program, a replaced `gh` or `crew`);
- code that an allowlisted reader runs from config or hook files, written by
  this or an earlier call to a path outside the stores: git hooks and git's
  global `core.hooksPath`, `core.fsmonitor` and `core.pager`, gh's browser, an
  rg config file;
- `gh … --web`, which launches a browser while run as a reader;
- `git add`, `git commit`, `git fetch` and `git push` run in or on a store,
  and any other git reader's writes to git's own index, objects and refs
  (these count as git reading);
- a `cd NAME` into a value `NAME` got outside the command's text (a shell
  profile, an earlier call, a script file a shell runs); a value set through
  `eval` or `source` loses the dir instead, as `CDPATH` may be set too;
- an environment value a program runs as code (`PAGER='sh -c …'`), which is
  judged only for the paths it names;
- a `~` or `~user` a modeled writer or reader leaves literal: the
  home-in-place reading covers unknown programs' operands and environment
  values only;
- a home a program spells with its own token rather than `~` (ssh's `%d` in
  `-S` or `ControlPath`), which is judged as written;
- an unknown-key string longer than 4096 bytes, which is not judged at all,
  so a packed list padded past it passes;
- a variable a shell profile sets, `/proc/<pid>/cwd`, a bind mount, macOS
  Unicode normalization, and a data name in another case on a
  case-insensitive file system;
- an MCP tool named like a read, with no write verb, that writes, and an MCP
  tool that runs a command under a key other than `command` (a
  process-interaction tool's `input`);
- a fact edited through a hard link created after the check, a call
  hookyard does not route, a call after hookyard abstains for a handler that
  crashes or runs past its budget, and a race that swaps a path between the
  check and the write.

It over-denies:

- opaque code anywhere in a command, wherever it runs, a store or not (an
  arithmetic read of a `while read`-fed value, a `$(…)` command word);
- an unresolved write while a store is in reach: any whole-word `/`, `.` or
  `..` that reaches an ancestor of a store, or a store's name anywhere in the
  call's text. This includes an inline program or remote script holding a
  standalone `/`, `.` or `..` (`python3 -c 'print(1 / 2)'`);
- a `claude` prompt, or any unknown program's operand, holding a standalone
  `.`, `/` or `..` from a dir that holds a store (`claude -p "check ."` from
  `~`);
- a shell write to a store fact, and a write that may name a dir holding a store
  (`cmake ..` from a dir directly under one, `foo .` or `cp x .` from `~`);
- an unknown program's operand or cluster suffix that names a store or a dir
  holding one (`foo .` from `~`), including a packed operand after `=`, `:`
  or `,` even when it is a remote path (`host:/…`);
- a command word that holds `=`, which is an unknown program with a dir the
  guard cannot tell, while a program path with a space is an ordinary unknown
  program;
- a read-only command given a flag outside its list, which is judged as an
  unknown program: `awk '$1 > 5'`, `awk '/a|b/'` and `awk '{print "a@b"}'` (any
  `>`, `|` or `@` in an awk program), `sort -T`, `gh pr checkout`;
- an associative-array subscript, which is read as arithmetic;
- a name that no value tracking can resolve: `d=/tmp/x; rm -rf "$d"` is
  unresolved and denied from `~`;
- a command that withdraws the allowlist and names a store path: any exported
  or host-environment name outside the safe set (`GIT_PAGER=cat git log`),
  and any `PATH`, `IFS` or `BASH_ENV` assignment;
- an environment assignment value that names a store or a dir holding one,
  even when the program ignores it (`PATH=~:$PATH x` from anywhere);
- any value the command binds that names a store or a dir holding one, once
  it runs an unknown program or withdraws the allowlist, exported or not
  (`d=~; make`), and one it reads at run time while a store is in reach;
- a quoted `~` or `~user` in an unknown program's operand, a cluster suffix
  or an option value, read with the home in place too (`foo '~/notes/x'`,
  `foo '~alice/notes/x'`, `foo -o'~/notes/x'`);
- an environment value naming a path whose basename is an allowlisted
  command, which withdraws the allowlist even when nothing writes it
  (`X=~/bin/cat make; cat ~/notes/MEMORY.md`);
- a `cd NAME` into a dir literally named like a variable bash assigns
  (`cd PWD`, `cd _`), which loses the dir;
- a `cd NAME` where `NAME` is a host variable, or one the command binds,
  which also changes into its value with `cdable_vars` off (`cd NOTES; rm
  facts/a.md` from anywhere, when `$NOTES` is a store);
- any write while running in a store, and a reader run behind a flagged
  wrapper that the table does not know;
- an unknown tool whose input holds `.`, `..`, `/`, or a word that names a
  store or a dir holding one relative to the cwd (`{"repo":"notes"}` from
  `~`);
- a write tool's text under an unknown key (or a content key when the input
  has no path key) that holds a path token naming a store, a dir holding one
  or an `.attest` element (an issue comment body naming `~/notes/MEMORY.md`),
  and a path-key string any token of which names one (`a,notes` from `~`);
  a store-fact write whose array elements, joined, hold `reviewed`;
- a file tool writing a store fact whose other fields (an instructions or
  description field) hold the word `reviewed`; and, from a cwd inside a
  store, an unknown tool whose input holds the word `reviewed` anywhere;
- an MCP read tool outside the list that names a store's `.git`
  (`read_multiple_files`);
- a pi edit to a fact whose new text holds `reviewed`, `\`, `!` or `*`,
  markdown emphasis included;
- an embedded JSON document in a tool input that is past its budget.

## Store repositories

Each store is its own private git repo. Run `priors lint` there as a
required check: [`store-ci.example.yml`](store-ci.example.yml) is a
starting point.
