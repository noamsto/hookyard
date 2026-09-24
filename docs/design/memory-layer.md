# The memory layer: cross-harness agent memory

**Status:** design proposal. Nothing here is implemented.
**Scope:** a memory store shared by Claude Code, Codex, Cursor and Pi, delivered
through hookyard's existing advisory contract, fed by the crew bus and sessions.
**Related:** [hookyard.md](hookyard.md) (§7 the envelope, §8 native config
emission), [yard-mode.md](../yard-mode.md) (advisory rendering per engine),
`internal/verdict/capability.go` (the per-engine advisory set this design is
bounded by).

Three of the four engines ship no memory at all, and the fourth keeps its own in
a place no other engine can read. This document specifies the store, the two
read paths, and the injection points — and then argues, from measured evidence,
that the store should be plain markdown rather than any of the systems the
memory-startup category sells.

---

## 1. Problem statement

| engine | memory today | where | readable by the others |
| --- | --- | --- | --- |
| Claude Code | auto memory (model-written) | `~/.claude/projects/<project>/memory/` | no |
| Pi | none | — | — |
| Codex | none | — | — |
| Cursor | none | — | — |

Claude Code's auto memory is a real layer, not a stub. Verified on this machine:
62 topic files across 11 project directories, each a markdown file with
`name`/`description` frontmatter and a per-directory `MEMORY.md` pointer index.
Per Anthropic's docs and a subsequent bug report about the undocumented
truncation error, the shape is:

- `MEMORY.md` is a **pointer index**, one line per topic file, injected into
  every session, truncated at **the first 200 lines or 25 KB, whichever comes
  first**;
- topic files are *not* loaded until read, and are surfaced by the harness's own
  per-turn retrieval;
- scope is **per repository**, shared across worktrees;
- a background consolidation pass runs after **24 h and 5 sessions**.

That is a good design, and this spec borrows its shape deliberately (§4.1). What
it does not have is reach. A memory written by Claude on `dispatcher` is
invisible to a `pi` session in the same worktree, invisible to a Codex worker,
and invisible to the dispatcher that could act on it — while crew workers, who
run with `--strict-mcp-config` and therefore no MCP tool that an embedding store
would expose, are exactly the processes that keep rediscovering the same facts.
A concrete example already sitting in that corpus: *"Claude haiku dispatch
workers stop on a permission prompt for every shell command with `$`-expansion;
use sonnet for unattended trivial work."* The dispatcher judge, which picks
engine and model on every dispatch, has never seen it. It is the same class of
fact as the `ratings.jsonl` outcomes the judge also does not read.

Two failure modes follow, and they are the two this design is measured against:

1. **Rediscovery.** A fact learned in one repo, or by one engine, is relearned
   in the next.
2. **Unreadable evidence.** `~/.local/share/crew/ratings.jsonl` accumulates
   `outcome`, `rework_count`, `review_high`, `blocked_count` per run, keyed by
   engine, model, tier and repo — and nothing consults it at dispatch time.

---

## 2. Prior art evaluated

The question asked of every candidate was not "does it work" but **"what does it
buy over a directory of markdown files and `rg`?"** That is the right bar here
because plain files clear every constraint in §3 by construction, so a
structured store has to earn its place by winning on recall.

### 2.1 What the measurements say

Two independent comparisons exist, and they disagree with each other in the
usual way — so read both.

**Letta's own benchmark** *(August 2025, `Benchmarking AI Agent Memory: Is a
Filesystem All You Need?`, LoCoMo, GPT-4o mini)* compared two systems:

| system | LoCoMo |
| --- | ---: |
| Letta — filesystem + search tools | **74.0 %** |
| Mem0 — graph-based memory | 68.5 % |

Two systems, one task type, so this is motivation rather than proof. The stated
mechanism is the interesting part: *"agents today are highly effective at using
tools, especially those likely to have been in their training data (such as
filesystem operations)"* — a model already knows how to grep, open and rephrase,
so a purpose-built index has to beat a competent agent holding that repertoire.

**A controlled file-vs-structured study** *(The Shapes of Agent Memory, 2026)*
ran the two architectures head to head on the same reader. It is the more useful
of the two because it holds the model constant and reports the category
breakdown, which is where the edge actually lives:

| category | file-based (index + grep) | structured (embed + ranked) |
| --- | ---: | ---: |
| temporal reasoning | 41 % | **80 %** |
| multi-session aggregation | 33 % | **61 %** |
| single-session | — | — |

The multi-session row is the mechanism, in the author's own words: file-based
answers are assembled from facts mentioned across conversations, and *"a literal
search over a deliberately small index is the wrong tool for that. If the
joining fact sits in a topic file the model never thought to grep, it is simply
gone, and the model, to its credit, usually says it does not know rather than
inventing an answer."* Ranked retrieval over an unbounded store does not have
that failure mode, because the fact was saved whether or not anyone predicted it
would matter, and similarity rather than a filename brings it back.

Three further findings from the same study bound how much that edge is worth:

- **Structure beyond ranked retrieval buys nothing measurable.** A
  place-plus-time hybrid was statistically indistinguishable from a flat vector
  index (+0.3 pts, paired CI95 [−1.8, +2.4]) on LoCoMo. On LongMemEval-M's long
  haystacks the same two stores separated by 15 points (0.750 vs 0.600,
  p = 0.008) — so the gap opens with history length, and *which* structure does
  not.
- **Distilled graphs lose to raw text.** Both graph rows handed the reader
  LLM-distilled facts and entity summaries and *"trail every raw-turn store, and
  the strongest graph row loses 3.3 points to the flat index while spending six
  times its context."* Extraction on the write path costs accuracy, not just
  money.
- **Consolidation does not pay at small scale.** The nightly merge pass, run at
  ~50 sessions per history, *"merged real duplicates and bought no accuracy"*;
  it *"pays most on long histories read by weak models, least on short ones read
  by strong ones."*

The study's own headline is the honest summary, and it should temper any
architectural enthusiasm here: *no single benchmark ranks memory systems,* and
swapping the model that reads and judges the memory moved the score further than
swapping the memory did.

**What file-based pays.** Not free either. In the same study, LLM curation on
the write path cost ~246k model tokens and ~35 minutes of wall clock per history
against ~5 minutes for embedding with no model in the loop — roughly sevenfold —
and the iterative read path (index → grep → read → grep again) truncated on
**20 of 144** answers against the structured arm's 3, because it asks the model
to generate more, across more rounds, with more chances to be cut off.

Both of those numbers belong to *LLM-curated* files specifically: §2.3 shows the
write-side cost is a property of who sits on the write path, not of markdown.

### 2.2 The systems, and what each is actually betting on

| system | shape | what it gives you | why it is not the answer here |
| --- | --- | --- | --- |
| **Claude Code auto memory** | index + topic files, model writes, no embedder | the shape this spec adopts; already runs in production here | per-repo, Claude-owned path, invisible to three engines |
| **Pi-memory** (jayzeng) | markdown + `qmd` BM25/vector, injected in `before_agent_start` | the closest prior implementation to this design, for Pi specifically | a Pi extension, so it reaches one engine; no cross-engine story |
| **OpenClaw `memory-core`** | `MEMORY.md` + dated logs + 3-phase nightly "dreaming" | usage-gated promotion into the always-loaded index | Node/OpenClaw runtime; the consolidation pass is deferred here (§8) |
| **Cline Memory Bank / Cursor / Windsurf memory** | instruction-file family, hand-maintained or auto-written | proves the file pattern is the shipped default | not agent-agnostic, not versioned, not one corpus |
| **`server-memory`** (MCP reference) | knowledge graph, `create_entities`/`create_relations`/`search_nodes` | typed entities, no schema drift | **MCP-only** — unreachable from a `--strict-mcp-config` worker (§3) |
| **basic-memory** | markdown source of truth + derived SQLite FTS5, `bm tool search-notes/read-note/write-note` CLI | ranked recall *and* a CLI, so it survives the no-MCP rule; Obsidian can open its folder; AGPL-3.0, Python 3.12+ | a second runtime and a second sync story for a corpus of tens of files; the *architecture* is worth copying, not the dependency (§4.5) |
| **Inkwell** | MCP, markdown source of truth, typed `kb://` graph, bi-temporal superseding | the best worked example of supersession-in-markdown | MCP-only delivery |
| **mem0** | LLM fact extraction → vector store + entity hints | automatic capture, no model on the read path | write path *is* an LLM; opaque store; no human curation surface; measured behind a filesystem arm |
| **Letta** | agent-curated tiers, files/blocks | the strongest published case *for* files | a full agent runtime, not a memory layer for a fleet of existing engines |
| **Zep / Graphiti** | bi-temporal knowledge graph, validity windows | temporal invalidation as a first-class primitive | self-hosted needs Neo4j; and the study measures its distilled output losing to raw text |
| **Cognee** | entity-extraction pipeline | entity graph construction | same extraction-on-write penalty, plus a pipeline to operate |
| **bi-temporal family** (Mubit, SurrealDB tri-temporal, Agent Memory Atlas) | valid-time × system-time × known-time | answers "what did we believe in April" separately from "what happened in April" | a database, for a problem git history plus two frontmatter fields already covers (§4.7) |

Two conclusions fall out, and they shape everything below.

**The edge is real, specific, and not what the category advertises.** It is not
the knowledge graph (indistinguishable from a flat index, and its distilled form
*loses*), and it is not consolidation (bought no accuracy at the measured
scale). It is **ranked recall over something you did not predict you would need**
— worth 39 points on temporal reasoning and 28 on multi-session aggregation, and
worth more as history grows. A markdown store's job is therefore to keep the
door open to that capability *without* adopting an embedder today.

**Every structured option is delivered by a mechanism this fleet cannot use.**
MCP servers are invisible to crew workers; embeddings need a service; graphs
need a database. The delivery constraint (§3) is more binding than the recall
constraint, and it points the same way the measurements do.

### 2.3 `recall` (raiyanyahya) — the closest shipped tool, and a different half

An earlier revision of this document named the tool specified in §4 `recall`.
That name is taken, by a project worth reading on its own merits. `recall`
(MIT, ~750★, v0.4.0) is fully-local project memory for Claude Code with opt-in
opencode support. Verified from source rather than its README:

| | |
| --- | --- |
| capture | Claude Code `Stop` / `SessionEnd` hooks append new turns to `.recall/history.md`, incrementally |
| digest | `scripts/summarizer.py` — **TF-IDF + TextRank extractive summarization**, vendored, stdlib-only, numpy as an optional accelerator |
| surface | a `SessionStart` hook (matcher `startup\|resume\|clear`) prints `.recall/context.md`: goal, files touched, commands run, where we left off, `git diff --stat` |
| scope | **per project**, in-repo `.recall/`, commit or gitignore |
| safety | best-effort secret redaction before writing; injected context explicitly **fenced as untrusted reference data** |
| engines | Claude Code first-class, opencode opt-in via a generated plugin; Codex, Cursor and Pi unsupported |

Three things it changes about this document, and one it does not.

**It is a counterweight to §2.1's write-cost claim.** That section reports
file-based memory paying ~246k model tokens and ~35 minutes per history to
curate. Recall shows the cost is an artifact of *who is on the write path*, not
of files: a classical extractive summarizer produces a usable digest for **zero
model tokens**, offline, with no key and no network. The measured penalty
applies to LLM-curated capture, not to file-based memory as such.

**It independently validates §4.4's attribution rule.** Recall fences recalled
content as data and tells the reader to disregard instructions inside it — the
same conclusion the Pi bridge reached from the other direction, where an
unattributed block made a probe model call it *"exactly the shape of an
injection attempt"* (`docs/design/fixtures/pi-pre-tool-advisory/`). Two
independent implementations agreeing that recalled text is an injection surface
is about as strong as this evidence gets. Recall's opencode path notably *omits*
the fencing — its README says so — which is the failure mode rather than the
design.

**Its packaging is hookyard build-mode's target**, and its adapter seam is one
module from another engine: `.claude-plugin/plugin.json` plus a four-hook
`hooks/hooks.json`, and a `--harness {claude,opencode}` flag dispatching to a
per-harness `collect_events` module. That is the cheap path to session continuity
on Pi or Codex, and it is not what §4 specifies.

**What it does not do is the thing §1 is about.** Recall holds a digest of what
a session did, keyed to one project directory. It does not hold curated, durable,
cross-repo facts; it has no retrieval over them (the digest is loaded whole); it
has no staleness or supersession model; and its corpus is per-project by
construction, so *"claude haiku workers stall on prompts"* — true of dispatcher,
applicable to any crew — has no home in it. Recall answers *"where were we?"*;
§4 answers *"what is true about this tooling, and should we act on it?"*

---

## 3. Requirements

Each requirement is stated with the evidence that makes it binding, because
several of them rule out otherwise-attractive designs.

| # | requirement | evidence |
| --- | --- | --- |
| R1 | **Readable and writable with ordinary file tools** (`read`, `grep`, shell) | crew workers run `--strict-mcp-config` with zero MCP servers (`home/ai/claude-code/mcp-profiles.nix`). Anything MCP-shaped is invisible to exactly the fleet that needs memory most |
| R2 | **Reaches all four engines** | hookyard's own settled boundary: Codex has **no advisory channel on any event**, and Cursor's advisory rides only a rendered permission (`internal/verdict/capability.go`). So the store must be readable by Codex and Cursor as *files*; injection can only ever cover Claude and Pi |
| R3 | **Works on a host with no GUI** | `halo` runs `desktop.mode = "none"`. The `obsidian-cli` binary is a Unix-socket client to a *running* Obsidian app (`$XDG_RUNTIME_DIR/.obsidian-cli.sock`; verified on this machine: *"The CLI is unable to find Obsidian"*), so it is not an agent interface |
| R4 | **Survives concurrent writers on two or more machines** | agents run on `tp-g5`/`tp-g6` and `mbp-m4-pro`, sometimes simultaneously in worktrees off one repo |
| R5 | **Human-curatable and retireable** | agent memory's dominant real failure is a stale fact that keeps being injected. It needs a surface for review, correction, and deletion |
| R6 | **Durable, inspectable, versioned, no lock-in** | the corpus is the long-lived asset; the tooling around it is not |
| R7 | **Bounded cost and latency per turn** | injection runs on a session-start and prompt-submit path; `hookyard` already caps a handler at 4300 ms, and a memory lookup that blocks a turn is worse than a memory that is missing |
| R8 | **Degrades to nothing** | a missing binary, a missing index, or a timed-out retrieval must leave the session exactly as it was, never half-configured. Same fail-open discipline as the Pi bridge's `askRouter` |

Non-requirements, so nobody designs for them: sub-100 ms semantic search, recall
over raw conversation transcripts, automatic capture of every turn.

---

## 4. Design

### 4.1 The store

Plain markdown, **one fact per file**, in a pointer-index layout — the shape
Claude Code's auto memory already proved in production, adopted here so the
62-file corpus can migrate by moving files rather than transforming them.

```
<store>/
  MEMORY.md                     # pointer index: one line per fact, injected at session start
  INDEX.md → MEMORY.md          # (no alias in v0; named MEMORY.md for Claude parity)
  dispatcher/
    haiku-workers-stall-on-prompts.md
    auto-merge-skips-ci.md
  hookyard/
    pi-bridge-advisory-channel.md
  _global/
    wt-post-switch-owns-navigation.md
  _archive/                     # superseded facts, moved not deleted
```

Frontmatter is a superset of what Claude already writes, so the existing corpus
stays readable by both the engine that wrote it and the new tooling:

```markdown
---
name: haiku-workers-stall-on-prompts
description: Claude haiku dispatch workers stop on a permission prompt for every
  shell command with $-expansion; use sonnet for unattended trivial work
metadata:
  node_type: memory
  type: project          # project | reference | preference | decision | lesson
  scope: repo            # repo | global
  repos: [dispatcher]
  engines: [claude]      # which engines the fact was observed on
  valid_from: 2026-09-16
  superseded_by: null    # a filename, once this stops being true
  verified: 2026-09-16   # when a human or agent last confirmed it still holds
  originSessionId: 98a49727-b288-4f1c-9e6c-e7453ce01ef6
  modified: 2026-09-16T13:42:44.668Z
---

Observed 2026-09-16 (crew 1789561716-857282, PR #205): ...

**Why:** ...
**How to apply:** ...
```

Two conventions carry meaning without a schema engine, following Pi-memory's
"tags are content conventions, not enforced metadata" and the markdown-vault
graph-engineering guidance (typed edges + supersedes chains + a lint + a recall
budget are the 20 % a vault lacks over a plain linked graph):

- **Typed edges in the body**: `supersedes:`, `contradicts:`, `applies-to:`,
  taking `[[wiki-link]]` values. A graph *filter* over frontmatter — not a graph
  database.
- **`type` is a closed set.** The lint (§4.6) rejects a sixth value, which keeps
  retrieval filters honest.

`MEMORY.md` is a pointer index in Claude's exact sense: one line per fact,
`- [Title](path) — one-line description`, capped at the same 200-line/25 KB
budget so the two consumers agree on the ceiling. It is generated, not
hand-written; the lint regenerates and diffs it.

**Why one fact per file.** It is what makes R4 nearly free: two agents editing
`MEMORY.md` concurrently would conflict on every write, while two agents writing
two different fact files do not touch the same bytes. Every sync substrate
becomes adequate when writers do not share files — which is what makes the
choice in §4.8 a preference rather than a commitment.

### 4.2 Scoping and resolution

- **repo scope** — a fact that is only true in one repository (`repos: [x]`).
- **global scope** — a fact about the machine, the tooling, or the person
  (`scope: global`), which is where cross-repo lessons live. The
  `haiku-workers-stall` fact is filed under `dispatcher/` but is a dispatcher
  *policy*, so it illustrates the boundary: the fact is repo-scoped, its
  applicability is not.

Resolution is a filter, not a hierarchy: for a session in repo `R`, retrieval
considers `scope == global` **or** `R ∈ repos`. No precedence rules, because
precedence rules are where instruction files go wrong.

### 4.3 Write path

Two writers, one corpus. Neither is "every turn".

**a. Session reflection (agent-initiated).** The agent gains three file
operations, not a new tool surface:

```
priors add  --type lesson --scope repo --repo dispatcher --stdin
priors list [--repo R] [--type T] [--stale]
priors show <name>
```

`priors add` validates frontmatter, refuses a duplicate `name`, regenerates the
index, and exits non-zero on a lint failure. It is a *writer's* convenience over
`$EDITOR`, not a separate store: an agent may equally write the file directly,
and the lint will accept it. This is deliberate — a store that can only be
written through its own tool is a store that fails R8.

Capture policy: the model decides after a turn, as in file-based designs
generally. The known cost is the ~246k-token-per-history write bill measured in
§2.1 — which applies to *mining every session*, and does not apply here, because
this corpus is written a few facts at a time by an agent already in the loop.

**b. Dispatcher distillation (mechanical).** On `crew reap`, a run's outcome is
already written to `~/.local/share/crew/ratings.jsonl`. A distillation step may
propose one fact per discovered regularity (e.g. engine/model/tier combinations
with `rework_count` above a threshold *and* a consistent cause) and write it as
a `type: project` draft marked `confidence: proposed`. Proposals are not
injected until a human or a later run confirms them (§4.6) — the failure mode
worth avoiding is an unverified statistical claim becoming a standing
instruction.

### 4.4 Read path: three tiers, one budget

Adopted in outline from Pi-memory's selective-injection design, which replaced
dump-everything with search-relevant-and-inject and documented the priority
ordering explicitly. Budgets are per source and total; trimming happens from the
lowest priority upward.

| tier | trigger | source | budget | truncation |
| --- | --- | --- | --- | --- |
| **1** | `session_start` | `MEMORY.md` pointer index (repo + global) | 4 K chars | from the middle |
| **2** | `prompt_submit` | ranked `priors search "<prompt>"` → top 3 | 2.5 K chars | from the start |
| **3** | any time | agent runs `priors search` / `rg` / reads a file | unbounded | — |
| | | **total injected** | **8 K chars** | |

Tier 1 is cheap and unconditional, and it is what makes the system work when
everything else fails — a session with a broken retrieval backend still sees the
index. Tier 2 is where the measured edge lives (§2.1), and it is the tier that
*does not work today* (§5, hookyard). Tier 3 is the escape hatch, and is the
reason R1 matters: the agent can always grep.

Tier 2 must be bounded and must fail open:

- hard timeout **800 ms** (well inside hookyard's 4300 ms handler cap, and
  ~26× Pi-memory's 30 ms BM25 target);
- on timeout, empty result, missing index, or any non-zero exit: **inject
  nothing**, log the miss to the event record, and continue. A retrieval that
  cannot answer is indistinguishable from a store that has nothing;
- the injected block is attributed in its own text, exactly as the Pi bridge
  already does with `[hookyard advisory] ` — an unattributed block reaches the
  model looking like a prompt injection, which is a finding from hookyard's own
  `docs/design/fixtures/pi-pre-tool-advisory/` probe.

### 4.5 Retrieval backend: one interface, three implementations

The store's retrieval is behind one contract so that the §2.1 conclusion — "keep
the door open to ranked recall without adopting an embedder today" — is a
configuration change rather than a rewrite:

| version | backend | what it can match | cost | when |
| --- | --- | --- | --- | --- |
| **v0** | `rg` + frontmatter filters | literal terms, tags, filenames | ~5 ms, no dependency | ships with the store |
| **v1** | SQLite **FTS5** over the same files, derived and rebuildable | ranked BM25, stemming, phrase | ~30 ms, one C binary, no service | when `rg` misses become noticeable |
| **v2** | embeddings over the same files, same index shape | paraphrase ("what DB do we use?" vs "Chose PostgreSQL") | ~2 s, a model and a key | only if v1's misses are measured |

Two rules hold the door open. **Files stay authoritative**: the index is derived,
deleted and rebuilt without loss, exactly as basic-memory specifies and as
Pi-memory's "files are the index" principle requires. And **the backend never
appears in the store's schema** — no embedding column that a file cannot carry,
no chunk boundaries to maintain.

This is where established solutions were genuinely consulted rather than
dismissed: basic-memory's derived-SQLite-FTS5 architecture is the right v1, and
`qmd` (literal + vector search) is Pi-memory's v0/v2. Neither is adopted as a
dependency in v0, because a corpus of tens of hand-curated facts is served
exactly by `rg` and the measured edge only opens as history grows.

### 4.6 Hygiene: the lint and the retirement rule

Agent memory's dominant failure is not a missing fact, it is a stale one
injected with the same confidence as a fresh one. Three mechanical rules:

1. **Supersede, do not delete.** A fact that stops being true gets
   `superseded_by: <name>` and is moved to `_archive/`. It stops being injected
   by tier 1/2 and remains readable, because "we used to believe X because Y" is
   often the more useful fact. This is the bi-temporal edge (§2.2) at the cost
   of two frontmatter fields — `valid_from` is valid time, git history is system
   time, and a full tri-temporal database is not warranted for a corpus this
   size.
2. **`verified` decays.** A fact whose `verified` date exceeds a threshold (v0:
   90 days) is flagged by `priors list --stale`, and tier 2 down-ranks it rather
   than hiding it. Staleness is visible, not silently enforced.
3. **The lint** rejects: unknown `type`, missing `name`/`description`, a
   duplicate `name`, a `superseded_by` pointing nowhere, a `repos` entry naming
   no repo, a dangling `[[wiki-link]]`, and an index out of sync with the
   directory. It runs in the store's own pre-commit and in the write path, so a
   malformed fact cannot be injected.

Consolidation — OpenClaw's nightly "dreaming", or mem0's dedup — is
**deliberately deferred**, and the deferral is evidence-based rather than
lazy: at the measured scale it merged real duplicates and bought no accuracy
(§2.1). When it is eventually needed, the policy worth copying is OpenClaw's
gate rather than a similarity threshold: an item earns promotion into the
always-loaded index by *being used*, clearing a recall-count threshold across
distinct queries — a signal neither the write nor the read path can see.

### 4.7 Delivery: hookyard's advisory contract, unchanged

The memory layer does **not** add a hook mechanism. It is a hookyard `exec`
handler — the contract hookyard already has — and it is therefore the same
wiring story as every other handler, with the per-engine render already written
and tested.

```
session_start   → priors index      → advisory → tier 1
prompt_submit   → priors search     → advisory → tier 2
(pre_tool / post_tool reserved for future targeted recall)
```

What each engine can actually receive, read off `internal/verdict/capability.go`:

| engine | `session_start` | `prompt_submit` | file reads |
| --- | --- | --- | --- |
| Claude Code | ✅ advisory | ⚠️ **channel exists upstream, not rendered yet** | ✅ |
| Pi | ✅ advisory (queued → `before_agent_start`) | ⚠️ **reply currently discarded** | ✅ |
| Cursor | ⚠️ only alongside a rendered permission | ❌ | ✅ |
| Codex | ❌ **no advisory channel exists** | ❌ | ✅ |

So the reach matrix is the requirement R2 in practice: **Codex and Cursor get the
store as files, and that is the honest ceiling.** Codex is the interesting case —
it reads `AGENTS.md` natively, and a repo can therefore point Codex at the index
through the instruction-file path with no hook at all. That is why the store must
be file-native rather than hook-native: the hook is an optimization for two
engines, the files are the mechanism for all four.

Two gaps must be closed in **hookyard**, and they are the only hookyard changes
this design needs:

1. **`prompt_submit` has no advisory slot on any engine.** `HasAdvisorySlot`
   allows only `pre_tool`, `session_start` and `post_tool` for Claude and Pi.
   Upstream, the channel exists on both — Claude's `UserPromptSubmit` returns
   `additionalContext`, and Pi's `before_agent_start` fires per prompt with
   `event.prompt` and returns `{message}`. Tier 2 does not exist until this
   slot is added.
2. **The Pi bridge discards the `input` reply.** Its `input` handler returns
   `undefined` unconditionally, and `before_agent_start` is registered only to
   flush a single `session_start`-queued advisory ("one slot, latest wins"). A
   per-prompt advisory needs that registration to become a real handler rather
   than a flush.

Per hookyard's conventions, a Claude-side advisory slot on `prompt_submit`
requires a captured payload fixture under
`docs/design/fixtures/hook-payloads/` before it can be claimed as supported —
the same evidentiary bar `pre_tool`'s advisory cleared via aeye's
`diagram-guidance.sh` running against real Claude Code.

### 4.8 Sync: git, with the viewer optional

The store is a private git repository, cloned to the same path on every host.
Agents are already fluent in git; it provides real three-way merge with
ancestry, an audit trail, and a headless path that works on `halo` and in CI.
Because §4.1 puts one fact per file, merge conflicts are rare rather than
structural.

The comparison, for the record, since it was asked directly:

| transport | merge quality | history | headless | mobile | cost |
| --- | --- | --- | --- | --- | --- |
| **private git** | 3-way, ancestry | full | ✅ | via Working Copy / GitSync | free |
| Obsidian Sync | diff-match-patch for `.md`; conflict files if opted in; open class of spurious conflicts from background writers | 1 mo (Standard) | only via `ob`, and `ob` is a *sync* client, not an interface | ✅ native | $8/mo (Plus: 10 vaults) |
| Syncthing | `.sync-conflict-<date>-<device>.md` copies | opt-in versioning | ✅ | ✅ | free |

**Obsidian is adopted as a viewer and nothing else.** Point it at the checkout:
graph view, backlinks, and — the genuinely useful part — **Bases**, which gives a
table over the frontmatter (`type`, `repos`, `verified`, `superseded_by`) and is
the best available surface for the §4.6 retirement job. That is a real advantage
over `rg`, and it is why Obsidian appears in this design at all. It is not the
medium: `obsidian-cli` needs the app running (R3), `ob` moves bytes but cannot be
queried, and its sync's markdown merge is a text merge with no ancestry, which is
the wrong tool for a corpus two agents write to concurrently. Running git *and*
Sync over one directory is explicitly rejected.

Prior context worth carrying: Obsidian was installed in `nix-config` on Apr 16
2026 with two vaults (`personal`, `work`), the `sync`/`bases`/`properties` core
plugins enabled, and an `obsidian-mcp` server; the MCP server was removed
Jul 1 2026 and `obsidian.nvim` Jun 30 2026, and as of now the vaults exist on
`tp-g6` with **zero markdown files** in them. The viewer earns its place here
only if agent-written content is what fills it.

---

## 5. Wiring

The store needs no per-engine registration. This is worth stating plainly,
because it is the reason the component count stays at one: hookyard is the only
thing registered with each engine, and it already is. `priors` is invoked *by*
hookyard and by the agent's own shell.

`priors` is a working name, chosen only because `recall` was taken by the
project evaluated in §2.3. It is unclaimed in this problem space — the `priors`
packages on npm and PyPI are unrelated, and the top GitHub matches are NeRF and
diffusion research — but the name carries no design weight and is a one-line
change.

| repo | change | why |
| --- | --- | --- |
| **hookyard** | `prompt_submit` added to `HasAdvisorySlot` for Claude and Pi; Pi bridge's `input` reply delivered (not discarded); `before_agent_start` registration made a real per-prompt handler | the only hookyard changes required; tier 2 is impossible without them (R2, §4.7) |
| **hookyard** | captured `prompt_submit` advisory payload fixtures per engine, per its own evidentiary convention | a claimed-advisory engine with no fixture is a claim, not a capability |
| **`priors`** (new binary, own repo) | `add` / `list` / `show` / `search` / `lint` / `index`; store path from config; v0 `rg` backend | the store needs a release cadence independent of the router; §4.4's fail-open contract lives here |
| **nix-config** | install `priors`; a `hookyard.json` manifest entry wiring `session_start` + `prompt_submit` to it; clone the store repo on every host incl. `halo` and `mbp`; `AGENTS.md` include line for Codex/Cursor/Pi; optional Obsidian `programs.obsidian.vaults` entry | one manifest, four engines — the pattern `programs.hookyard.manifests` already exists for |
| **dispatcher** | `crew reap` → distillation proposals (§4.3b); the judge consults `priors search` before choosing tier/engine/model | closes failure mode 2 — the judge currently decides from a static table while `ratings.jsonl` holds the evidence |
| **nix-config** | worker MCP profile unchanged (zero servers) | memory must not be the reason a worker grows an MCP dependency (R1) |

Ordering matters: the hookyard gaps are prerequisites for tier 2, but the store
and tier 1 deliver value with no hookyard change at all — a git repo, an index,
and an `AGENTS.md` include line reach all four engines on day one.

---

## 6. What would change the recommendation

Stated so the design can be falsified rather than defended:

- **The corpus grows past a few hundred facts across many domains.** The
  measured gap opens with history length; at that point v1 (FTS5) is not enough
  and v2 (embeddings) becomes the honest answer.
- **Queries become conversational rather than named.** `rg` and BM25 both fail
  on paraphrase; the study's multi-session failure mode is precisely this.
- **A second machine starts writing claims the first never validates.** Then
  §4.6's lint stops being hygiene and becomes the load-bearing component.
- **Injection starts costing measurable tokens per turn.** The 8 K budget is
  the lever, and tier 1 is the first thing to shrink.

---

## 7. Verification

**Deterministic** (no LLM, no network, temp directories):

- frontmatter round-trip: every field survives write → read → write;
- `priors search` filters: `scope`/`repos`/`type`/`superseded_by`, and that a
  superseded fact is absent from tier 1 and 2 output;
- index generation is idempotent, and the lint catches each rejected class in
  §4.6 (one test per rule);
- **fail-open**: missing binary, missing index, unreadable file, and a
  deliberately hung search each yield empty injection and exit 0;
- budget: each tier's truncation order, asserted positionally, and the total cap.

**Injection end-to-end** (the test that actually matters, adapted from
Pi-memory's test 8): write a fact, start a *new* session, and ask the question
without instructing the agent to search. If it answers, tier 2 worked. Repeated
per engine that has an advisory slot, with the Codex case asserted as a
**negative** test — the fact is reachable by file read, and *not* injected.

**Cost and latency**, because §4.4 has a deadline: p50/p95 of `priors search` at
10, 100 and 1000 facts, against the 800 ms budget, so the v0→v1 trigger is a
number rather than a feeling.

**Recall A/B**, ported from Pi-memory's eval shape: a corpus built from the real
62 migrated facts, ~15 questions across source types, run with injection on and
off. The expected result — and the reason to run it — is that the delta
concentrates in facts that are *not* in the tier-1 index window, which is the
only thing that would justify tier 2's complexity.

---

## 8. Non-goals (v0)

- no vector database, no embeddings, no external service;
- no knowledge-graph database;
- no per-turn fact extraction;
- no background consolidation / "dreaming" (§4.6 defers it on measured grounds);
- no Obsidian Sync, no vault-as-store;
- no MCP server — explicitly, because a worker cannot see one;
- no conversation-transcript digest. That is §2.3's territory, and `recall`
  already does it for Claude Code and opencode at zero model tokens; the cheaper
  move for the other two engines is an adapter on its `--harness` seam, not a
  second implementation here.

---

## 9. Open decisions

These block implementation and are the author's calls, not the design's:

1. **Store placement.** One global repo cloned everywhere (recommended: matches
   the pointer-index shape and lets a fact be found from any cwd) versus
   per-repo `.agents/memory/` (matches Claude's existing per-repo scoping, but
   loses every cross-repo lesson).
2. **Migrate or start fresh.** 62 existing facts exist. Migrating means adding
   `scope`/`repos`/`verified` frontmatter to files Claude wrote.
3. **Two writers, one corpus.** If Claude Code keeps writing to
   `~/.claude/projects/<project>/memory/` while this store grows, the corpus
   forks. The options are: symlink Claude's path at the store, stop Claude's
   auto-memory, or accept the fork and consolidate at reap time. **This is the
   one with a real cost either way** and it should be decided before any file
   moves.
4. **`priors` as its own repo, or a `hookyard priors` subcommand.** The design
   argues for a separate binary (§5) — hookyard's thin router is a stated
   property and the store has a different schema cadence — but a subcommand
   would mean one binary installed machine-wide instead of two.
5. **Whether to adopt `recall` at all (§2.3).** Three shapes: use it as-is for
   session continuity and build §4 only for durable facts (recommended);
   contribute a `pi`/`codex` adapter to it and skip §4 entirely — much cheaper,
   but it does not close failure mode 2, because the dispatcher still never
   learns from its own outcomes; or both. Note the collision if both ship: they
   want the same `session_start` budget, so §4.4's tier 1 and recall's
   `context.md` would compete for it, and the index should shrink or be dropped.

---

## 10. Workstreams, in dependency order

| # | workstream | repo | delivers |
| --- | --- | --- | --- |
| 1 | store layout, `priors` v0 (`add`/`list`/`search`/`lint`), git repo, index generation | `priors` | tier 1 + tier 3 on all four engines |
| 2 | nix-config wiring: install, clone on every host, `AGENTS.md` include line | nix-config | reach without any hookyard change |
| 3 | migrate or seed the corpus (decision 2/3) | — | content to actually retrieve |
| 4 | hookyard: `prompt_submit` advisory slot + Pi bridge `input` reply + fixtures | hookyard | tier 2 on Claude and Pi |
| 5 | dispatcher: judge consults `priors`; `crew reap` distillation proposals | dispatcher | closes failure mode 2 |
| 6 | optional: Obsidian as a viewer over the checkout; Bases table for the stale sweep | nix-config | §4.6 curation, if it earns it |

Workstreams 1–3 are worth doing regardless of how the rest lands, and 4 is
independent of 5.
