# hookyard: a cross-engine hook registry and router

Status: design proposal. No implementation exists yet. hookyard is a
standalone tool — its own binary, its own repo, its own release surface.
Claude Code, Codex, and Cursor each declare hooks in their own native format;
aeye, lazytmux, dispatcher, and nix-config each wire behavior into those
formats independently today. This document specifies the registry and router
that replaces that duplication, and the transport by which any subscriber —
houston among them — can watch the resulting event stream. This document
supersedes `hook-registry.md` in full; the disposition of each of that
document's eleven sections is recorded below.

## Supersession of `hook-registry.md`

`hook-registry.md` (841 lines) is superseded outright by this document. It
was substantively correct except for one settled question it answered
wrongly: it recommended building the registry and router inside houston. The
user rejected that. Every section of the prior document is accounted for
below as carried unchanged, carried reworked, or dropped with a stated
reason; nothing is lost by silence.

Section titles below are the prior document's own. The dispositions were
checked against the assembled text of this document, not against a plan.

| Old § | Old title | Disposition | New home | Reason |
|---|---|---|---|---|
| 1 | Problem statement | Carried, reworked | §1 | This pass's own drift counts and writer counts replace unreproducible or superseded figures; final paragraph rewritten to stop building toward a houston-hosted conclusion |
| 2 | Prior art evaluated | Carried, reworked | §2 | HookBus verdict, patent paragraph, and survey carried whole; the Python-floor comparison is re-measured against this pass's own Go shape rather than houston's inherited figure |
| 3 | Recommendation: build it in houston | **Dropped, replaced** | §3 | The houston-hosting recommendation is a settled-wrong answer, not a hedge to weaken. No sentence of it survives. The section slot is refilled with the standalone recommendation |
| 4 | Router shape: per-event exec, no daemon | Carried, reworked | §4 | Shape and reasoning carried; the shape's measurements move to the new §4.1 rather than living inline |
| 4.1 | *(none — new)* | New | §4.1 | Not a prior-document section. It is the document's one home for every measurement this pass took |
| 5 | Failure mode | Carried, reworked | §5 | The trade-off is re-argued against the zero-subscriber transport, not restated next to it |
| 6 | Live-view tap | Carried, reworked, merged | §6 | Its argument that the tap was cheap *because* it was in-process is retired outright; the section becomes a transport contract any subscriber can use, with houston as one example |
| 7 | Normalized payload schema | Carried, reworked | §7 | Envelope shape, event-naming rule, and tool-name mapping table carried; engine-only-events table gets the correction below |
| 8 | Native config emission, registration, and coexistence | Carried, reworked | §8 | The most houston-hosted section in the prior document; installer and renderers become hookyard's own command, not a generalized `houston hooks install` |
| 9 | Migration order | Carried, reworked, renumbered | §10 | Re-checked against a binary with no installed base and no release pipeline, not an existing houston subcommand |
| 10 | Houston panel | **Dropped as a standalone section, dissolved** | §6 | Once houston is a subscriber rather than a host, there is no houston-specific panel section to write; what remains is generic subscriber behavior, already covered by the transport contract |
| 11 | Open questions | Carried, renumbered | §12 | Each of the seven items carries forward with an explicit status; new questions the standalone premise raises are appended |

Two sections in the new document have no old-section ancestor at all: §9
(delivery — packaging, path form, version skew), absent
from the prior document because it never had to decide how a binary reaches
the machine when the binary was a houston subcommand already installed
there; and §11 (boundary: what hookyard does not absorb), new ground the user
raised after the prior pass, not a correction to
anything that pass wrote.

## 1. Problem statement

Three agent engines run on this machine — Claude Code, Codex, Cursor — and
each declares hooks (pre/post-tool guards, session lifecycle notifications,
status updates) in its own native config format. Four repos independently
wire behavior into those formats: aeye, lazytmux, dispatcher, and nix-config
itself. Because there is no shared registration point, the same *behavior*
gets declared, and usually re-implemented, once per engine.

The clearest case is aeye. Five behaviors — `images.sh`, `diagrams.sh`,
`session-reset.sh`, `session-backfill.sh`, `diagram-guidance.sh` — are
declared three times, in `adapters/claude-code/plugin/hooks/hooks.json`,
`adapters/codex/plugin/hooks/hooks.json`, and `adapters/cursor/hooks.json`,
differing only in event-name casing and how the plugin root is spelled
(`${CLAUDE_PLUGIN_ROOT}` vs `$PLUGIN_ROOT` vs `<ADAPTER_DIR>`). The prior
pass measured drift on these files and reported 231, 152, and 58 changed
lines for `diagrams.sh`, `session-backfill.sh`, and `images.sh`
respectively. This pass re-measured, by the same method — `diff a b | grep
-cE '^[<>]'`, i.e. added-plus-removed lines, run against a clean aeye tree
(`17da13f`) whose relevant scripts last moved at `7ea566c` (2026-08-30),
before the prior pass ran — and got **229, 150, and 56**. Those are not
close-enough rounding: they are what the stated method yields today, on
files that have not moved since before the figures being corrected were
written. No counting method tried this pass — `grep -cE '^[<>]'`, `wc -l`,
or any other adapter pairing — reproduces 231/152/58. `session-reset.sh`
fares worse: this pass measures 13 changed lines against the same
claude-code/codex pairing, and the prior figure of 21 matches no pairing
tried at all, including codex-vs-cursor (41). The prior pass's numbers are
therefore not carried as fact; this pass's own counts and method are what
the document records.

None of that changes the shape of the conclusion, which does survive: three
of the five original behaviors are genuinely drifted logic, not cosmetic
copies — different session-id extraction, different path resolution — while
`diagram-guidance.sh` is byte-identical across claude-code and codex (0
changed lines, confirmed again this pass) and so is not drifted between those
two — its Cursor copy does differ, by 12 changed lines under the same method,
making this a two-way result rather than a three-way one — and
Cursor's `session-backfill.sh` is a deliberate 6-line no-op stub, not a
degraded copy, so diffing it against the other two overstates drift. Against
that, two files outside the original five-item list are drifted too:
`session-reset.sh` (13 changed lines, claude-code vs codex) and each
adapter's own `lib/shim.sh` (claude-code has none; codex vs cursor differs by
59 lines). aeye already has one piece of shared-code machinery — a
`justfile` `sync-{codex,cursor}-core` step that vendors two core files via
`cp` — which is evidence the maintainer has already felt this pain, not
evidence the problem is solved. Net effect, unchanged from the prior pass:
aeye has real, multi-file, multi-engine drift; the fix is not weaker than
originally believed, it is differently shaped, and now differently counted.

The other repos show the same pattern at smaller scale, with two corrections
this pass makes to the prior pass's account. lazytmux declares 14 events /
23 hook entries for Claude Code via a plugin `hooks.json` — confirmed again
this pass — and separately injects hook content into `~/.codex/config.toml`
and `~/.cursor/hooks.json` straight from its Nix home-manager module, using
two different injection mechanisms: Codex's `config.toml` can carry
substantial hand-edited content, so lazytmux appends via a marker-guarded
`sed`/heredoc rather than templating the whole file, while Cursor's
`hooks.json` is merged in via `jq` with marker-scoped strip and `jq empty`
validation before write. **aeye injects nothing into `~/.codex/config.toml`;
lazytmux owns all of it** — the file's only content today is two
marker-guarded blocks, both lazytmux's, confirmed directly on this machine.
That correction, made once already by the prior pass,
holds up again under a second, independent check.

The prior pass counted three independent `jq` writers into
`~/.cursor/hooks.json`. This pass counts **four declared, three live**: (1)
aeye's `adapters/cursor/install.sh`, which strips by path substring; (2)
nix-config's inline dispatcher-stop block, which strips by
`dispatcher-cursor-notify`; (3) lazytmux's `cursor-hooks-install.sh`, marker
`/bin/cursor-status-hook`; and (4) lazytmux's
`cursor-relaunch-hooks-install.sh`, marker `/bin/cursor-relaunch-stamp`,
gated on `resumeCursorEnable` and not currently materialized on this
machine. The live file today carries output from the first three. Four
separate scripts hand-rolling the same marker-strip-and-validate merge over
one JSON file is the shape worth naming — call it **the marker-scoped
independent-writer idiom** — and it is the clearest single piece of evidence
that the current model, every repo owning its own writer in every engine's
format from scratch, does not scale past four repos, let alone the fifth and
sixth that will eventually want a guard. Whatever hookyard becomes to this
idiom, it is not "the fourth writer" or any other ordinal; that framing goes
stale the moment a writer is added, removed, or gated, as one already has
been.

dispatcher's duplication is corrected in the other direction, and the
correction comes with a caveat about *which* dispatcher. The prior pass found
one `SessionEnd` hook for Claude Code and one for Codex, and concluded
dispatcher had no Cursor hook mechanism at all. That does not survive against
the revision nix-config actually pins — `158abc8` — which ships
`adapters/cursor/scripts/dispatch-notify.sh`, byte-identical to
`adapters/core/dispatch-notify.sh` and to the Codex copy, all three confirmed
matching by direct comparison this pass.

The caveat matters and is stated rather than smoothed: dispatcher's own
`main` at the time of writing (`2865f4f`, older than the pin) has no
`adapters/cursor/scripts/` at all, and `scripts/gen-adapters.sh` at that
revision copies `dispatch-notify.sh` into exactly two trees — claude-code and
codex — never cursor. So the Cursor copy is **not** attributable to the
generator as it currently stands, and a reader checking `main` will not find
the file this correction rests on. The correction holds for the pinned
revision, which is the one that runs on this machine; whether the generator
grew a third tree in the pin or the file arrived another way was not
determined. dispatcher's duplication is therefore **three-way,
not two-way**. What it still lacks is a Cursor `hooks.json` of its own: only
claude-code and codex have one, and the Cursor-side wiring for
`dispatch-notify.sh` is done externally, by nix-config's `jq` merge — the
same merge machinery, and the same marker-scoped independent-writer idiom,
that also carries aeye's and lazytmux's entries into the live file. The
generator enforces byte-identity across the copies it produces; nothing
enforces re-running it, so a hand-edit to one copy can still silently drift
the moment someone bypasses the generator.

nix-config already contains the seed of the right idea, and its current
shape is itself evidence for what this design should not repeat.
`home/ai/agent-hooks/` holds exactly four guard binaries
(`git-default-branch-guard`, `nix-stage-guard`, `git-commit-autostage-guard`,
`secret-read-guard`) that are already engine-neutral in the sense that
matters: they speak Claude Code's PreToolUse contract — JSON on stdin, a
decision on stdout, silence is abstain — confirmed verbatim from the
module's own header comment. `home/ai/pi/hook-bridge.ts` already treats that
contract as a wire protocol worth reusing: it shells out to three of the
four guards (not `secret-read-guard`, which isn't wired for pi) via
`execFile` with a 5s timeout, consolidates with deny-wins, and treats
malformed or empty output as abstain. That is a small, working preview of
exactly the router this document specifies, built once, for one non-native
caller.

houston implements a second, larger preview, of a different half of the
same pattern, and is cited here as prior art precisely because the split it
demonstrates is engine-neutral and does not depend on where the missing
piece eventually gets built. `hook/hook.go`'s `Dispatch` reads Claude Code's
stdin JSON, merges it into a per-session state file, and returns only
`error`/`nil` — it never writes anything to stdout and never invokes a
guard. `main.go`'s `cmdHook` always returns exit 0 regardless of that error
("Never fail loudly from a hook — Claude Code would surface the error to the
user"). `hook/install.go` self-installs into `~/.claude/settings.json` with
`Timeout: 5` per entry and no matcher — it runs alongside whatever guards
are already registered, not competing with them. `runs/hooksource.go` plus
`runs/registry.go` compose a layered `Registry` (sources `tmux`, `crew`,
`hooks`, fixed precedence, keyed by a correlation key that falls back from a
tmux pane id to the session id) exposed over `/api/runs` and
`/api/runs/stream` (SSE), already consumed by a frontend. Hook dispatch and
the web server are the same binary, decoupled through state-dir files, so
dispatch works with no server running. All five of these facts are confirmed
directly against the source this pass and are cited as prior
art only — nothing in this design imports them, depends on their field
names, or assumes they are installed.

What the pi and houston previews jointly establish is where the actual gap
sits, independent of engine and independent of which repo happens to hold
the closest precedent today: **ingestion** (stdin → decision or state write)
and **live view** (state → stream → renderer) each already exist in more
than one place on this machine and generalize cleanly to more engines and
more repos. The piece that does not exist anywhere — consuming several
guards' verdicts and rendering one native-shaped allow/deny/ask response
inside an engine's own timeout, which is the hard constraint every one of
these callers already respects on its own terms — has no working
implementation, full stop. That gap is new work regardless of which engine
raises it and regardless of which repo, if any, currently has the nearest
neighboring code; nothing about houston owning the nearest ingestion and
live-view precedent implies houston is where the missing piece belongs, any
more than pi owning the nearest router preview implies pi is.

## 2. Prior art evaluated

### HookBus

[HookBus](https://hookbus.com) (`github.com/agentic-thinking/hookbus`) is
this exact design, already built and self-hosted: a daemon that publishers
push lifecycle events into, where subscribers return allow/deny/ask, sync
subscribers block the caller and async ones only observe. It is the
reference implementation of a separate spec repo,
`agentic-thinking/agenthook`. The facts below were checked directly against
the GitHub API, hookbus.com, and the project's `NOTICE` file by the prior
pass, and are **carried here as checked then, not re-fetched this pass**:

| Fact | Value |
|---|---|
| Core license | Apache-2.0 (Python) |
| Publisher licenses | MIT, stated — but the Claude Code publisher's actual GitHub license metadata is `NOASSERTION`, not MIT; a minor inconsistency worth a footnote, not a blocker |
| Publishers shipped | Claude Code (Python), Codex (JavaScript), OpenCode (JavaScript), Amp (TypeScript) |
| Cursor publisher | **Does not exist.** hookbus.com lists it as "coming soon"; the linked page 404s |
| Core stars | 2 |
| Publisher stars | 0, each |
| Last activity | publishers untouched since May 2026; core last pushed June 2026 |
| Self-description | README describes it as suitable for "development, evaluation, and controlled pilots" |
| Commercial tier | hookbus.com sells a paid Enterprise tier alongside the open-source core |

Two facts matter more than the star count. First, performance: HookBus's
Claude Code publisher is a per-event Python subprocess that imports `json`
and `urllib.request` before it can do anything, so whatever floor that shape
has sits on the critical path of every tool call, before any handler even
runs. The prior pass measured that floor at ~39ms and compared it to
houston's inherited `houston hook <event>` figure of 2.35ms, for a ~17x
multiple. This pass re-measured the Python floor itself, on this machine,
with the same `import json, urllib.request`, stdin-to-decision shape:
**34.9ms** (σ 4.6ms, 300 runs). Compared against this pass's own measured
Go shape — a static binary doing the equivalent stdin-to-decision-to-state-
write work, **1.0ms** (σ 0.1ms, 2811 runs; method and payload in §4.1 — that
is the document's one measurement home, so nothing about the method is
repeated here) — the multiple
is **33.8x**. The prior pass's ~39ms and ~17x figures are superseded by
these; the conclusion they support — a compiled binary is already cheap
enough to stay on the critical path per event, with no daemon required — is
unaffected, and is in fact somewhat stronger, since the comparison is now
against a binary of exactly the shape this design specifies, not one
inherited from a differently-shaped multi-purpose CLI.

Second, the `NOTICE` file discloses a pending UK patent application,
**GB2608069.7**, filed April 2026, claiming a governance-aware lifecycle
event bus with **priority-weighted deny-wins consolidation**, circuit
breakers, and failover/merge groups. Apache-2.0 §3 grants a patent license
for the project's code *as distributed*; it does not extend to an
independent reimplementation. It is a filed application, not a granted
patent, and UK claims do not publish until roughly late 2027, so there is
nothing to infringe or design around in a legal sense today. It is a factor
to name, not a blocker.

Naming it plainly: recommending an independent build, rather than adopting
HookBus's own code, means this design forgoes the Apache-2.0 §3 patent
license grant entirely — that grant only ever covered *their* code as
distributed, and an independent implementation was never inside it
regardless of what this document recommends. That trade is acceptable
specifically because this section's verdict already rejects HookBus on its
technical and maturity merits alone (the Python subprocess floor, the
missing Cursor publisher, the "evaluation and pilots" maturity), not because
of the patent — the grant being unavailable to an independent build isn't a
new cost this recommendation introduces, it's a fact about a path already
ruled out on other grounds. It is also the reason this document, and the
design it specifies, never adopts the claim's own vocabulary for its own
rule — "priority-weighted" appears here and in §4 only to name what this
design deliberately does not do. §4 below defines an
independent, much simpler rule (plain deny-wins: any deny wins, full stop)
that happens to be the natural choice for the four-guard case this machine
actually has, not a paraphrase of someone else's pending claim.

**Verdict: do not adopt or wrap HookBus.** The combination of a Python
subprocess floor on the tool-call critical path (34.9ms, re-measured this
pass), single-digit-star maturity that the project itself scopes to
"evaluation and pilots," no Cursor publisher (a real, current gap for a user
who dispatches work to Cursor workers), and a Nix-first, static-binary stack
that already has a cheaper, better-fitting answer rules it out on the merits
alone — independent of the patent question.

### Everything else

`@agiflowai/hooks-adapter` is a real, actively published npm package doing
payload normalization across agent hook formats. It is not adopted here,
but it is evidence that the schema question in §7 is a shared, recognized
problem and not specific to this machine's set of repos — this design's
normalized envelope is solving a problem other people are independently
solving too, which is a point in its favor, not a novelty claim.
`weykon/agent-hooks` is a Rust trait for unified *registration* across
several agent CLIs; it is closer to a language-level interface than a bus,
and it doesn't touch the router/consolidation problem this design needs
solved. Neither changes the recommendation below.

## 3. Recommendation: a standalone tool

hookyard is its own binary, its own repo, its own release surface. This is
settled input from the user, not a choice this document re-argues: the
prior pass answered "where does the code live?" with "inside houston," and
that answer is rejected. What follows is the standalone framing that
replaces it, not a case for standalone over the alternative.

The dependency direction is explicit and one-directional: **aeye, lazytmux,
dispatcher, and nix-config depend on hookyard; hookyard depends on none of
them.** hookyard knows nothing about any consumer's internals, imports
nothing from any of the four repos, and its correctness does not require any
of them to be present. Each of the four contributes its own manifest,
declaring which of its
handlers fire on which events — the same shape in which aeye's, lazytmux's,
and dispatcher's hook scripts are wired today, and versioned alongside the
handler it describes. What differs from that pattern is who writes the
engine-facing config: **no consumer repo emits native config for hookyard.**
One aggregated invocation does, for all of them at once, for the reason §8
gives — a per-repo invocation would strip the other repos' rows.

That direction has a real cost, and it is named rather than elided: hookyard
is a fifth thing that has to be present and correct on the machine, where
today there are four, and it is a build/release/test pipeline that does not
exist yet, where houston's would have been reused. Nothing here inherits an
existing pipeline; a new one — flake package output, CI, versioning — has to
be built for hookyard specifically. That is a genuine expense, not a rounding
error, and it is the price of the reduction in per-repo, per-engine
duplication documented in §1.

Two rationales the prior pass gave *for* building inside houston are retired
outright, not softened into hedges, and are not restated anywhere in this
document. The first was that building there would reuse an existing
release, build and test pipeline instead of standing up a new one. The
second was that co-location made the live-view write cheap, because it
crossed no process boundary. Both were true only on the premise that
hookyard's code lived inside houston's binary; neither survives the
standalone premise, and restating either in a softened form — "still mostly
true," "a smaller version of the same benefit" — would misstate the actual
shape of the new work. hookyard's live-view tap is designed in §6 as
a named transport any subscriber, houston included, reads independently and
without hookyard's knowledge; it is a second integration to build, not a
free one, and this document does not pretend otherwise.

The positive argument is not merely that standalone is tolerable despite
that cost — it is that a standalone static binary is, if anything, a
**better** fit for the per-event-exec shape §4 commits to than a subcommand
of a larger, multi-purpose binary was.

The grounding is §4.1's absolute number, not a comparison. A single-purpose
static binary doing exactly the work hookyard needs — read stdin, fork
guards, consolidate, emit a verdict, append one record — completes the whole
fan-out over four real guards in **12.1 ms**, and the process start alone in
**0.88 ms**. That is the entire budget question answered: at those numbers
per-event exec is affordable outright, with no argument needed about which
binary it lives in.

What standalone adds on top is not speed but the absence of a reason to grow.
A binary that also serves HTTP, holds a registry, and streams events has a
standing pull toward initializing more than a hook needs, and every such
addition lands on the critical path of every tool call. A single-purpose
binary has no such pull. That is a structural argument about what the code
will look like in a year, and it is the honest form of the claim — this
document deliberately does **not** rest it on a comparison against the prior
pass's 2.35 ms figure for `houston hook <event>`, because §12 records that
figure as measuring the wrong code path, and a number discarded as
unrepresentative cannot be load-bearing here.

nix-config remains exactly what it already was: the home for *wiring*, not
logic. Each consumer repo's Nix module points its engine's native config at
hookyard's binary and declares which guards from which repos are active;
none of that changes shape by virtue of hookyard being standalone rather
than houston-hosted, because nix-config never held the registry or router
logic under either premise.

Delivery — the concrete packaging shape, the path form emitted into native
config, and how version skew across independently-migrating consumer repos
is handled — is decided by pinned decision D1 (flake package output,
consumed as a flake input, wired per-repo, with an absolute, rebuild-stable
path rather than a bare name resolved on `PATH`) and developed in full in
§9, including which absolute form — a profile path, not a store path — the
evidence there settles on. This section only cites D1 by name, as the
delivery shape the rest of the recommendation assumes; it does not re-derive
or re-argue it.

## 4. Router shape: per-event exec, no daemon

**The router is a subcommand of hookyard's own static Go binary, exec'd once
per hook event by each engine's native hook mechanism. There is no
long-running daemon.**

That follows from the Nix-first constraint before it follows from any
measurement. A daemon on this machine would be a user service whose binary
path changes on every rebuild, which buys restart choreography — stop the old
unit, start the one the new generation pins, decide what happens to the
events that arrive in between — plus socket lifecycle, liveness probing, and
restart-on-crash. A per-event exec gets all of that for free: the engine's
native config holds one absolute path (§9), the kernel resolves it at hook
time, and the process that answers a tool call is by construction the version
the current home-manager generation installed. There is no in-between state
to design, because there is no process that outlives an event.

The measurement in §4.1 is what says that shape is affordable rather than
merely tidy, and it is measured on this machine for this workload, not
inherited: the whole router shape — read stdin, fork four real guards
concurrently, consolidate, print one verdict — costs **16.1 ms in its
worst measured case**, against a 5 s emitted timeout. A daemon would trade
that away to avoid a process start that is 0.88 ms. There is no
daemon-shaped problem here to justify a daemon-shaped solution.

### The consolidation rule

Claude Code's own documentation states that all matching hooks for one event
run in parallel, so native multi-hook fan-out is confirmed for that engine.
How an engine consolidates verdicts from parallel hooks that *disagree* was
unread for all three when the rule below was written. One of the three has
since been read directly, and it agrees: `cursor-agent`'s own reducer folds
two hooks' `permission` values with `deny` beating `ask` beating `allow`,
which is this design's rule exactly. Claude Code's and Codex's native
consolidation rules remain unread (§12, item 2).

That match is corroboration, not the source. This design still states its own
rule rather than borrowing one, because the reason for stating it — a router
that consolidates security verdicts should not inherit a rule it cannot see,
and should not change behaviour when an engine changes its own — is unaffected
by one engine turning out to have picked the same rule. It is deliberately the
simplest rule that can express a security control:

> **Consolidation rule: plain deny-wins.** Collect every handler's verdict for
> one event. `abstain` is the identity element — a handler that abstains is
> indistinguishable, for consolidation, from a handler that never ran. If any
> handler returns `deny`, the consolidated verdict is `deny`. Otherwise, if
> any handler returns `ask`, the consolidated verdict is `ask`. Otherwise
> `allow`.
>
> **`advise` is not a verdict and does not enter this lattice.** A handler
> that returns advisory context (§7) has expressed no opinion on whether the
> call should proceed, so it consolidates exactly as `abstain` does. Its
> advisory strings are collected separately and attached to whatever the
> consolidated verdict turns out to be — a denied call and an allowed call can
> both carry advice.

The rule needs no priority weights, and this document deliberately avoids the
**priority-weighted** vocabulary of the HookBus `NOTICE`'s pending
application (§2). The avoidance is a consequence, not the motive: weights
exist to let a ranked handler's verdict override an unranked one's, and the
only override that ranking makes possible on a deny-wins lattice is *a lower
weight allowing what a higher weight denied*, which for a set of security
guards is a defect rather than a feature. There is nothing here to weigh —
every handler is a guard, every guard's opinion counts equally, and one deny
is decisive. That is also the rule the two working implementations on this
machine already implement, independently of this design:
`home/ai/pi/hook-bridge.ts` fans out to three of the four `agent-hooks` guards
and consolidates deny-wins, and the guards themselves declare
silence-is-abstain in `agent-hooks/default.nix`'s own header. Cursor's own
reducer, above, is a third independent arrival at it — by a vendor with no
knowledge of this design, which is about as good as convergent evidence for a
consolidation rule gets. §4.1 reports the
rule executed
end-to-end over all four unmodified guards, so it is exercised behaviour, not
a proposal.

### What the router can deny, per engine

Whether an engine can *act* on a `deny` is a separate question from whether
the router can compute one, and it is answered separately per engine. The
honest state of that answer today:

| Engine | Confirmed deny path | Evidence |
|---|---|---|
| Claude Code | Yes | The shipped hook reference embedded in `claude-code-2.1.263` states PreToolUse blocks via `hookSpecificOutput.permissionDecision`, whose accepted value set is `"allow" \| "deny" \| "ask" \| "defer"` — `defer` is a fourth value this document did not previously carry. `PermissionRequest` takes a separate shape, `decision: {"behavior": "allow"}` or `{"behavior": "deny", "message": "…"}`. Three of the four `agent-hooks` guards already emit the PreToolUse shape, the fourth emits the advisory arm of the same contract (§7), and §4.1's end-to-end run shows one doing so |
| Codex | **Yes** — ruling reversed, see below | `codex-cli 0.153.4`'s own output-validation strings enumerate the supported surface by rejecting everything outside it: `PreToolUse hook returned permissionDecision:deny without a non-empty permissionDecisionReason` (deny accepted, reason mandatory), against `unsupported permissionDecision:allow`, `unsupported permissionDecision:ask`, `unsupported continue:false`, `unsupported stopReason`, `unsupported suppressOutput`, `unsupported decision:approve`, and `updatedInput without permissionDecision:allow`. A separate `permission_request` path carries `PermissionRequest hook denied approval` |
| Cursor | **Yes** | `cursor-agent 2026.09.08-6caf4ff` reads a `permission` field whose value set is exactly `allow`, `deny`, `ask`, honoured on the six events it lists as permission-capable: `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile`, `beforeTabFileRead`, `subagentStart`, `preToolUse`. A handler exiting 2 also blocks, with its stderr taken as the reason |

**The Codex ruling is reversed: `pre_tool` enforces.** The prior pass ruled
Codex observe-only — guards running, verdicts recorded with
`enforced: false`, no tool call ever blocked — on the explicit condition that
the ruling would be lifted when Codex's deny path was confirmed. It is now
confirmed, so the ruling is lifted rather than carried, and the standing gap
in coverage it described does not exist on this Codex version. Guards on
Codex enforce, and `secret-read-guard` and `git-default-branch-guard` stop a
Codex tool call the same way they stop a Claude Code one.

What Codex accepts is narrower than what Claude Code accepts, and the shape
of the narrowing is unusually convenient: **Codex's `pre_tool_use` is a
deny-only decision channel.** `permissionDecision: "deny"` with a non-empty
`permissionDecisionReason` is honoured; `allow` and `ask` are both explicitly
rejected, as are `continue: false`, `stopReason`, `suppressOutput`, the legacy
`decision: "approve"`, and any `updatedInput`. A router whose consolidation
rule is deny-wins (below) never needs the rejected half: the only verdict it
has to render to Codex is the one Codex takes. The engine with the poorest
decision vocabulary is the engine whose vocabulary happens to be exactly the
one this design uses.

One thing about this evidence is worth stating precisely, because the rest of
the document is careful about it. The confirmation is **static**: it comes
from the validation strings compiled into the shipped `codex-cli 0.153.4`
binary, which enumerate the accepted surface by naming everything outside it
as unsupported. That is strong evidence about the contract — these are the
messages Codex emits when it refuses a field — and it is not the same as
having watched Codex refuse a tool call. §12 carries the live confirmation as
a remaining item, downgraded from security-blocking to a behavioural check on
a contract already read off the implementation.

Note where that verdict goes. It goes to hookyard's own record, which exists
on disk whether or not anything is subscribed to it (§5, §6). It does *not*
go to a panel, a renderer, or any other process's state directory; a
subscriber may of course read it, and houston is one candidate subscriber
among others, but the record's existence and usefulness do not depend on one
being installed, running, or ever written.

### Bounds on what the router reads

Two sizes are capped, both on the attacker-reachable path, and both because
§5's independence assumption depends on them rather than because unbounded
input is untidy.

**The inbound payload is capped before decode.** The router reads at most
1 MiB from stdin and refuses to decode more. A payload over the cap is not
truncated and re-parsed — truncation would change the meaning of the JSON —
it is rejected outright, which produces the router's own `error` path: the
fail-open verdict is printed and a record is appended saying why. 1 MiB is
far above any real hook payload and far below anything that pressures memory
on the critical path.

**Each handler's stdout is capped as it is read.** The router reads at most
64 KiB from a handler and stops there, killing the handler if it keeps
writing. A verdict is a decision plus a reason; nothing legitimate approaches
that. A handler that exceeds it is recorded as `error`, not `abstain`, so the
two are distinguishable in the stream — a handler that has started producing
unbounded output is a defect, and treating it as a silent abstention would
hide exactly the case §5 needs visible. `reason` is separately truncated to
512 bytes for the record (§6); the 64 KiB cap is what the router will read at
all.

Neither cap is a security boundary on its own. Their purpose is to remove the
cheapest ways an attacker-shaped payload could push the router or a handler
into failing, which is what makes §5's residual small rather than open-ended.

### The timeout budget chain

No deployed timeout on this machine is worth inheriting, because the deployed
values disagree and they disagree for reasons that have nothing to do with a
deny path. Confirmed first-hand this pass: lazytmux's Codex blocks in
`~/.codex/config.toml` set `timeout = 30` on every entry; the live
`~/.cursor/hooks.json` carries 15, 30 and 60 across its writers; and
dispatcher's Codex plugin `hooks.json` declares none at all. Those are all
fire-and-forget status and notification hooks, where a generous ceiling costs
nothing because nothing waits on the answer. The number this design has to
fix is different in kind: it is the timeout hookyard's own installer *emits*
into native config (§8), and it sits on a synchronous path where the engine
is holding a tool call until the router exits.

| Layer | Budget | Rationale |
|---|---|---|
| Emitted timeout (native config, §8) | **5 s**, all three engines | This entry is on the synchronous deny path, so it is deliberately tighter than the 15–60 s fire-and-forget values deployed today. It is not a performance budget — §4.1 measures the whole shape at 16.1 ms worst case, 0.3% of it — it is a hang-containment budget: it bounds how long one stuck guard can freeze one tool call before the engine gives up on it. Five seconds is short enough that a human waiting on the call reads it as a stutter rather than a hang, and long enough that no guard doing honest work on a cold filesystem cache is cut off. houston's installer emits 5 per entry too (`hook/install.go:139`), which is prior art that the value is livable — it is not the reason for it |
| Router's internal deadline | **4.5 s** | 500 ms under the emitted timeout. Two things have to fit in that margin, not one: the router process's own start and fan-out, which §4.1 measures at 16.1 ms worst case and which the margin therefore covers about 30-fold over; and — the part that matters more — the router's ability to *lose gracefully*. Hitting its own deadline first, rather than being killed by the engine's, is what lets the router print a fail-open verdict and append a `router: "timeout"` record before it exits (§5). A router that only ever died at the engine's timeout could never record the fact that it did |
| Per-handler sub-budget | **4.3 s**, one shared deadline context, run concurrently | Handlers run as concurrent subprocesses under a single deadline context, not in sequence, so the sub-budget is the internal deadline minus a ~200 ms consolidation margin: the time the router needs after the slowest handler returns or is killed to build the verdict, write it, and append the record. §4.1's fan-out numbers are the evidence that concurrency is the right structure here — four guards cost what one costs — so the sub-budget is per-handler wall clock, not a share of a serial budget |
| Engine default when no timeout is declared | **None on Codex — measured, and worse than an unknown default** | A Codex `UserPromptSubmit` entry declaring no `timeout`, running a hook that ticked once a second, was allowed to run for the full 180 s of its own loop and was never killed; Codex displayed `Working … Running hook` and waited. So the fallback is not a generous default, it is no bound at all: an undeclared timeout lets one hook stall a turn indefinitely. Measured to 180 s, which is where the probe stopped rather than where Codex did. This vindicates the rule that hookyard's installer always emits an explicit timeout, and upgrades the reason from "the default is unknown" to "there is no default to rely on" |

What this chain buys over what exists piecemeal today is one number, emitted
identically for three engines, that the router is designed to live inside —
rather than three engines' worth of values chosen for hooks that nothing waits
on.

## 4.1 What this costs, measured

Everything in this section was measured on this machine during this pass:
**AMD Ryzen AI MAX+ 395, 32 cores, Linux 7.2.3, Go 1.26.7**. Method:
`hyperfine -N --warmup N --input <payload>`, with `-N` to exclude shell
startup from the timing, and the payload a realistic Claude Code `PreToolUse`
JSON body fed on stdin. Run counts are hyperfine's own and are reported per
row. The benchmark sources are throwaway probes that live outside this repo
and are **not committed** — the commands, payload shape and results here are
what makes them re-runnable.

**The stated limit, up front:** the measured artifact is a throwaway probe
standing in for the router's *process shape*, not the router. There is no
router to benchmark; the probe reproduces what the router does structurally —
decode stdin, fork guards concurrently under a deadline, consolidate, print
one decision, write one state file — and nothing about its guard logic. The
guards it forks, by contrast, are not stand-ins: they are the four real,
unmodified scripts from `home/ai/agent-hooks/`.

### Per-event process shapes

The question this answers is whether a per-event exec is affordable at all,
and how much the language choice costs before any handler runs. The Go probe
is a static binary (`CGO_ENABLED=0`, `-ldflags="-s -w"`) that decodes stdin,
prints a `hookSpecificOutput` decision, and writes then removes one state
file. The other two do the equivalent stdin-to-decision work in their own
idiom.

| Shape | Mean | σ | Runs |
|---|---|---|---|
| Static Go binary, stdin → decision → state write | **1.0 ms** | 0.1 ms | 2811 |
| bash + `jq`, stdin → decision | 4.5 ms | 0.4 ms | 626 |
| `python3`, `import json, urllib.request`, stdin → decision | **34.9 ms** | 4.6 ms | 300 |

Go is **4.4x** faster than bash + `jq` and **33.8x** faster than the Python
floor. The third row is the shape HookBus's Claude Code publisher has, and
that 34.9 ms is paid before any handler runs, on every tool call; it is the
measurement §2's rejection rests on, taken here rather than inherited.

### Router fan-out

This is the shape the prior pass's open questions flagged as never measured.
The probe reads stdin once, forks N guards concurrently under a 4.3 s context
deadline, treats non-zero exit, empty output or malformed JSON as abstain,
consolidates deny-wins, and prints one decision. The guards are the four real,
unmodified `agent-hooks` scripts.

| Shape | Mean | σ | Runs |
|---|---|---|---|
| Router, 0 guards | 0.88 ms | 0.07 ms | 2935 |
| Router + 1 real guard | 12.4 ms | 1.4 ms | 304 |
| Router + 4 real guards, all abstain | **12.1 ms** | 1.6 ms | 211 |
| Router + 4 real guards, one denies | **16.1 ms** | 2.1 ms | 200 |

Two conclusions matter, and both are load-bearing for decisions elsewhere in
this document.

**Fan-out is free at this scale, on this machine.** Four concurrent guards
cost the same as one — 12.1 ms against 12.4 ms, a difference comfortably
inside σ, and the four-guard mean is if anything the lower of the two, which
is itself a sign the two rows are near-identical workloads rather than a
measured speedup. The cost is the first guard subprocess, not the number of
them. That is why §4's per-handler sub-budget is stated as per-handler wall
clock under one shared deadline rather than a share of a serial budget.

The claim needs its bounds stated, because §4 leans on it. It was measured at
N=4 on a 32-core machine with four guards that each finish in ~10 ms. It says
nothing about N approaching core count, and nothing about handlers that are
themselves slow — §8's own example manifest declares a 1500 ms handler, two
orders of magnitude heavier than these. What generalizes is the mechanism
(concurrent handlers under one shared deadline cost the slowest, not the sum);
what does not generalize is the specific conclusion that a fifth or sixth
repo's guards are free. On this machine, at this scale, with guards of this
weight, they are.

**The slowest shape measured here is 16.1 ms against a 5 s emitted timeout —
0.3% of budget.** That is the maximum across the four probed configurations,
not a true worst case: the real worst case is a handler that consumes its
entire 4.3 s sub-budget, which is precisely what the 5 s ceiling exists to
contain. The emitted timeout is therefore not a performance constraint on the
router in any regime this machine has measured; it is a ceiling on how long a
*stuck* guard can hold a tool call. That reading is what §4's timeout chain
argues from.

### Deny-wins, verified end-to-end, guards unmodified

The tables above measure cost. This run measures behaviour, and it is the only
first-hand evidence in this document for the deny-wins rule and for the claim
that the existing guards run unmodified — §7's handler-compatibility argument
cites it rather than restating it.

A `git commit -m wip` payload, with `cwd` pointing at a checkout sitting on
its default branch, was fed to the router probe over all four unmodified
guards. `git-default-branch-guard` returned a Claude-Code-shaped
`permissionDecision: "deny"` carrying its own reason string; the other three
exited 0 with no output, which is abstain; and the router emitted a single
consolidated `deny`. Fed a payload none of the four objects to, all four
abstained, and the router printed nothing and exited 0.

Both halves matter. The deny path shows a real guard's real verdict surviving
consolidation intact, including its reason. The all-abstain path shows the
identity element behaving as specified — four handlers that ran and had no
opinion are indistinguishable from no handlers at all. Neither required
touching a guard: they were run exactly as `home/ai/agent-hooks/` ships them.

## 5. Failure mode

This design makes two decisions at two layers, and they are emphatically not
the same decision. Conflating them is how "fail-open" gets read as a blanket
posture rather than a specific choice about a specific process.

**A single guard erroring or timing out abstains.** Non-zero exit, empty
output, unparsable output, or a handler that hits the 4.3 s sub-budget all
land on the same verdict: `abstain`, the identity element. This is already the
behaviour of both working implementations on this machine — the pi bridge
treats malformed or empty guard output as abstain, and the guards' own
contract makes silence abstain — and §4.1's probe implements it. One flaky
guard must not be able to block every tool call it is wired to. What changes
here is only bookkeeping, and it is the part the mitigation turns on: the
record in §6
distinguishes an `abstain` (the guard ran and had no opinion) from an `error`
or a `timeout` (the guard ran and failed to have one). Both consolidate
identically; only one of them is a defect, and the record is where the
difference survives.

**The router itself being unreachable or failing is a separate decision, and
the recommendation is fail-open.** If the binary cannot be found, cannot
start, or cannot produce a verdict before its own 4.5 s deadline, the tool
call proceeds as though no hook had fired.

That trade-off is argued, not defaulted, and it is genuinely close. Fail-open
means a defect in the router, or a wired path that stopped resolving, silently
disables *every* guard wired through it, with nothing visible at the point of
the tool call: an agent that was supposed to be stopped from committing to
`main` simply commits to `main`. Fail-closed means the same defect freezes
every tool call, on every agent, on every engine, simultaneously — a
machine-wide outage caused by the infrastructure that was supposed to be
protecting the code, not by the code it was protecting. Both are bad; the
question is which is worse *given where this router sits*, and the answer
comes from that placement rather than from a general preference. hookyard is
in front of every guarded tool call across three engines at once, so its
fail-closed blast radius is the whole machine's agent capacity, while its
fail-open blast radius is the subset of calls a guard would have denied — a
small subset, since §4.1's all-abstain path is the overwhelmingly common one.
A single point of failure that can only degrade is a better single point of
failure than one that can only stop. **Fail-open is the recommendation.**

### The independence assumption, and why it needs defending

The blast-radius argument above compares *frequencies*: fail-open costs the
subset of calls a guard would have denied, and that subset is small. That
comparison is only valid if router failure is statistically independent of
whether the call under evaluation should have been denied. It is not
automatically independent, and the design has to say so rather than leave the
assumption buried.

The router's inputs are attacker-influenced whenever the calling agent is
under prompt injection or otherwise compromised — which is precisely the
situation `secret-read-guard` and `git-default-branch-guard` exist for. The
envelope carries `tool_input` verbatim from the engine, and that is the field
an attacker steers. So a payload shaped to crash one guard, to exhaust its
sub-budget, or to make the router itself fail on decode produces exactly the
same outcome as a random flake — `abstain`, then fail-open — but triggered on
the one call the guard would have stopped. Frequency is the wrong axis for a
control whose entire value is concentrated in the rare deny.

This design does not have a way to make failure provably independent of
input, and claiming otherwise would be dishonest. What it does instead is
narrow the ways input can cause failure, so that the residual is small enough
to accept:

- **Bounded decode and bounded handler output (§4).** The router's own parse
  is size-capped before decode, and each handler's stdout is capped as it is
  read. A payload cannot make the router allocate without limit, and a guard
  cannot be made to fail by being handed more output than the router will
  read. These caps exist for this reason, not for tidiness.
- **A guard that fails is recorded as having failed (§6).** `error` and
  `timeout` are distinguished from `abstain` in every record. A guard that
  starts failing on a particular shape of input is visible as a pattern in
  the stream, which is what turns a steered failure from silent into
  discoverable.
- **What is deliberately *not* done.** hookyard does not fail closed for
  security-classed handlers. A per-handler `critical: true` flag that made
  fail-open conditional is the obvious next step, and it is deliberately not
  specified here, because it reintroduces the machine-wide-freeze failure
  mode this section just rejected, on the exact events most likely to fire.
  It is named as the first thing to reconsider if a steered-failure case is
  ever observed in practice, and §12 carries it.

The honest statement of the position: fail-open is chosen knowing that an
attacker who can shape tool inputs may be able to convert a deny into an
allow, and the mitigation is that doing so leaves a record, not that it is
impossible.

### What the mitigation actually is

The prior pass recommended fail-open *on the condition* that the live-view tap
made a silently-disabled guard visible after the fact. Under the standalone
premise that condition is no longer automatically satisfied, because §6's
transport must work with zero subscribers — a stream nobody reads cannot carry
a mitigation the recommendation depends on. This is resolved rather than
restated, and it is resolved by taking the first of the two available answers:

**The record hookyard writes is always-on, and it is not the subscription.**
§6 specifies one append-only file that the router appends to at the end of
every event it handles, unconditionally, with no listener, no socket, and no
handshake. Subscribers are optional *readers* of that file. The mitigation is
that the record exists on disk, not that something consumed it; a subscriber
turns a recoverable fact into a visible one, which is a convenience, not the
condition. Under the standalone premise this is the only version of the
argument that survives — and it survives cleanly, because writing a line to a
file is exactly as available with zero subscribers as with one.

What that record makes recoverable is precisely the fail-open failure. Each
record carries the consolidated verdict, whether it was enforced, and a
per-handler breakdown with each handler's outcome distinguished as
`allow`/`deny`/`ask`/`advise`/`abstain`/`error`/`timeout`. So "a guard
silently stopped
firing" is not an inference from absence of harm: a guard that is erroring
appears in every record with `outcome: "error"` and its message, and a guard
that has fallen out of the registry entirely appears in no record's handler
list while its siblings still do. Both are answerable by reading one file, and
neither requires a subscriber to have been running at the time.

### The sub-case the record cannot cover

The headline fail-open scenario is that the binary cannot be found. Nothing
runs; nothing writes a record. An always-on record cannot cover that case by
being written, and this document does not pretend otherwise. What it can do is
be precise about which sub-cases it *does* cover and how the one it does not
gets noticed.

| Router-unreachable sub-case | Covered by the record being written? |
|---|---|
| Router starts, a handler errors or times out | Yes — per-handler `error`/`timeout` outcome, on every event |
| Router starts, then fails before consolidating (unreadable manifest, malformed inbound payload, internal panic) | Yes — it prints the fail-open verdict and appends `router: "error"` with the reason before exiting |
| Router starts, exceeds its own 4.5 s deadline | Yes — the 500 ms margin under the emitted 5 s exists so it can print and append `router: "timeout"` rather than being killed mid-event |
| Router exec fails: hookyard absent from the profile, or the wired path not executable | **No.** Nothing runs |

The last row is the one that has to be answered on other terms, and §9's path
form changes what it means. Because native config carries one absolute path
rather than a bare `hookyard` resolved on `PATH`, this is never an environment
problem — never "the engine handed the hook a `PATH` that didn't contain it",
the exact failure that `home/ai/cursor/default.nix` already pins `PATH` to
avoid for `jq`. It is a path that either resolves or does not, which makes it
a *decidable* check rather than a reproduce-the-hook-environment guess. Two
consequences follow:

**It is rare by construction.** Native config is re-rendered on every
home-manager activation, and §9's profile path resolves through the active
profile, which is itself a GC root. Reaching this state requires hookyard to
be absent from the profile outright — never installed on this host, removed
from `home.packages`, or an activation that failed partway — or the config to
have been hand-edited past hookyard's own writer.

**Its detection is by absence, and absence is polled, not pushed.** Two
checks, neither of which needs a subscriber. The first is hookyard's own
`doctor` subcommand — hookyard's, not any other tool's — which reads each
engine's native config, extracts every emitted hookyard command string, and
reports any whose path does not exist or is not executable. That check is only
possible because §9 emits a resolvable absolute path. The second is
the stream itself, read for what is missing: the newest record's timestamp
against the last time an agent session ran. A stream whose newest record
predates this morning's work means either nothing is wired or what is wired is
dead, and that is an `stat` on one file, not a subscription.

**Both checks have a bootstrap hole, and it is the same one.** `doctor` is a
subcommand of the binary that is missing, so in the exact state it was
introduced to detect — hookyard absent from the profile — it cannot run. And
on a machine where hookyard has never run at all there is no stream file to
`stat`, so staleness has nothing to compare against either. The two checks
cover the case where hookyard *was* working and stopped; neither covers the
case where it never started. What covers that is the activation itself:
hookyard's own home-manager module asserts the binary is in
`home.packages`, so a wiring that emits config without installing the binary
fails at build time rather than at hook time. That is a build-time check, not
a runtime one, and it is the honest answer — the runtime detectors do not
cover their own absence, and the design does not claim they do.

The residual is stated plainly rather than mitigated away: between the moment
a wired path stops resolving and the moment someone runs `doctor` or notices a
stale stream, guards are off and nothing says so. Absence-detection is
polling-shaped by nature; the only designs that make it event-shaped are a
daemon (rejected in §4) or a fail-closed posture (rejected above, and which
would announce the outage by causing one). That window is the price of
fail-open, and this design pays it knowingly.

## 6. The event stream: transport contract

hookyard emits an event stream that any subscriber can consume, and hookyard
knows nothing about any subscriber. This section is also where the prior
document's houston-panel section goes: once houston is a subscriber rather
than a host, there is no houston-specific section left to write — there is a
transport contract, and houston is one example of something that could read
it. What a subscriber renders is out of scope here.

### The transport

**An append-only JSONL file under hookyard's own state directory. One line per
handled event, appended with a single `write(2)` on a descriptor opened
`O_WRONLY|O_APPEND|O_CREAT`. No daemon, no socket, no lock.**

The choice is forced by the two hard requirements together. It must work with
zero subscribers, which rules out anything requiring a listener — a Unix
socket has no one to connect to on a machine with no subscriber installed, and
a connect attempt on the router's path is a stall the design cannot afford.
And there are many concurrent writers by construction: three engines, many
tmux panes, several hook events in flight at once, each a separate short-lived
process. `O_APPEND` is the mechanism that makes many independent writers safe
without a lock or a coordinator, which is exactly the shape a per-event exec
router has. A spool directory of one file per event would also work with zero
subscribers, but it makes the subscriber's job harder (ordering by filename,
readdir storms, its own cleanup) for no gain over an append.

| Property | Contract |
|---|---|
| **What** | One JSON object per line, terminated by `\n`, written whole in one `write(2)`. Schema versioned by a `v` field |
| **Where** | `$HOOKYARD_STATE_DIR`, else `$XDG_STATE_HOME/hookyard`, else `~/.local/state/hookyard`; the stream lives in `stream/YYYY-MM-DD.jsonl` (UTC). This is a local filesystem; the append semantics below are not claimed over NFS |
| **Ordering** | Byte order within a file is append order — the order in which routers *finished* events, not the order in which engines started them. Within one pane the two coincide for synchronous events, because the engine holds the tool call until the router exits and cannot issue the next one first. Across panes and engines, nothing is claimed; each record carries its own `ts` and a subscriber that needs a cross-pane order sorts by it and tolerates skew |
| **Durability** | Visible to any reader on the machine as soon as `write` returns. No `fsync`: the record survives the router process dying, and is not claimed to survive a power loss. That is the correct trade for a record whose write is downstream of a verdict already delivered |
| **Atomicity** | One record = one `write(2)`, capped at 64 KiB so a short write is not a practical concern. Records that would exceed the cap are truncated, not split, and marked `truncated: true`. That a single `O_APPEND` write is not interleaved on a local filesystem is relied on here and was **not tested under concurrency this pass — unverified** |
| **Retention** | Daily rotation by filename. A router that finds itself writing the first record of a new day removes stream files older than 14 days. This is the only housekeeping hookyard does, it costs one `readdir` per day, and it is why the always-on record is not an unbounded disk commitment |
| **Discovery** | The state directory path above, and nothing else. A subscriber globs `stream/*.jsonl`, reads the newest to end, then follows it (inotify or poll), and re-globs when the date rolls |
| **No subscriber** | Nothing changes. The file is written, rotated and expired identically. There is no registration step, no listener count, and no code path that behaves differently because something is reading |
| **Permissions** | The state directory is created `0700` and stream files `0600`, explicitly, not left to the ambient umask. The stream is a behavioural log of every guarded tool call on the machine — `cwd`, `tool_name`, session identity — and on a shared host that is not something other local users should be able to read |
| **Back-pressure** | None, in either direction. hookyard never waits for a reader and readers cannot slow it down. Delivery is at-most-once from a subscriber's point of view: a subscriber that is not running when a record is written sees it only if the file is still within retention when it starts |

### The ordering rule, which is the load-bearing part

**Print the consolidated decision for the engine first. Write the stream
record second, best-effort, with any error swallowed.** This survives from the
prior design unchanged and it is what makes the tap fire-and-forget in
practice rather than only in intent.

The reason is sharper than "so the write does not delay the verdict", because
the engine does not stop waiting when it reads the decision — it waits for the
process to exit, so the record write is still inside the emitted 5 s. What
writing the decision first actually buys is that the verdict is *already
complete and flushed* before any I/O that could stall is attempted. A full
state directory, a permissions problem, a disk that has gone slow: none of
them can turn a computed deny into a malformed or absent answer, and if the
engine's timeout fires mid-append, it fires against a process whose decision
the engine has already read in full. The swallowed error is the same posture
the record itself takes toward its own failures — a stream write that fails
must never convert into a verdict change, because a verdict change is a
security decision and a failed append is a bookkeeping problem.

### The record

```
{
  "v":               1,
  "ts":              "2026-09-09T11:04:22.481932Z",
  "key":             "%21",
  "engine":          "codex",
  "session_id":      "...",
  "canonical_event": "pre_tool",
  "native_event":    "PreToolUse",
  "cwd":             "/home/noams/nix-config",
  "tool_name":       "Bash",
  "verdict":         "deny",
  "enforced":        false,
  "reason":          "on default branch main; branch first",
  "router":          "ok",
  "router_ms":       4312,
  "handlers": [
    {"name": "git-default-branch-guard",   "outcome": "deny",    "ms": 11},
    {"name": "secret-read-guard",          "outcome": "abstain", "ms": 9},
    {"name": "git-commit-autostage-guard", "outcome": "advise",  "ms": 8,
     "advice": "Unstaged tracked changes present alongside staged changes",
     "delivered": false},
    {"name": "nix-stage-guard",            "outcome": "timeout", "ms": 4300}
  ]
}
```

`canonical_event` and `native_event` follow §7's naming rule exactly — six
canonical names, everything else under an explicit `engine:NativeEvent` — and
§7 is where a subscriber reads which events it can expect from which engine,
including the one event that does not exist cross-engine. This section does
not restate that set.

The example is deliberately a bad day rather than a typical one: one guard
denies, one advises, one abstains, and one is killed at the 4.3 s sub-budget.
Note that `router_ms` is 4312, not 16 — handlers run concurrently under one
deadline, so the router's wall clock is the slowest handler plus
consolidation, and a run containing a timed-out handler cannot also be fast.
§4.1's 16.1 ms figure is what this looks like when every handler returns
promptly. Note also that the killed handler is `timeout`, not `error`: the two
are distinct outcomes and conflating them in an example would undercut the
distinction the next paragraph rests on.

Four fields carry the weight of §5's argument, and one outcome value does.
`verdict` is the consolidated
result; `enforced` is `false` exactly when the router computed a verdict the
engine cannot act on. With Codex's `pre_tool` deny path confirmed (§4), no
engine is wholesale observe-only any more, so this field now marks the
narrower per-event cases — an `ask` rendered to Codex, which accepts only
`deny`, or an event whose engine has no decision slot at all; `router` is `ok`, `error` or `timeout`, which is what
makes a router that failed *after starting* recoverable; `handlers`
distinguishes `abstain` from `error` and from `timeout`, which is what makes a
guard that has silently stopped working recoverable; and an `advise` outcome
carries the advisory text itself plus a `delivered` flag, so an advisory the
target engine had no slot for (§7) is visible as *written but not delivered*
rather than disappearing. In the example above `delivered` is `false` because
the engine is Codex, which has no confirmed advisory slot — the advice
happened, the model never saw it, and the record says both.

One thing is deliberately absent: the record carries `tool_name` but not
`tool_input`, and not §7's raw `native` blob. The envelope handlers see is an
in-process value; the record is a durable file on disk, and tool inputs
routinely contain full shell commands, file paths and environment. Writing
them to a 14-day retained log to make a subscriber's rendering slightly richer
is a bad trade, and it would also put record size at the mercy of the payload
rather than under the 64 KiB cap.

Guard-authored `reason` strings are kept, truncated to 512 bytes, and they
carry two obligations that are easy to miss. **A guard must not echo matched
content into its reason.** The exclusion of `tool_input` above is defeated
entirely if `secret-read-guard` — the handler most likely to want to quote
what it matched — writes the matched material into a string that is then
persisted for fourteen days. The contract is that a reason names *why* it
denied, not *what* it saw: "path matches a credential-file pattern", never
the line it matched on. This is a contract on handlers, which hookyard cannot
enforce by inspection, so it is stated here as a requirement on any handler
wired into the registry rather than pretended to be a router guarantee.
**And a reason is untrusted text to whoever renders it.** It is authored by a
handler, stored verbatim, and handed to any subscriber. A subscriber that
renders it into a terminal or a web view is responsible for escaping it;
hookyard neither sanitizes nor validates its content beyond the length cap.

### The correlation key

**The key is `$TMUX_PANE` when that variable is set; otherwise
`engine + "/" + session_id`.** This is hookyard's own rule and it is chosen
for three properties, none of which are about any particular subscriber.

It must be stable across a whole interactive session and identical for every
engine. A pane id is: it belongs to the terminal the engine was launched into,
not to the engine, and hookyard reads it from the environment — `$TMUX_PANE`
is an ordinary environment variable, engine-agnostic, present or not
regardless of which of the three is running.

It must be knowable by a subscriber that has never seen hookyard. A pane id is
externally observable — tmux itself will enumerate them — so a subscriber that
already knows about a pane can join hookyard's stream to it without hookyard
publishing a mapping, and without hookyard knowing the subscriber exists. That
property is why the key is not a uuid hookyard mints: a minted key would be
correct, stable and useless to anyone who was not listening when it was
created.

And the fallback is engine-scoped because session identifiers are only unique
within an engine — they are not even called the same thing across the three
(§7 normalizes `session_id`, `conversation_id` and `generation_id` onto one
field), so an unscoped session id could collide across engines and would carry
no signal about which one it came from.

Prior art, cited as evidence the shape works and nothing more: houston's
`runs/registry.go:16` documents its correlation key as "the tmux pane id where
there is one", and `runs/hooksource.go:116` falls back to a session-id-based
key — hardcoded to one engine's name, since that is the only engine it
ingests — overridden by the pane id when one is present. Both confirmed
against the source this pass. That is a working system that arrived at the
same two-tier key independently; it is not the derivation of this rule, and
hookyard neither imports nor depends on it. hookyard's rule would be the same
if that code did not exist.

The rule has a real limitation, and stating it is more useful than hiding it:
the key identifies a *pane*, not a session. Two engines sharing one pane — an
agent that launches another agent in place — collide on one key. A subscriber
that needs session granularity has it: `engine` and `session_id` are on every
record, always, and the key is a grouping, not an identity.

### What a subscriber gets, and what it must not assume

A subscriber gets an append-only, chronologically-appended, JSON-per-line
history of every event hookyard handled inside the retention window, with the
verdict and per-handler outcome for each. That is the entire contract. It must
not assume: that hookyard knows it exists; that a record for one event implies
one for any other; that all three engines appear; that the file is complete
back to the beginning of time (retention truncates it) or that it will grow
again (a machine may simply stop running agents); that the final line of a
file is whole, since a router killed mid-write can leave a torn record, which
a subscriber discards rather than retries; that unknown fields can be
rejected, since `v` will grow additively and readers must ignore what they do
not recognize; or that it may write to the stream, which is hookyard's alone
to append to.

## 7. Normalized payload schema

### Inbound: one envelope, translation not reinvention

The six event concepts already line up across engines almost exactly:

| Concept | Claude Code | Codex | Cursor |
|---|---|---|---|
| session start | `SessionStart` | `SessionStart` | `sessionStart` |
| prompt submit | `UserPromptSubmit` | `UserPromptSubmit` | `beforeSubmitPrompt` |
| pre tool | `PreToolUse` | `PreToolUse` | `preToolUse` |
| post tool | `PostToolUse` | `PostToolUse` | `postToolUse` |
| pre compact | `PreCompact` | `PreCompact` | `preCompact` |
| turn end | `Stop` | `Stop` | `stop` |

The Cursor column of this table is not read off documentation. The live
`~/.cursor/hooks.json` on this machine was inspected directly this pass: its
`hooks` object carries keys `sessionStart`, `beforeSubmitPrompt`,
`preToolUse`, `postToolUse`, `postToolUseFailure`, `preCompact`, `stop`, and
`subagentStart`. All six converged names above are present verbatim among
them, which is first-hand confirmation of the convergence claim, not an
inherited assumption.

This convergence is what makes the router tractable — it is mostly
translation, not new semantics. But the event *counts* diverge sharply once
you look past those six, and two of the three counts are no longer carried
figures. Cursor's is now exact: its shipped bundle declares an event enum of
**21** names — `beforeShellExecution`, `beforeMCPExecution`,
`afterShellExecution`, `afterMCPExecution`, `beforeReadFile`, `afterFileEdit`,
`beforeTabFileRead`, `afterTabFileEdit`, `stop`, `beforeSubmitPrompt`,
`afterAgentResponse`, `afterAgentThought`, `sessionStart`, `sessionEnd`,
`preCompact`, `subagentStart`, `subagentStop`, `preToolUse`, `postToolUse`,
`postToolUseFailure`, `workspaceOpen` — which confirms the prior pass's
"roughly 21" at the number, and confirms the protocol-specific pairs and
tab-completion hooks it named as present rather than assumed. Codex's
vocabulary is confirmed as **snake_case** and at least eleven names wide
(`pre_tool_use`, `post_tool_use`, `permission_request`, `session_start`,
`session_end`, `user_prompt_submit`, `stop`, `pre_compact`, `post_compact`,
`subagent_start`, plus the `Interrupt` the prior pass called unique to it),
read off the shipped binary and the `[hooks.state]` keys in the live
`~/.codex/config.toml`. Claude Code's 30–33 stays a carried figure: the hook
reference embedded in its own binary tabulates only ten events, which is that
document's summary rather than its complete list, so it neither confirms nor
refutes the total.

What this pass *did* recount, first-hand, against two live configs rather
than documentation, is the correction to the brief's original "engine-only
events" framing — and the correction stands, on stronger footing than before.
The original brief claimed `PermissionRequest` and `PostCompact` were
Codex-only, `SubagentStart`/`SubagentStop` were Claude-Code-only, and
`postToolUseFailure` was Cursor-only. Two of those four are now confirmed
wrong by direct inspection of a config this machine actually runs, not by
reading a changelog:

- `lazytmux/claude-plugin/hooks/hooks.json` — lazytmux's Claude Code plugin —
  declares 14 events across 23 hook entries, and its event-key list includes
  `PostCompact`, `PostToolUseFailure`, `PermissionDenied`, `Elicitation`,
  `ElicitationResult`, and `StopFailure`, alongside the six converged names.
  `PostCompact` and `PostToolUseFailure` appearing in a Claude Code plugin
  directly disproves "Codex-only" and "Cursor-only" for those two.
- The live `~/.cursor/hooks.json` carries `postToolUseFailure` and
  `subagentStart` as native Cursor event keys. `postToolUseFailure` existing
  natively in Cursor, on top of its confirmed existence in the Claude Code
  plugin above, means it is not confined to either engine the original
  framing assigned it to.

Two qualifications, in the interest of not overstating what was actually
checked. First, the lazytmux list contains `PermissionDenied`, not the
literal `PermissionRequest` the original brief named — a related event, from
the same permission-flow family, but not the same string. This pass has
first-hand evidence that *a* permission-flow event exists outside Codex; it
does not have first-hand evidence that `PermissionRequest` specifically does.
That correction is carried from the prior pass, not re-verified here, and the
distinction is worth preserving rather than papering over. Second, this
pass's evidence for `SubagentStart`/`SubagentStop` spanning all three engines
covers only the Cursor half directly (`subagentStart` in the live
`hooks.json`); the Claude Code half is carried, since neither of this
machine's two inspected Claude Code configs happens to declare a subagent
hook. Net result, carrying the qualifications forward rather than silently
resolving them: of the original "engine-only" claims, only `Notification`
holds up as genuinely Claude-Code-only, and that conclusion now rests on two
first-hand config reads for its strongest two legs, with the
`PermissionRequest`/`PermissionDenied` and Claude-Code-side-of-Subagent legs
still carried rather than re-verified.

Field shapes diverge too — and this layer is no longer carried from
documentation. One `pre_tool` payload per engine was captured live from a real
tool call; the fixtures are in
[`fixtures/hook-payloads/`](fixtures/hook-payloads/), and they correct the
prior account in two places, one of which makes the envelope's job easier
rather than harder.

| | Claude Code 2.1.263 | Codex 0.153.4 | Cursor 2026.09.08 |
|---|---|---|---|
| session id | `session_id` | `session_id` | `session_id` |
| second id | `prompt_id` | `turn_id` | `conversation_id` + `generation_id` |
| working dir | `cwd`, populated | `cwd`, populated | `cwd` **empty**; real path in `workspace_roots[0]` |
| tool name | `tool_name: "Bash"` | `tool_name: "Bash"` | `tool_name: "Shell"` |
| tool args | `tool_input` (`command`, `description`) | `tool_input` (`command`) | `tool_input` (`command`, `cwd`, `timeout`) |
| call id | `tool_use_id` | `tool_use_id: "exec-…"` | `tool_use_id` |
| event name field | `hook_event_name: "PreToolUse"` | `hook_event_name: "PreToolUse"` | `hook_event_name: "preToolUse"` |
| also present | `permission_mode`, `effort`, `transcript_path` | `permission_mode`, `model`, `transcript_path: null` | `model`, `cursor_version`, `user_email`, `workspace_roots` |

**The correction that helps: all three engines send `session_id`.** The prior
pass had the session identifier renamed per engine — `session_id`,
`conversation_id`, `generation_id` respectively — and that is simply not what
they send. `session_id` is universal; what differs is the *second*, narrower
identifier, and it is `prompt_id`, `turn_id` and `generation_id` respectively.
§6's correlation key gets a stable field across all three instead of a
three-way rename.

**The correction that hurts: Cursor's `cwd` is the empty string.** Not absent,
not wrong — empty, with the real directory only in `workspace_roots[0]`. A
guard that reads `cwd` to decide anything gets `""` on Cursor and a real path
on the other two, which is the worst shape for a bug: it fails silently and
only on one engine. `git-default-branch-guard`, which has to know which
repository it is looking at, is exactly the kind of handler this breaks. The
envelope must therefore populate its canonical `cwd` from
`workspace_roots[0]` when the engine's own `cwd` is empty, and that fallback
is a translation rule, not a nicety.

**A third thing the capture settles, which no field table would show.** No
engine's payload carries any indication of *which config source registered
the hook* — no `source`, `origin`, `config_path`, or equivalent in any of the
ten captured payloads. That is what decides §8's sink-4 question, and it is
dealt with there. What the payloads do carry is an unambiguous *engine*
discriminator: `cursor_version` appears only in Cursor's, `effort` and
`prompt_id` only in Claude Code's, `turn_id` only in Codex's. The router can
always tell which engine it is talking to; it cannot tell which file told the
engine to call it.

The prior pass also described Cursor as splitting tool events by protocol
(shell, MCP, file) *instead of* exposing a generic pre/post-tool-use pair.
That part does not survive this pass's evidence and is dealt with below,
because it changes which native keys hookyard actually registers.

**Rule for events: the six converged concepts get one canonical name each**
(`session_start`, `prompt_submit`, `pre_tool`, `post_tool`, `pre_compact`,
`turn_end`). **Everything else is routed under an explicit engine-scoped
name** (e.g. `codex:PermissionRequest`, `cursor:afterAgentThought`) — never
silently dropped, never invented a fake canonical mapping it doesn't have. A
handler subscribes either to a canonical name, which fires across every
engine that has an equivalent, or to an explicit engine-scoped name, which
fires only for that one engine's literal event.

Cursor's tool events need an explicit call, and this pass's evidence changes
what that call is. The prior pass described Cursor as splitting tool events by
protocol — `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile` and
their `after*` counterparts — and treated that split as Cursor's *primary*
realization of pre/post-tool-use, with the generic pair as a fallback. **The
live `~/.cursor/hooks.json` on this machine contradicts that.** Its key set is
exactly `sessionStart`, `beforeSubmitPrompt`, `preToolUse`, `postToolUse`,
`postToolUseFailure`, `preCompact`, `stop`, `subagentStart`. The generic pair
is present and deployed; none of the three protocol-split names appears at
all, in a file four independent writers actively maintain.

Two readings survive that, and the document does not have the evidence to
choose between them: Cursor may expose both families, with the protocol-split
names real but unused by anything on this machine, or the prior pass may have
carried them from documentation that describes a different version or a
different surface. Nothing was checked against Cursor's own docs this pass.

**The design resolves this by emitting what is actually deployed.** hookyard
registers Cursor's `preToolUse` and `postToolUse` for the canonical
`pre_tool` and `post_tool`, because those are the keys observed in a live
config. It emits **exactly one** Cursor entry per canonical event — emitting
both families would fire the router twice for one tool call, which §8's dedup
argument relies on being structurally impossible.

The `protocol` field stays in the envelope, and its role is narrower than the
prior framing gave it: when a protocol-split event *is* what fired, the field
records which one, so a handler that cares can tell. It is empty otherwise,
and it is not the mechanism by which Cursor's tool events reach `pre_tool`.
If the protocol-split family turns out to be the one Cursor actually delivers
on some version or surface, the collapse rule the prior pass specified is the
right one and hookyard registers those names for `pre_tool`/`post_tool`
**instead of** the generic pair, not in addition to it. Adding them would
violate the one-entry-per-canonical-event invariant above unless the two
families are known to be mutually exclusive per tool call, and that is exactly
what is unverified. §12 carries the verification.

The envelope itself:

```
{
  "engine":         "claude-code" | "codex" | "cursor",
  "canonical_event": "pre_tool" | "" ,   // "" when this event has no canonical equivalent
  "native_event":    "PreToolUse",       // the engine's own literal event name, always present
  "session_id":      "...",              // normalized from session_id / conversation_id / generation_id
  "cwd":             "...",              // normalized from cwd, or Cursor's per-event cwd
  "protocol":        "shell" | "mcp" | "file" | "",  // set only when a protocol-split event fired; empty otherwise, including for Cursor's deployed preToolUse
  "tool_name":       "...",              // present on pre_tool / post_tool
  "tool_input":      { ... },            // engine-native shape, see handler-compatibility below
  "native": { ... raw, untranslated engine payload ... }
}
```

`native` exists specifically so that engine-only fields (Cursor's
`workspace_roots` array, Codex's own extra fields) are never silently dropped
or invented into a shape they didn't come in — a handler that needs them
reads `native` directly; the top-level fields are the ones every handler can
expect to exist for the events it subscribed to.

### The handler-side compatibility requirement

The cross-engine reuse case rests entirely on reusing the four `agent-hooks`
guard binaries across all three engines, not just keeping them working on
Claude Code. That requirement forces a specific decision on both the inbound
and outbound schema above, not just a compatibility footnote.

Confirmed this pass, from `agent-hooks/default.nix`'s own header comment
(lines 1-7): the guards speak Claude Code's PreToolUse contract, read its raw
fields via `jq`, abstain — silently, by design — on anything that doesn't look
like that shape, and write a raw `hookSpecificOutput` object to stdout. The
header names **both** arms of that object, and the second is easy to miss:
"`permissionDecision: "deny"` blocks; `additionalContext` is advisory." The
contract was always two-armed. What an earlier rendering of it dropped was the
advisory half, and the next bullet is about what that omission would have
cost. They do not speak
a generic envelope or a generic verdict schema; they speak exactly one
dialect, Claude Code's own. That is the header's own description of the
contract; the specific field list a given guard reads (`.tool_name`,
`.tool_input.file_path` / `.path` / `.command`, and similar) was verified by
reading the individual guard scripts in the prior pass and is carried here,
not re-read line-by-line this pass — the header comment confirms the general
contract, not each script's exact `jq` paths.

If the router handed these guards the generic-looking envelope sketched
above unchanged, every one of them would read an empty `tool_name` (it's
nested differently, or the top-level field simply isn't spelled that way)
and silently abstain on **every single call, on every engine** — and fail-open
makes that failure invisible by construction: no deny ever fires, no error
is ever printed, and the guard simply stops protecting anything. This is
exactly the dangerous combination this section has to resolve, not just name.

**Resolution: Claude Code's own PreToolUse JSON shape *is* the handler wire
protocol, for all three engines, unmodified.** Concretely, and scoped
precisely to what the claim can actually deliver:

- The envelope is a superset of **the specific fields the guards actually
  read** — `tool_name`, and `tool_input.file_path` / `.path` / `.command` /
  `.pattern` / `.output_mode` / `.glob`, the last re-checked directly in
  `secret-read-guard.sh` this pass — not a literal, field-for-field superset of
  every field in Claude Code's raw hook JSON. Claude Code's own raw
  `PreToolUse` payload also carries a `transcript_path` field and spells the
  event name `hook_event_name`; the envelope above carries neither under
  those names (it uses `native_event`, and `transcript_path` would only be
  reachable through the `native` passthrough). No guard reads either field
  today, so this doesn't break anything, but the claim is a superset of the
  guard-relevant fields, not of the raw JSON shape as a whole. If a future
  guard needs `transcript_path` or a literal `hook_event_name`, promote it to
  a top-level envelope field at that point rather than assuming it's already
  covered.
- **The harder half of this claim is the tool-name mapping itself, and it has
  to be shown working, not just asserted.** Both guards branch on Claude
  Code's own literal tool-name strings (`secret-read-guard.sh`: `case
  $tool_name in Read) ... Grep) ... Bash) ...`; `git-default-branch-guard.sh`:
  `[[ $tool_name == "Bash" ]]`), so the router's translation step has to land
  on those exact strings for a Codex or Cursor event, not merely avoid
  crashing on one. This is not hypothetical to check: aeye's own Codex and
  Cursor adapters declare hook *matchers* naming each engine's tool
  vocabulary for the equivalent behaviors, and this pass re-grounded that
  first-hand, against the files themselves rather than trusting the prior
  doc's citation: `aeye/adapters/codex/plugin/hooks/hooks.json` matches
  `apply_patch`, `view_image`, and `Bash`; `aeye/adapters/cursor/hooks.json`
  matches `Read`, `Write`, and `Shell`;
  `aeye/adapters/claude-code/plugin/hooks/hooks.json`
  declares no matcher key at all, which is expected — it needs none, since
  Claude Code's own vocabulary is the target the other two are being mapped
  onto:

  | Native tool identifier | Engine | → Claude-Code-shaped `tool_name` |
  |---|---|---|
  | `Bash` | Codex | `Bash` (already identical — no mapping needed) |
  | `apply_patch` | Codex | `Write` (Codex's file-write mechanism; exact sub-tool, e.g. `Edit` for partial patches, is unresolved — see below) |
  | `view_image` | Codex | `Read` |
  | `Read` | Cursor | `Read` (already identical) |
  | `Write` | Cursor | `Write` (already identical) |
  | `Shell` | Cursor | `Bash` |

  Both engines' vocabularies match exactly what this table claims — the
  matcher strings found this pass are the same three per engine the prior
  doc's table names, so the table stands re-grounded, not merely re-asserted.
  Two of the six rows need **no translation at all** — Codex already calls
  its shell tool `Bash`, and Cursor already calls its read/write tools `Read`
  and `Write`, matching Claude Code's own vocabulary by coincidence of prior
  convention, not by this design's doing. The remaining three rows are a
  small, enumerable table, not an open-ended free-form mapping problem. What
  is **not yet resolved**, and stays open rather than folded into this
  "resolved" claim: whether Codex's single `apply_patch` tool should always
  map to `Write`, or needs to distinguish an edit-in-place case that guards
  might want to treat as `Edit`. That is one finite follow-up item against
  Codex's own hook documentation, not open-ended design risk.

  **The Cursor half of that question is closed, by Cursor.** `cursor-agent`
  ships this exact mapping itself, as a Claude-Code-vocabulary-to-Cursor
  converter — the table below is read off the bundle, not derived here:

  | Claude Code | → Cursor | Note |
  |---|---|---|
  | `Bash` | `Shell` | the inverse of this document's `Shell` → `Bash` row |
  | `Read` | `Read` | identical |
  | `Write` | `Write` | identical |
  | `Edit` | `Write` | **collapsed** — Cursor has no separate edit tool |
  | `Grep` | `Grep` | identical |
  | `WebFetch` | `WebFetch` | identical |
  | `WebSearch` | `WebSearch` | identical |
  | `Task` | `Task` | identical |
  | `Glob` | *(none)* | dropped, with a warning that the tool "is not supported in Cursor and will be ignored" |
  | `mcp__<server>__<tool>` | `MCP:<tool>` | server segment discarded |

  Three things follow for hookyard, and the third is a defect this document
  would otherwise have shipped. First, the protocol-split worry is answered:
  a Cursor tool event's identifier for the shell protocol *is* literally
  `Shell`, and MCP tool calls arrive under the `MCP:<tool>` form, so there is
  no third spelling for the `protocol` field to disambiguate. Second, the
  direction of the `Edit`/`Write` collapse is settled for Cursor by Cursor:
  `Write` is the canonical target and `Edit` folds into it, which is what this
  document's table already assumed for the Codex side. Third — and this is the
  new obligation — **`Glob` has no Cursor equivalent and is silently dropped.**
  A manifest entry that matches on `Glob` and expects three-engine coverage
  gets two. `hookyard validate` should reject, or at minimum warn on, a
  `match` that renders to an empty matcher for any engine the entry claims,
  because the failure mode is otherwise exactly §5's fail-open with no record:
  a handler that was never registered cannot abstain, error, or time out, and
  so leaves no trace in the stream at all.
- **The outbound schema is the same reuse — and it has three arms, not two.**
  Rather than inventing a second verdict format that handlers must learn, the
  handler-facing wire protocol *is* `hookSpecificOutput` on stdout: a
  `permissionDecision` (+ `permissionDecisionReason`) to decide, an
  `additionalContext` to advise, or silent exit 0 to abstain.

  **The advisory arm is not optional, and getting this wrong would have been
  a live regression.** Checked directly this pass:
  `git-commit-autostage-guard.sh` emits **only** `additionalContext` and never
  emits `permissionDecision` at all — its entire job is to tell the model that
  unstaged tracked changes will be stashed by pre-commit. The other three
  guards emit `permissionDecision`. A router that recognized only the decision
  arm would classify that guard as `abstain` on every call and silently drop
  its advice, which today reaches the model on Claude Code. It would look
  exactly like a guard that had nothing to say.

  So `additionalContext` is a first-class outcome, carried end to end: the
  handler outcome vocabulary is `allow`/`deny`/`ask`/`advise`/`abstain`/
  `error`/`timeout`; `advise` does not participate in the deny-wins lattice at
  all (it is not a verdict), and the router concatenates every advisory string
  it collected into the native response alongside the consolidated decision,
  in whatever slot the engine offers for it. Where an engine has no such slot,
  the advice is recorded (§6) and not delivered, and that is a stated loss
  rather than a silent one.

  This is also the clearest evidence that §4.1's end-to-end run does not prove
  as much as it appears to. On the deny payload the other three guards exited
  0 with no output, so the advisory path was never exercised — the run
  confirms the decision arm and says nothing about this one. The router's
  internal `abstain`/`allow`/`deny`/`ask`
  representation is what the router uses *between* itself and its own
  consolidation logic; at the handler boundary, on the wire, it is Claude
  Code's own shape, for every handler, on every engine. A new handler written
  specifically for this registry gains nothing from inventing a different
  wire format, so it doesn't get one.
- Net effect: **the four existing `agent-hooks` guards run unmodified**
  against this design, on Claude Code, Codex, and Cursor alike, *for the tool
  vocabulary in the table above, and only because the advisory arm above is
  part of the protocol*. Three of the four speak the decision arm; the fourth
  speaks only the advisory arm. No shim, no migration, no rewrite. This is
  a binding constraint on both schemas above, not a compatibility note bolted
  on afterward — it is *why* the envelope's field names are Claude Code's own
  instead of a fresh generic vocabulary. The remaining mapping gaps
  (`apply_patch`'s exact sub-tool, Cursor's protocol-split tool identifiers)
  are small and enumerable, not evidence the approach is wrong — but they are
  unresolved, and this document does not claim otherwise.

### Outbound: rendering the consolidated verdict per engine

The router's internal verdict (`abstain` / `allow` / `deny` / `ask` + a
reason string) has to become one native decision per engine, and any advisory
strings it collected have to ride alongside it:

| Engine | Verdict rendering | Advisory rendering | Confirmed? |
|---|---|---|---|
| Claude Code | `hookSpecificOutput.permissionDecision` = `allow`/`deny`/`ask`, `permissionDecisionReason` = reason | `hookSpecificOutput.additionalContext`, the concatenation of every advisory collected | Verdict yes — documented field, tri-state including `ask`. Advisory arm confirmed by an existing guard emitting it |
| Codex | Unconfirmed | Unconfirmed | Only fire-and-forget hooks observed deployed; Codex's deny path, its advisory slot if any, and its default timeout when an entry declares none were none of them verified this pass |
| Cursor | `permission` field | Unconfirmed | Field name confirmed; exact accepted value set (binary vs. tri-state) not confirmed this pass, and no advisory slot identified |

**Where an engine has no advisory slot, the advice is recorded (§6) and not
delivered.** That is a real loss and is stated rather than hidden: a handler
whose entire purpose is advisory — `git-commit-autostage-guard` is one —
contributes nothing the model can see on an engine with nowhere to put it.
Recording it means the loss is visible in the stream rather than silent, which
is the same trade §5 makes everywhere else.

**When a guard returns `ask` and the target engine has no equivalent (a
binary allow/deny engine), the router degrades `ask` to `deny`, never to
`allow`.** An `ask` is a request for a human decision the router cannot make
on its own; an engine that can't surface that request synchronously should
not have it silently resolved in the permissive direction. This applies to
Cursor unless and until its `permission` field is confirmed to support a
genuine third state, and it is the deliberately conservative default for any
future engine whose decision shape turns out to be binary.

## 8. Native config emission, registration, and coexistence

### One declarative table, rendered per engine

The registry's source of truth is a single table — **guard × event ×
engine** — owned by hookyard and by nothing else. Each row names a handler
id, the canonical or engine-scoped event it subscribes to (per §7's naming
rule), the engines it applies to, the binary that gets executed, and an
optional per-handler timeout override that must fit under §4's sub-budget.
That table is the only thing in this design that knows the full picture; the
three native config formats are *renderings* of it, not independent sources
of truth, and no engine's file is ever read back to reconstruct what the
table says.

Rendering happens at install time, from hookyard's own install command. **It
is invoked exactly once per activation, with the full merged manifest list —
never once per consumer repo.** This is an invariant, not a preference, and
getting it wrong is silently destructive: hookyard's strip is whole-file and
keyed on a marker (`/bin/hookyard`) that does not distinguish which manifest
produced a row, so a second per-repo invocation would strip the first repo's
rows and write only its own. Three repos' hooks would vanish with no error on
the next rebuild.

That is a real departure from how the existing writers work, and it is worth
naming as such. aeye's installer and the dispatcher block *are* separate
per-repo activation entries, and they get away with it because each strips
only its own marker. hookyard cannot copy that shape, because its whole point
is that one table spans every repo — a per-repo strip would have to know which
rows belong to which manifest, which means encoding manifest identity in the
marker, which reintroduces the orphaning problem the whole-file strip exists
to avoid.

So the wiring differs from the per-repo pattern in one specific way:
**nix-config owns the aggregation.** Consumer repos' home-manager modules
contribute manifest paths to a single list; one activation entry passes that
whole list to one hookyard invocation. That is still nix-config wiring paths
rather than owning logic — the list is paths, and the table is hookyard's —
but it is a shared list rather than four independent blocks, and a fifth repo
joins by adding its path to it. §9 covers what path the command and its
emitted entries carry. The three targets, and what already lives in each:

| Engine | Target file | Mechanism | Pre-existing writers |
|---|---|---|---|
| Cursor | `~/.cursor/hooks.json` | `jq` merge: validate, marker-scoped strip, append, atomic rename | four declared, three live |
| Codex | `~/.codex/config.toml` | marker-guarded `sed`/heredoc append | lazytmux only, two blocks |
| Claude Code | `~/.claude/settings.json` | JSON merge under the same marker discipline | hand edits, the Nix `--settings` overlay, plugin `--plugin-dir` trees, and houston's installer as a *potential* writer (it has never run here) |

**Cursor.** Independent `jq` mergers already write `~/.cursor/hooks.json`,
and this pass counts **four declared, three currently live** (§1): aeye's
`adapters/cursor/install.sh`, stripping by the path substring
`/adapters/cursor/scripts/`; nix-config's inline dispatcher-stop activation
block, stripping by `dispatcher-cursor-notify`; lazytmux's
`cursor-hooks-install.sh`, marker `/bin/cursor-status-hook`; and lazytmux's
`cursor-relaunch-hooks-install.sh`, marker `/bin/cursor-relaunch-stamp`,
gated on `resumeCursorEnable` and not currently materialized on this
machine. This design does **not** propose replacing them. Consolidating four
hand-rolled mergers into one is a separate, later change that "no flag day"
explicitly rules out doing as a precondition; hookyard's Cursor writer
instead follows the idiom the existing ones already established and becomes
one more independent writer alongside them. Call the mechanism **the
marker-scoped independent-writer idiom**. Deliberately attach no ordinal to
hookyard: "the fourth writer" is already wrong this pass, and would go stale
again the next time a writer is added, removed, or gated — as one already
has been.

Read first-hand, the idiom is more specific than "merge with jq", and worth
stating precisely because hookyard has to reproduce it exactly: validate the
*inherited* file with `jq empty` and **refuse rather than clobber** if it is
malformed (both live writers bail with a message and a non-zero exit);
strip only entries whose `command` contains your own marker; append your
rows; write through a temp file and `mv`, so a reader never sees a partial
file. One difference between the two live implementations is worth
inheriting from the stricter side: lazytmux strips its marker across *every*
event key (`with_entries`), while nix-config's dispatcher block strips only
within `.hooks.stop`. hookyard needs the whole-file variant, because its
table can legitimately move a handler from one event to another between
versions, and a key-scoped strip would leave the old entry orphaned under
the old key — firing forever, owned by nobody.

Worth noting against any impression that these writers merely coexist by
luck: nix-config orders two of them explicitly, with
`lib.hm.dag.entryAfter ["aeyeCursorHooks"]` on the dispatcher block. That is
real coordination machinery, not an accident. hookyard's writer does not
need to be ordered against the others — marker-disjoint writers commute —
but it does need the same two environmental facts the existing blocks
already encode: it runs after `writeBoundary`, and it runs with `jq` and
`coreutils` pinned onto `PATH`, because `home/ai/cursor/default.nix` pins
them precisely on the stated grounds that the activation environment
"doesn't guarantee jq".

**Codex.** `home/ai/codex/default.nix` never touches `agent-hooks` — this is
the gap hookyard closes, and it is the one engine where the four shared
guards have no route in at all today. Confirmed this pass: the hook content
of `~/.codex/config.toml` is entirely lazytmux-managed, two marker-guarded
blocks (`# lazytmux-managed: codex status-line hooks` and
`# lazytmux-managed: codex resume-on-restore SessionStart hook`), with zero
aeye content. hookyard appends its own marker-guarded block using the same
`sed`/heredoc-append mechanism, **not** a jq- or TOML-level rewrite of the
file, for the reason lazytmux's own comment gives: `config.toml` carries
substantial hand-edited content — model selection, `mcp_servers`,
per-project trust — that templating the whole file would clobber.

Be precise about what is and is not present here, because it is easy to
overstate in both directions. `~/.codex/config.toml` **does** exist on this
machine and was read directly this pass: two lazytmux marker blocks, ten hook
entries, every one carrying `timeout = 30`, ten profile paths and no store
paths, under the keys `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `PreCompact`, `Stop`, `PostCompact` and `PermissionRequest`.
That file is first-hand evidence for Codex's hook-block TOML syntax, its
`timeout` key, its emitted path form, and six of the event names §7's
convergence table lists — stronger grounding than §7's own attribution to a
documentation read.

nix-config's own codex module *is* materialized here, contrary to what an
earlier pass recorded: `home/ai/default.nix` gates it on
`osConfig.profile == "work"`, and this host is a work-profile host, so
`~/.codex/agents` and `~/.codex/AGENTS.md` both resolve (§11, claims 2 and 3).
On a personal-profile host the gate would close and those paths would be
absent — but the gate affects nix-config's Codex wiring, not `config.toml`
itself, which lazytmux writes either way.

**Claude Code.** This is the engine with the most existing writers, and the
only one where the choice of surface has a real failure mode rather than a
stylistic preference. Three surfaces already exist. The first is
`~/.claude/settings.json`, which is hand-edited and which houston's installer
self-installs into. The second is the Nix `--settings` overlay, which
deep-merges object-valued keys **with the overlay winning per top-level event
key, not per array entry**. The third is the plugin `hooks.json` directories
loaded via `--plugin-dir`, which is where lazytmux's and aeye's Claude Code
hooks actually live.

The overlay's hook inventory, re-counted first-hand this pass:
`home/ai/claude-code/default.nix` declares **11 hook entries** —
`SessionStart` 1, `PreToolUse` 9 across three matcher groups (Bash 6, Read
2, Grep 1), `PostToolUse` 1 — of which **6 invoke the four shared
`agent-hooks` guards**: `nix-stage-guard`, `git-commit-autostage-guard` and
`git-default-branch-guard` once each on the Bash matcher, and
`secret-read-guard` three times, on the Bash, Read and Grep matchers. The
brief's figure of ~15 is not it.

This pass can be more precise about where ~15 came from than the prior pass
was, and the correction is worth recording because it is mechanically
reproducible. `grep -c 'command = '` over that file returns exactly **15**:
the 11 hook entries plus the `statusLine` command and three
language-server commands (`nixd`, `bash-language-server`,
`pyright-langserver`) that are not hooks at all. That is the likeliest
source of the overcount — a whole-file count of `command =` in a file where
`command` is also the key for a status line and three LSPs. The prior pass
attributed the overcount instead to conflating `default.nix`'s entries with
plugin-sourced ones, which remains a real and separate surface: the file's
own comment states that "status-state hooks live in the lazytmux CC plugin
and image-capture hooks in the aeye CC plugin (both loaded via
`--plugin-dir` in the wrapper below); only personal hooks remain here". Both
explanations describe genuine extra `command` entries a naive count would
sweep in. This document carries the mechanically reproducible one as the
likely cause and the plugin-directory one as the standing fact about the
surface, rather than asserting either as the proven origin of a number
neither pass observed being produced.

**The collision risk does not exist in the form this document feared, and the
correction is a reversal rather than a refinement.** The feared behaviour was
that a `PreToolUse` entry written into `settings.json`, competing with the
overlay's nine entries across three matchers, would be discarded outright and
silently by the overlay. That is not what Claude Code does. Its hook-capture
step enumerates three settings sources — `userSettings`, `localSettings`,
`flagSettings` (the `--settings` overlay) — and **flat-maps** their `hooks`
objects into one list. Hooks *union* across sources; no source overrides
another's entries. The only de-duplication applied is by the *resolved real
path of the settings file*, so two sources that turn out to be the same file
on disk are counted once — and `flagSettings` is explicitly exempt even from
that. Ten entries would be registered from the case above, and all ten would
run.

Both halves of the earlier conclusion therefore fall. Nothing is silently
discarded, so there is no data-loss risk to mitigate; and the operational
rule that followed from it — that nix-config must cede the top-level event
keys **before** hookyard's installer may be pointed at `settings.json` — is
not required by the merge semantics and is withdrawn. The honest status of
the Claude Code `settings.json` path on a Nix-managed machine is **not
blocked**: hookyard can write there while the overlay keeps its own entries,
and neither writer needs to know about the other. That is the same
"coexist as one more independent writer" property §8 already relies on for
`~/.cursor/hooks.json`, and it turns out to hold for Claude Code for a
different underlying reason — a union rather than a marker-scoped strip.

What replaces the retired risk is the *other* failure mode a union produces,
which is the one §8's dedup discipline is already built for: if hookyard is
registered in the overlay **and** in `settings.json`, it is registered twice
and runs twice per event. Marker-scoped single ownership — one writer owns
hookyard's entry, wherever it chooses to put it — is what keeps that from
happening, and it is now the only thing that has to be true, rather than a
precondition on nix-config dropping keys. `~/.claude/settings.json` still has
**no `hooks` key at all** today (`jq 'has("hooks")'` → `false`), so no entry
of either kind exists yet.

The plugin `--plugin-dir` surface remains worth evaluating, but for a reason
that has shrunk: it is a fourth hook source that does not participate in the
settings merge, which is a tidiness argument rather than the escape hatch
from a clobbering merge it was previously being held in reserve as.

**One genuinely new failure mode surfaced while reading this path, and it is a
fail-open one §5 does not cover.** The same code gates hooks off entirely, not
per-entry, under a list of conditions: `disableAllHooks` set in user or flag
settings, an `allowManagedHooksOnly` policy, safe mode, a plugin-only
restriction, bare mode, an unreadable policy file — and, the one most likely
to be met by accident, **an untrusted workspace**, which skips every hook with
the internal reason `untrusted_workspace`. `localSettings` hooks are dropped
separately when the workspace is untrusted or when `settings.local.json` is
git-tracked (reason `repo_provenance`), and `flagSettings` hooks are skipped
when a runtime patch supplies its own. Every one of these produces guards that
do not run, on a machine where they appear to be installed, with nothing in
hookyard's own record to show it — because a handler that was never invoked
cannot abstain, error, or time out. A guard silently disabled by workspace
trust is indistinguishable, from inside the record, from a session where
nothing dangerous was attempted. `hookyard doctor` is the right place to
answer it: it can read the same settings sources and report whether hooks are
gated off, rather than leaving the operator to infer coverage from an empty
stream.

**The trust gate is not a Claude Code quirk — all three engines have it, and
the payload-capture run hit it on every one.** Cursor refused to run in a
fresh directory until passed `--trust`, warning that the agent "can execute
code and access files in this directory". Codex prompted before loading
anything and said exactly what was at stake: "Trusting the directory allows
project-local config, **hooks**, and exec policies to load." Claude Code's is
the one read from code rather than met head-on, since its probe run was given
a pre-seeded trust record. So "hooks do not run in an untrusted workspace" is
a uniform property of all three engines rather than one engine's
idiosyncrasy, and it is the likeliest way for a guard to be silently absent
on a machine where it is correctly installed. `hookyard doctor` should report
workspace trust per engine for the directory it runs in; that is the check,
not an extra beside it.

**Codex keeps directory trust in the same file hookyard writes, which
constrains the installer.** `~/.codex/config.toml` holds both `[hooks.state]`
(per-entry hook trust, §9) and `[projects."<dir>"] trust_level = "trusted"`
(per-directory trust). This was confirmed the hard way during the capture
run: rewriting that file wholesale dropped a directory trust granted minutes
earlier, and Codex prompted for it again. An installer that regenerates
`config.toml` would silently revoke every project the user has trusted and
every hook they have reviewed — worse than losing a hook entry, and
experienced by the user as Codex abruptly distrusting their machine.
Preserving `[projects]` and `[hooks.state]` is therefore a hard requirement
of the Codex writer, not the tidy option: the marker-scoped strip must carry
both through the atomic rename untouched.

houston's own installer is named here for exactly one reason: it is another
writer to `~/.claude/settings.json` that hookyard must be able to coexist
with — potential rather than pre-existing, since it has not actually written
here. It is not hookyard's installer, it is not kept and generalized, and
nothing in this design extends it. **The installer is hookyard's own
command**, shipped in hookyard's own binary. Two observations follow from
treating houston as a co-writer rather than a host. First, it has not
written here: the absence of a `hooks` key is also evidence that houston's
installer has never run on this machine, and nix-config wires no `houston
hook` invocation anywhere (checked this pass, no matches). Second, because
hookyard's writer is marker-scoped like every other writer in this section,
coexistence with houston's entries needs no negotiation — the same property
that lets four unrelated Cursor writers share one file.

### Registration interface

A handler-owning repo declares its handlers in a small manifest in its own
repo, next to the binaries the manifest describes. Per handler: an id, the
binary's path, the canonical and/or engine-scoped events it subscribes to,
which engines it applies to, and an optional timeout override under §4's
sub-budget. Illustratively, and not as a committed schema:

```json
{ "handlers": [
  { "id": "aeye/images", "exec": "<path>/images.sh",
    "events": ["post_tool"], "engines": ["claude-code","codex","cursor"],
    "match": ["Read","Write","Bash"], "timeout_ms": 1500 }
] }
```

**`match` is written in the normalized vocabulary, never an engine's native
one.** That is what makes a manifest cross-engine: a handler declares that it
cares about reads, writes and shell calls once, in the `Read`/`Write`/`Bash`
terms §7 normalizes onto, and hookyard translates in both directions — it maps
each native identifier to the normalized `tool_name` on the way in (§7's
table), and renders the corresponding native matcher strings on the way out,
so a Codex entry gets `apply_patch|view_image|Bash` and a Cursor entry gets
`Read|Write|Shell` from the single `match` above. A manifest that spelled
`Shell` would be declaring Cursor's native word in a field that is not
engine-scoped, which is exactly the ambiguity the normalization exists to
remove; manifest validation (below) rejects identifiers outside the
normalized set, which today is exactly `Read`, `Write`, `Bash`, `Grep` and
`Glob` — Claude Code's own vocabulary for the tools the existing guards match
on. The set grows only when §7's mapping table grows, which is why §12 item 6
(whether `Edit` needs to join it for Codex's `apply_patch`) is a question about
this set and not only about that table.
hookyard is pointed at a list of manifest paths, supplied the way nix-config
already wires each repo's hook scripts today: as Nix-supplied paths in each
repo's own home-manager module. It merges every manifest it is given into
one table and renders native config from the merged result. **hookyard does
not know about participating repos in advance** — there is no list of
consumers compiled into it, and adding a fifth repo is a wiring change in
nix-config plus a manifest in that repo, not a hookyard release.

This keeps the source of truth for *what a handler does and when it fires*
inside the repo that owns the handler, versioned alongside it, while
hookyard stays the one place that knows how to render three different native
formats. It mirrors the existing split rather than inventing one: nix-config
wires paths and never owns hook logic, and this design adds one more kind of
path for it to wire, not a new kind of thing it has to understand.

### Manifest trust, validation, and safe rendering

A manifest is a list of binaries hookyard will execute on the critical path of
every tool call, so where its authority comes from has to be stated rather
than inherited by implication.

**The trust root is nix-config's own review.** Manifests reach hookyard only
as Nix-supplied paths from a home-manager module, which means a manifest can
only enter the table by way of a change to nix-config that someone merged.
That is exactly the trust model today's hand-written hook scripts already
have, and hookyard neither weakens nor strengthens it: it performs no
signature check, no integrity check, and no provenance check beyond parsing.
It is worth saying out loud because manifests are now the mechanism by which
new subprocess executions get wired into a gate that fires on every tool call
across three engines — the review that admits one is the whole of the
control.

**Validation is on entry, and failure is loud.** Every manifest is validated
before it reaches the table: fields hookyard does not recognize are ignored,
so a manifest written against a newer hookyard degrades rather than breaks,
but a manifest that cannot be parsed, or whose `exec` does not resolve to an
executable path, fails the install outright. An install that half-succeeds
would leave three engines' configs describing a table that never existed.

**Handler ids are unique, and collisions fail the install.** Manifests come
from independently maintained repos, so two of them can name the same
handler id or claim overlapping `(event, engine, match)` tuples. Neither is
resolved silently. A duplicate `id` across merged manifests is an error, not
a last-writer-wins override. Overlapping matchers are *allowed* — two guards
legitimately watching `Bash` is the normal case, and deny-wins is exactly
how their verdicts combine — but two entries identical in id are not, because
that is the case where one repo's change silently replaces another's guard.
The rule is the same shape as the unparsable-manifest rule: refuse the
install, name both manifests, change nothing.

**hookyard's marker must not appear in any other writer's command string.**
This is an invariant hookyard imposes on the shared files, and there is a live
precedent for why it matters. `home/ai/cursor/default.nix` wraps dispatcher's
notifier in a `writeShellApplication` for exactly this reason, stated in its
own comment: aeye's installer strips every entry whose command contains
`/adapters/cursor/scripts/`, and dispatcher's script lives at precisely that
path, so a direct reference would be deleted on aeye's next activation. That
is a genuine marker collision between two independent writers, worked around
with indirection. The claim that marker-disjoint writers commute is therefore
conditional on the markers actually being disjoint, which is not automatic.
hookyard's marker is `/bin/hookyard`, which no other writer's command
plausibly contains — but the invariant is stated rather than assumed, because
the one time it was violated on this machine it was resolved by a wrapper
script and a DAG edge, not by anything noticing.

**Rendering escapes, it does not splice.** Manifest-supplied strings end up
inside three different config formats, and one of those files carries
substantial hand-edited content the design has already committed to not
clobbering. Each renderer therefore encodes rather than concatenates: the
Cursor writer builds its entries as JSON values through `jq` (which is
already how the three existing writers do it), and the Codex writer emits
TOML through a real encoder rather than interpolating strings into a
heredoc. A manifest value containing a quote, a bracket, or a newline must
not be able to terminate hookyard's own block early and land content in the
surrounding `mcp_servers` or trust configuration. Values are additionally
constrained on entry — ids and event names to an identifier character set —
so the encoder is the second line of defence rather than the only one.

**The Codex writer gets the same write discipline as the Cursor one.** The
Cursor idiom this design commits to reusing is stricter than "merge with
`jq`": validate the *inherited* file first and refuse rather than clobber it,
strip by marker, write a temp file, rename atomically. The Codex path
inherits all four. Its file is parsed and validated as TOML before it is
touched; the strip and the append happen against a temp copy and land with
one rename. A strip-then-append performed in place is two operations on a
file full of hand-edited content, and a crash between them leaves a
half-configured machine. That was previously discounted on the grounds that
Codex was observe-only and so had little to lose; with Codex's deny path
confirmed (§4), a half-written Codex config now costs a guard that enforces,
which makes the atomic-rename discipline load-bearing rather than tidy.

### The dedup / double-firing story, re-derived

The prior doc's answer to double-firing rested on a claim that live-view
state writes are "self-healing by construction": an old direct write and a
new registry write both landed on one correlation-keyed entry in **one state
directory**, so `runs/registry.go`'s per-source merge collapsed them into a
single row rather than two. That is a **same-sink** argument, and it is
cited here only as prior art for what it demonstrates — that a keyed merge
over one store does collapse duplicate writers. Under the standalone premise
it cannot be carried, because its premise is gone: hookyard does not write
houston's state directory, does not key into it, and does not control the
merge that would collapse anything there.

Re-deriving it means asking, for each behavior, *which sink each writer
targets during the migration window*. There are three sinks in play, and
they behave differently.

**1. The handler's own sink — same sink, and the interesting case.**
hookyard does not replace handlers; it routes events to them. During
migration a handler can be invoked twice for one engine event: once through
its repo's old native entry, once through hookyard's. Both invocations are
the *same binary* writing the *same* place it always wrote. So this is a
same-sink case — but the merge that decides whether the duplicate is
harmless belongs to the handler, not to hookyard, and hookyard supplies no
correlation key that affects it. What matters is each handler's own write
semantics, and those differ, first-hand:

- aeye's `images.sh` appends to a per-pane manifest and says so in its own
  comment: "Append-only: no write-side dedup. Concurrent firings can emit
  duplicate (path,mtime) lines; the viewer collapses them on read". A
  double-fire here is genuinely harmless — but by aeye's reader collapsing
  identical records, not by any key merge, and that is a property of aeye's
  viewer that hookyard neither provides nor can promise on aeye's behalf.
- dispatcher's `dispatch-notify.sh` is the other extreme, and it is worse
  than "not idempotent". It calls `tmux display-message` (a visible,
  irreducible side effect: two invocations, two messages) and appends a
  `status`/`exited` record to `$GIT_COMMON_DIR/crew/events.jsonl`. That
  append is guarded — but the guard skips only when the last recorded state
  is `done`, `failed` or `pr_open`. `exited` is **not** in that set, so a
  second invocation appends a second `exited` row even when it runs strictly
  after the first and reads the first's record. The guard was written
  against a different problem (not overriding a meaningful terminal status),
  and it does not suppress a duplicate of itself.

**2. hookyard's own always-on record (§6) — single-writer, and incomplete
during migration.** Nothing else writes it, so it has no dedup problem at
all: one hookyard entry per engine event, and the marker-scoped strip keeps
hookyard from ever installing two entries for one event against itself
(§9 has the version-bump case that would otherwise break this). The honest
limit runs the other way: firings that still go through a repo's *old*
native entry never pass through hookyard and are therefore **absent** from
its record. During migration hookyard's record is a complete account of what
hookyard routed, not of what the engine fired. §5 and §12 lean on the record
to make a silently-disabled guard recoverable after the fact; that argument
holds only for behaviors already migrated, and this section is where the
qualification is stated rather than discovered later.

**3. houston's state directory — genuinely two sinks, and not hookyard's to
reconcile.** An unmigrated behavior writing houston's state directory
directly, and hookyard writing its own stream that houston subscribes to,
are two different stores. A shared correlation key merges nothing across
them: keys collapse rows *within* a store. If houston ends up seeing one run
through both paths, resolving that is houston's own layering problem — its
`Registry` already composes several sources under fixed precedence, which is
cited as prior art that the shape is tractable, not as a mechanism hookyard
depends on or may assume is installed.

**4. Cursor's Claude-Code import — a double-fire path no per-engine
discipline prevents, and it is not hypothetical.** Every sink above assumes
one engine reads one config. `cursor-agent` does not. Its hook resolution
enumerates nine sources, and three of them are Claude Code's:
`enterpriseHooks`, `teamHooks`, `userHooks` (`~/.cursor/hooks.json`),
`projectHooks` (`.cursor/hooks.json`), `runtimeHooks`, then
**`claudeUserHooks`** (`~/.claude/settings.json`), **`claudeProjectHooks`**
(`.claude/settings.json`), **`claudeProjectLocalHooks`**
(`.claude/settings.local.json`), and finally plugin hooks. It ships a
converter for them, which is where the Claude-Code-to-Cursor tool mapping in
§7 comes from — it rewrites each imported entry's matcher into Cursor's
vocabulary and fills in Cursor's own `loop_limit` and `failClosed` fields.

The consequence lands directly on §8's central commitment. "One declarative
table, rendered per engine into that engine's native config" quietly assumes
the renderings are disjoint at runtime. They are not: an entry rendered into
`~/.claude/settings.json` **and** an entry rendered into
`~/.cursor/hooks.json` both reach Cursor, which will run the handler twice
for one Cursor event. Marker-scoped single ownership does not help, because
neither writer is duplicating the other — each is the sole owner of its own
engine's file, and the duplication happens inside Cursor's resolver. This is
the one double-firing case in this section that is a property of the *design*
rather than of a migration window, so it does not age out when the last
behavior migrates.

**This is now decided, by the payload capture.** The two candidates were:
collapse the engines into one registration and lose per-engine matcher
control, or keep both registrations and have the router suppress the imported
duplicate by its provenance. The second was the better design and depended on
a fact nobody had checked — whether an inbound payload says which config
source registered the hook. It does not. None of the ten captured payloads
(§7) carries a `source`, `origin`, `config_path` or any equivalent, on any
engine. Provenance-as-read-from-the-payload is not available, so the
suppression rule as originally framed cannot be written.

It survives in a different form, because hookyard does not need the engine to
tell it something hookyard itself wrote. **The router supplies its own
provenance through the command line it emits.** §8 already renders one entry
per engine from one table row, so each rendering can carry its own engine tag
in argv — the `~/.cursor/hooks.json` entry invokes the router with
`--registered-for cursor`, the `~/.claude/settings.json` entry with
`--registered-for claude-code`. The payloads then supply the other half: they
carry an unambiguous engine discriminator even though they carry no
provenance (`cursor_version` only on Cursor, `prompt_id`/`effort` only on
Claude Code, `turn_id` only on Codex). The rule is one comparison:

> **Cross-registration suppression.** If the engine detected from the payload
> is not the engine this invocation was registered for, the router exits
> without running handlers and appends a record with a `suppressed`
> outcome. A Cursor event arriving through the Claude Code registration is
> therefore dropped exactly once, by the invocation that should not have been
> reached, and the native registration handles it normally.

That keeps per-engine matcher control, needs nothing from the engines, and
costs one string comparison on the critical path. The `suppressed` record
matters as much as the suppression: a cross-registration hit is also the
signal that an engine has started importing another engine's config, which is
how this whole problem was found in the first place.

One asymmetry constrains where hookyard registers rather than how it
suppresses: `hasFailClosedHooksForStep` consults only Cursor's enterprise,
team, project, user, and runtime sources — **not** the Claude-imported ones —
so a handler that reaches Cursor *only* via `claudeUserHooks` can never be
fail-closed there. Any handler hookyard may later want to fail closed on
Cursor has to have a native Cursor registration, which the decision above
already gives it.

**A second, narrower double-fire lives entirely inside Cursor, and the
capture found it too.** With both `preToolUse` and `beforeShellExecution`
registered, a single shell call fired *both* — the generic event and the
protocol-split one, one after the other. On the deny path only `preToolUse`
fired, because denying there short-circuits before the protocol-split event
runs. So the two families are not alternatives to choose between on taste:
`preToolUse` is strictly earlier and gates the other. hookyard registers
`preToolUse` alone for `pre_tool` on Cursor, and an entry that wants the
narrower shell-only matcher takes `cursor:beforeShellExecution` as an
engine-scoped name (§7) — never both for one handler.

**Verdict: the self-healing half does not survive.** It was true of one
sink under one merge that hookyard neither owns nor writes to. What replaces
it is narrower and more honest: *during migration, whether a double-fire is
harmless is a property of the individual handler's own write semantics at
its own sink, verified per handler, and it is never something hookyard
provides.* Where a handler is append-only with reader-side collapse (aeye's
manifests) a transient double-fire is invisible; where it has an external
effect or an accumulating log (dispatcher's tmux message and its crew
event log) it is visible and permanent. The old rule said side-effecting
behaviors were the exception; the corrected rule is that idempotence is the
exception, and it must be demonstrated rather than assumed.

**The coexistence rule that replaces it, and the case where it does not
apply.** A behavior's old native entry is removed in the **same commit** that
adds its hookyard-emitted entry. That applies to every behavior, including
the ones whose handlers look idempotent — an idempotent handler buys a shorter
review window and a forgiving failure if the two land out of order, never a
standing coexistence anyone plans around. Double-firing is then bounded by the
review window of one PR migrating one behavior, and the bound does not depend
on anyone having correctly classified the handler.

**Two repos are involved in most of these migrations, and it is worth being
exact about which edits actually have to be atomic.** The naive worry is that
the old entry and the new manifest live in different repos — dispatcher's
Cursor entry is written by nix-config's inline block, not by dispatcher; aeye's
Cursor rows by aeye's own installer; lazytmux's Codex block by lazytmux — so
the swap spans two commits by construction.

That worry mostly dissolves under the aggregation invariant above. A manifest
does not enter the table by existing in its own repo; it enters when
**nix-config** adds its path to the single shared list. So for dispatcher, both
operative acts — removing nix-config's inline Cursor block, and adding
dispatcher's manifest path to nix-config's list — are edits to nix-config, and
they land in one commit. The manifest file itself can sit unreferenced in
dispatcher for as long as anyone likes, doing nothing. The same holds for aeye
and lazytmux, whose installers and blocks are themselves nix-config activation
entries. **The same-commit swap is available in every case this document
names**, and it is the rule.

The residual case is narrower: a behaviour whose old entry is written by
something nix-config does not control — a repo's own installer invoked outside
activation, or a hand-edited entry. There the swap genuinely spans two changes,
and the rule is ordering rather than atomicity: **remove the old entry first,
add the manifest path second.** That inverts the exposure, from a window where
both fire to a window where neither does, which is the better failure for every
handler here — fail-open (§5) already accepts that a guard not running is
survivable, while this section has just established that a double-fire may not
be recoverable. dispatcher's notifier is the sharpest illustration of why the
ordering matters if it is ever needed: a missed notification costs one message,
a duplicated one is permanent in two places.

**How "no flag day" is satisfied.** Migration is staggered *across* repos
and *across* behaviors within a repo, never *within* one behavior. Nothing
requires two repos to cut over together, and nothing requires a repo to cut
over all of its behaviors at once, because the marker-scoped
independent-writer idiom lets hookyard's rows and every existing writer's
rows live in the same file without either knowing the other exists. The
constraint is satisfied by the writer model, not by a migration schedule
anyone has to coordinate.

## 9. Delivery: packaging, path form, version skew

### Nix delivery shape

hookyard ships as a **flake package output** — a static Go binary, built by
its own flake, exposed as `packages.<system>.hookyard`. nix-config takes
hookyard as a flake input, and each consumer repo's home-manager module
contributes its manifest paths (§8) to a single shared list. Native config is
then rendered by **one** hookyard invocation over that whole list — not by
each module writing its own engine's config, which §8 explains is silently
destructive under a whole-file marker strip. This is the same shape every
other hook
script on this machine already arrives in, and it satisfies the "Nix-first,
static binary, not a Docker daemon" constraint by construction rather than
by policy — there is no service to supervise, no socket to own, and nothing
to start before an engine can fire a hook.

One decision belongs here rather than in §8, because it is a delivery
decision with a version-skew consequence: **hookyard is pinned once, by
nix-config, not per consumer repo.** Consumer repos ship manifests —
version-skew-tolerant data — and never their own hookyard input. If each of
four repos pinned its own, one machine could end up with four hookyard
builds rendering into three shared config files, and the guard × event ×
engine table would stop being a single table in any meaningful sense. One
input, one binary, one rendering pass.

### Path form: a stable profile path

Native config is emitted with an **absolute, rebuild-stable profile path** —
`${config.home.profileDirectory}/bin/hookyard`, which resolves on this
machine to `/etc/profiles/per-user/<user>/bin/hookyard`. Not a bare
`hookyard` resolved on `PATH`, and not a pinned `/nix/store/...` path.

Two separate questions get conflated here, and separating them is what
decides the answer. The first is **absolute path or bare name**, and it is
not close. `home/ai/cursor/default.nix` pins `PATH` for its own activation
environment "because it doesn't guarantee jq"; if the environment handed to
an activation script cannot be trusted to find `jq`, the environment handed
to a hook process by an editor launched from a desktop shortcut cannot be
trusted to find `hookyard`. A bare name resolves at hook-fire time against
whatever `PATH` the engine passes down, which is precisely what this
machine's existing wiring already refuses to rely on. The emitted command is
absolute.

The second question — **which** absolute path — is the real one, and the
store path loses it on first-hand evidence. lazytmux is the only project on
this machine that has actually shipped hooks into all three engines, and it
deliberately emits profile paths rather than store paths. Its Codex block
states the reason in its own comment: "codex records hook trust as a content
hash over the config, so a store path that changes every lazytmux rebuild
would force a fresh `/hooks` trust each bump. The profile path is
rebuild-stable." The live `~/.codex/config.toml` bears this out — every
lazytmux entry in it is `/etc/profiles/per-user/…/bin/claude-status-update`,
and no entry is a store path.

Codex's trust-hash behaviour itself was **not** independently verified this
pass; the claim is lazytmux's own comment, and §12 carries it as an open
item. But the decision does not rest on it alone. Two further costs fall out
of the store path regardless:

- **Config churn.** A store path changes on every build, so every hookyard
  bump rewrites three engines' config even when the guard × event × engine
  table has not changed at all. That makes the installer's correctness
  load-bearing on every rebuild rather than at install and at table changes.
  A profile path changes only when the table does.
- **Marker fragility.** With a changing command string, hookyard's
  marker-scoped strip must be invariant under the bump, or it fails to match
  the previous generation's entry, leaves it in place, and makes hookyard
  double-fire *against itself* on every version bump. That is a real trap the
  profile path removes rather than requires care around.

What the store path would buy is exactness: the emitted config names one
immutable build, so the binary that runs is provably the one the generation
pinned. The profile path instead names whatever the active generation puts
there, which means a rollback silently changes which hookyard runs. That is
the honest cost of this choice, and it is accepted: a hook wired into three
engines' config has to keep working across rebuilds more than it has to be
byte-pinned, and a rollback changing behaviour is the same thing every other
binary on the profile already does.

The evidence for the store path that the earlier reading leaned on does not
actually survive scrutiny either. aeye's Cursor installer and nix-config's
`dispatcher-cursor-notify` do emit store paths, so the live
`~/.cursor/hooks.json` genuinely mixes both forms — but neither of those is
wired into Codex, and neither has had to survive a trust-hash. The one
project that has, chose the profile path.

**The marker rule still holds, for a smaller reason.** hookyard's strip
marker is `/bin/hookyard` — the invariant tail of the emitted command. With
a profile path the command string is already stable, so the marker is no
longer load-bearing against version churn; it stays invariant anyway so that
a future change of path form cannot silently orphan a generation's entries.
All three live Cursor writers already follow this shape: aeye strips on
`/adapters/cursor/scripts/`, nix-config on `dispatcher-cursor-notify`,
lazytmux on `/bin/cursor-status-hook`.

**One consequence for delivery.** A profile path only resolves if hookyard is
actually installed into the profile, which means the home-manager module that
wires hookyard must also put the binary in `home.packages`. lazytmux asserts
exactly this for its own binaries. That assertion is part of hookyard's Nix
module, not an operator's responsibility.

### Version skew across independently migrating repos

Native config is re-rendered on **every home-manager activation**, so an
emitted path is always the one the current generation pins. With the profile
path form the emitted string does not even change between versions, so the
common case is that a hookyard bump rewrites nothing at all in three engines'
config: the path stays `${profileDirectory}/bin/hookyard` and only the binary
behind it moves. A consumer repo that has not migrated is untouched by any of
this — its old wiring is an independent entry under its own marker, and
hookyard's re-render strips only hookyard's rows.

The profile path also disposes of the collection problem rather than managing
it. `/etc/profiles/per-user/<user>/bin/hookyard` is a symlink into the active
profile, which is itself a GC root; it resolves to whatever the current
generation installed, and it keeps resolving across bumps without the config
being touched.

What that does **not** cover is the honest half:

- **A machine where hookyard leaves the profile.** The profile path is stable,
  not guaranteed. If hookyard is removed from `home.packages`, or an
  activation fails partway and leaves the profile without it, the emitted
  config still names a path that no longer resolves. Stability buys nothing
  against the binary simply not being installed.
- **Rollback silently changes which binary runs.** This is the cost the path
  form accepts. A profile path names the active generation's hookyard, so
  rolling back a generation rolls back the router without rewriting a line of
  any engine's config. That is the intended behaviour, but it means the
  emitted config is not evidence of which build actually executes.
- **Config that travels.** `~/.cursor/hooks.json` and `~/.codex/config.toml`
  are plain files in `$HOME`. Anything that copies them to another machine —
  dotfile sync, a restored backup, a container mount — carries a path that is
  valid-looking and host-specific. A profile path is *more* dangerous here
  than a store path, not less: a store path fails loudly on a host that never
  built it, whereas `/etc/profiles/per-user/<user>/bin/hookyard` may resolve
  on the destination host to a different version, or to another user's
  binary. Whether any such sync exists here is **unverified**; the failure
  shape is stated because the file format invites it.
- **Manifests versus binary.** hookyard's version is pinned once, but the
  manifests come from four repos on their own schedules. A manifest written
  against a newer hookyard than the one nix-config pins is the realistic
  skew, and it is a *parsing* problem, not a path problem: the rule is that
  hookyard ignores manifest fields it does not recognize and fails the
  install loudly on a manifest it cannot parse at all. Nothing here makes
  an unknown field silently change a handler's routing.
- **The window inside an activation.** Between strip and atomic rename, a
  hook can fire against the pre-rename file. That is safe — the profile path
  it names resolves throughout — and it is the reason the
  write goes through a temp file and `mv` rather than an in-place edit. It
  is not covered by the re-render guarantee; it is covered by the write
  discipline in §8.
- **First deployment.** None of this says anything about a machine where
  hookyard has never been built. That is §10's problem, and it is the one
  version-skew-adjacent case where the answer is "there is no installed base
  to skew against yet".

### What this makes §5's fail-open case concretely

With an absolute profile path, §5's "the binary cannot be found" is not a
`PATH` miss and never will be. It is one of a small, enumerable set: hookyard
was never installed into the profile on this host, an activation failed
partway and left the profile without it, it was removed from `home.packages`,
or the config travelled to a machine that has no such profile entry. All of
them fail the same way, at `exec`, before any hookyard code runs — which is
why §6's observability argument cannot cover this case by writing a record.
There is nothing running to write one.

The path form does not change fail-open's verdict. It changes the diagnosis,
from "an environment problem that could be anything" to a single absolute
path that either resolves or does not, checkable with one `test -x` and
reported by `hookyard doctor` (§5).

## 10. Migration order

The task's working assumption was aeye first, lazytmux second. That order was
reasoned against a target that was, at the time, a houston subcommand with an
existing installed base. The standalone premise changes the target's own
history — hookyard has never run anywhere — so the order is re-checked here
against that fact, not re-affirmed by default.

The structural argument for aeye first is unchanged and still the strongest
reason available: aeye remains the only repo with **three-way** duplication
across Claude Code, Codex, and Cursor, and its hooks are diagram, image, and
session-continuity conveniences, not security guards. Getting a brand-new
mechanism's plumbing wrong there costs a broken diagram script; getting it
wrong on a guard costs a silently disabled check. That asymmetry used to be
argued from the two facts that dominated *severity* — Codex's
zero-guard-path gap and the settings.json/Nix-overlay collision risk — and
this pass has resolved both of them, in opposite directions from what the
argument assumed. Codex's deny path is confirmed, so there is no
zero-guard-path gap to be careful around (§4); and Claude Code unions hook
sources rather than letting the overlay clobber `settings.json`, so there is
no silent-discard risk to sequence around either (§8).

The order does not change, because neither fact was ever the load-bearing
reason for it. What justifies aeye first is that hookyard has never run
anywhere and the first consumer pays the first-deployment risk below; low
stakes for a first trial run is an argument about hookyard's own maturity,
not about which gap is scariest. Two resolved risks make starting elsewhere
*less* urgent, not aeye *less* suitable. One new risk found this pass does
argue for care in the same direction: Cursor reads Claude Code's hook config
directly, so a handler registered for both engines can double-fire in
Cursor by construction (§8, sink 4) — and aeye, as the only three-way repo,
is exactly where that surfaces first and costs least. lazytmux's Cursor injection stays
the strong second candidate for the same reason as before: it already proves
in production the exact coexistence idiom §8 commits hookyard to reusing —
marker-scoped strip plus `jq empty` validation — as one more independent
writer into `~/.cursor/hooks.json`, not a rewrite of the file. Migrating it
second is how the design validates that "coexist as one more writer" holds up
against a real fourth entry before anything riskier is attempted.

**One input has changed, and it moves dispatcher, not aeye or lazytmux.**
§1 corrects dispatcher's duplication from two-way to three-way: the
dispatcher revision nix-config pins (`158abc8`) ships
`adapters/cursor/scripts/dispatch-notify.sh`, byte-identical to
`adapters/core/dispatch-notify.sh` and to the Codex copy — with §1's caveat
that dispatcher's older `main` does not have this file, so the ordering
argument below rests on the pinned revision rather than on upstream's current
state. The prior doc's reason for placing dispatcher's
migration *after* the Codex-gap-closing work was that dispatcher's own
migration "would also need to solve the no-Cursor-mechanism gap from
scratch." That reason no longer holds, for a reason narrower than "dispatcher
now has a Cursor script": dispatcher still has no Cursor `hooks.json` of its
own, but the gap that mattered — getting `dispatch-notify.sh`'s output *into*
Cursor's native config at all — is not dispatcher's to solve from scratch,
because nix-config's inline `jq` merge block already does it externally,
using the identical marker-scoped independent-writer idiom lazytmux's own
entry uses in the same file. That mechanism is exactly what migrating
lazytmux second is meant to validate. So dispatcher's position moves up: once
the coexistence idiom is proven twice — once as the subject of the whole
design (aeye) and once by adding hookyard as a further writer beside
lazytmux's own entries in a file it already shares — there is no
remaining reason to defer dispatcher behind it. **Dispatcher becomes the
natural third migration**, ahead of the guard migration, which stays last
precisely because it is the highest-stakes item and deserves a router proven
correct on three live migrations, not two.

**The fourth item is renamed by this pass, not reordered.** It was
"Codex-gap-closing work" — build a deny path where Codex had none. Codex has
one (§4), so what remains in fourth position is the thing that work was
always in service of: routing `secret-read-guard` and
`git-default-branch-guard` through hookyard on all three engines, with all
three enforcing. That is a smaller job than it was — no mechanism has to be
invented, and Codex needs only the `permissionDecision: "deny"` shape it
already accepts — and it stays last for the unchanged reason that it is the
only migration where getting it wrong disables a security control rather
than a convenience.

The revised order: **aeye, lazytmux, dispatcher, then the guard migration** —
unchanged at the top, dispatcher promoted from "blocked behind a from-scratch
gap" to "next in line" now that the gap it was blocked behind turns out to
already be solved by machinery lazytmux's migration validates, and the fourth
item narrowed from building a Codex deny path to using the one Codex has.

**The new first-deployment risk the standalone premise introduces.** Every
migration above assumes hookyard is present and correct when a consumer repo
activates its wiring. Under §9's delivery shape — a flake package output,
consumed as a flake input, installed into the profile and wired at an
absolute profile path — that assumption has a specific, concrete failure it
did not have when hookyard was a subcommand of a binary already on the
machine: the first activation that wires any consumer is also the first time
hookyard has ever needed to be in the profile at all.

§9's argument that the wired path keeps resolving is an argument about
*steady state*. It says the profile path survives version bumps without the
config being touched, and that a consumer who has not yet migrated is
insulated because its old wiring is a fully independent entry. Neither
protection exists on the *first* activation of the *first* consumer. There is
no prior hookyard generation to fall back to if the flake input fails to
build, and there is no old wiring underneath it, because migrating onto
hookyard is what that activation is doing. If hookyard does not land in the
profile — a build failure, a module that wires the config without adding the
package, an activation that fails partway — the emitted path does not
resolve, and the result is exactly §5's fail-open case. Fail-open means the
guard or convenience that hook was supposed to run simply does not run,
silently, with nothing on the machine that has ever seen this fail
differently to compare against.
This is a genuinely new risk category: no existing installs, no release
pipeline with a track record, and no operator who has watched this specific
failure happen and recovered from it before.

The chosen order already addresses this risk, rather than requiring a
different one: the first-deployment failure mode above is going to be paid by
*some* consumer's *some* first activation no matter which repo migrates
first, because hookyard has never run anywhere regardless of the order
chosen. Paying it on aeye means the worst case of a path that never resolves
on a totally green migration path is a broken diagram script, discovered and
fixed on the lowest-stakes repo, before dispatcher's notification or the
Codex gap-closing work's guard-shaped behavior ever depends on the same
untested activation path. The order was already built to absorb "prove the
mechanism where a mistake is cheap before trusting it somewhere expensive";
the standalone premise's first-deployment risk is that same argument's
sharpest instance yet, not a new argument for a different order.

## 11. Boundary: what hookyard does not absorb

The brief's own account of static-artifact unification is labelled
preliminary — "to confirm or correct" — and is treated that way here: as the
input to a check, not as ground already settled. Each of the six claims was
checked directly against `nix-config`'s modules this pass.

| # | Preliminary claim | Verdict | Grounding |
|---|---|---|---|
| 1 | One agents tree at `home/ai/claude-code/agents` holds 13 agents | **Confirmed** | `ls home/ai/claude-code/agents` lists 13 `.md` files |
| 2 | Symlinked into `~/.codex/agents` and `~/.cursor/agents` | **Confirmed** | Declared for both engines (`home/ai/codex/default.nix:52`, `home/ai/cursor/default.nix:82`), and both resolve on this machine: `~/.cursor/agents` and `~/.codex/agents` each point at `home/ai/claude-code/agents`, which holds the 13 files of claim 1. A prior pass recorded `~/.codex/agents` as absent here and attributed it to `home/ai/default.nix` gating the codex module on `osConfig.profile == "work"`; the gate is real, but this host does not fail it — `lib/host-configs.nix` sets `profile = "work"` for `thinkpad-p14s-g5` |
| 3 | `AGENTS.shared.md` is symlinked to all three engines | **Confirmed** | `~/.claude/AGENTS.shared.md`, `~/.cursor/rules/shared-conventions.mdc`, and `~/.codex/AGENTS.md` all resolve to `home/ai/AGENTS.shared.md` here — the third for the same corrected reason as claim 2 |
| 4 | OpenCode has its own separate `agents`/`commands`/`skills`/`instructions` trees, sharing nothing | **Confirmed, with a caveat** | `home/ai/opencode/default.nix:48-62` wires four separate out-of-store symlinks. Three of the four trees are **empty**; only `instructions/` holds content (2 files). The divergence is structural, not yet realized in practice |
| 5 | Skills go through a separate Nix module | **Confirmed, with a gap the brief did not name** | `agent-skills/default.nix` imports `agent-skills-nix` and installs into `.claude/skills`, `.cursor/skills`, and `.codex/skills` (the last also gated on `work`). Locally-authored skills (`claude-code/skills`, 15 directories) are symlinked **only into `~/.claude/skills`** — they do not reach Cursor's or Codex's skill trees the way the agents tree reaches all three |
| 6 | Commands exist only for Claude Code and OpenCode | **Confirmed** | `.claude/commands` resolves to `claude-code/commands` (3 files). OpenCode's `commands` tree is wired but empty. No Cursor or Codex command wiring exists at all |

**A prior pass's own correction is itself withdrawn here.** That pass recorded
claims 2 and 3 as corrected — Codex's agents tree and `AGENTS.md` absent —
and explained the absence by the codex module's `osConfig.profile == "work"`
gate on a host it took to be running the personal profile. The gate exists,
and the reasoning about it was sound; the premise was wrong.
`lib/host-configs.nix` assigns `profile = "work"` to `thinkpad-p14s-g5`, this
host, and both paths resolve here. That pass had also flagged, as an open
item, whether a work-profile host would read differently — it does, and this
host *is* one, so the question and the correction it was attached to close
together. Claims 2 and 3 are confirmed, not gated.

The four remaining verdicts stand as recorded, and the two caveats are worth
keeping distinct from the retired one. Claim 4's is unrealized-in-practice
divergence: OpenCode's separate trees are real in the Nix declarations and
empty on disk, because nothing has been authored into three of the four yet.
Claim 5's is the one real wiring gap, and it is the only finding here
independent of any profile conditional — locally-authored skills reach
`~/.claude/skills` and have no analogous symlink into Cursor's or Codex's
skill trees, which is an omission in `claude-code/default.nix` itself.

Neither the retired correction nor the two standing caveats argues for
hookyard absorbing anything. A gap
in placement — a symlink not yet drawn, a tree not yet populated, a profile
conditional not yet flipped — is an argument about placement's own current
state of completion, not evidence that the placement problem is secretly a
protocol problem hookyard should take on. The distinction that matters is not
"is everything already wired," it is "what kind of fix closes the gap." Every
gap found above closes the same way the working cases already work: add a
symlink, populate a tree, flip a gate. None of them closes by having hookyard
translate a config format, rename an event, reshape a payload, or return a
verdict on a critical-path timeout — the four things that actually define the
protocol problem hookyard exists to solve (§4-§7).

That is also true of the one coexistence problem placement genuinely has.
`agent-skills/default.nix` pins `structure = "link"` specifically because the
module's default structures (`symlink-tree`, `copy-tree`) `rsync --delete`
their destination directory, which would wipe the locally-authored skills
`claude-code/default.nix` symlinks into that same directory. Two independent
writers sharing one destination is precisely the shape hookyard's own §8
coexistence problem has — but here it is solved entirely by a file-tree
convention (pick the structure that doesn't delete what it doesn't own), not
by anything resembling protocol negotiation. That is the boundary in
miniature: even placement's own coexistence failure mode is native to
placement's own toolkit.

The conclusion, stated as the outcome of this check rather than its premise:
hooks are a **protocol** problem. Three engines declare hooks in three
different config formats, under three different event vocabularies, carrying
three different payload shapes, and a guard's verdict has to return inside a
timeout on the tool-call critical path before the engine proceeds — none of
that is solved by putting a file somewhere an engine already looks, because
the engines do not already agree on what to read or how to interpret it.
Subagents, skills, commands, and rule files are, by contrast, a **placement**
problem: a file in a directory, in a format every consuming engine already
reads natively, with no runtime decision to make and nothing to return
inside a deadline. Registration there is placement, and placement's own
toolkit — symlinks, and where a destination is shared, a non-destructive
structure choice — already solves it, imperfectly deployed today (a profile
gate not flipped, a tree not yet populated, one wiring omission for
locally-authored skills) but solved in kind, not in a way that argues for a
different kind of fix. Nothing found this pass contradicts that division, so
hookyard's boundary holds where the brief expected it to: it handles the
protocol problem and does not absorb the placement problem.

## 12. Open questions

The prior document's seven open questions are carried forward, each with an
explicit status against what this pass actually checked. This section is the
document's complete list of what remains unverified; nothing is left
unverified and undeclared elsewhere.

**How this pass got its answers, since it changes how much they are worth.**
All three engines are installed on this host, and all three ship their own
contract in a readable form. `cursor-agent` is a set of JavaScript bundles
minified but not obfuscated, so its hook resolution, its reducer, and its
Claude Code converter can be read as code. `claude-code` is a compiled bundle
that still carries both its own hook reference and the readable JavaScript of
its settings-merge path. `codex-cli` is a Rust binary, which gives the least —
serde field names and validation strings — but those strings enumerate the
accepted surface precisely, by naming everything outside it as unsupported.
Reading an implementation's own rejection messages is stronger evidence than
reading documentation about it, and weaker than watching it behave. A later
pass added the top grade where it was needed: ten hook payloads captured from
real tool calls across all three engines, each engine watched refusing a tool
call, and Codex's undeclared-timeout behaviour timed. Statuses below say which
grade they rest on where the difference matters.

1. **The settings.json / Nix-overlay collision (§8).**
   **Resolved, and the risk is retired rather than mitigated.** Claude Code
   2.1.263 collects hooks from `userSettings`, `localSettings` and
   `flagSettings` by flat-mapping all three — hooks **union** across sources,
   with de-duplication only by the resolved real path of the settings file,
   and `flagSettings` exempt even from that. The overlay therefore cannot
   silently discard an entry in `~/.claude/settings.json`, which is the
   specific failure this item existed to track. §8 is corrected, §10's
   dependence on this item is removed, and what remains is the ordinary
   duplicate-registration case that marker-scoped single ownership already
   covers. `~/.claude/settings.json` still has no `hooks` key
   (`jq 'has("hooks")'` → `false`), so nothing is deployed either way.
2. **The exact native multi-hook consolidation rule, per engine (§4).**
   **One of three resolved.** Cursor's reducer folds two hooks' `permission`
   values as `deny` > `ask` > `allow` — this design's own rule, arrived at
   independently by the vendor. Claude Code's and Codex's native rules remain
   unread: Claude Code is documented as running matching hooks in parallel
   but not as to how it reconciles disagreement, and Codex was not examined
   on this point. §4 explains why the design states its own rule regardless,
   so the remaining two are informational.
3. **Per-engine deny capability beyond Claude Code (§4).** **Resolved for
   both engines, and this item is no longer security-blocking.** It was the
   one genuinely load-bearing question on this list, and it resolved in the
   direction that removes a coverage gap rather than confirming one:

   - **Codex** accepts `permissionDecision: "deny"` on `pre_tool_use` with a
     mandatory non-empty `permissionDecisionReason`, and rejects `allow`,
     `ask`, `continue: false`, `stopReason`, `suppressOutput`, legacy
     `decision: "approve"`, and `updatedInput`. A `permission_request` path
     can deny approval separately. Deny-only is exactly the vocabulary a
     deny-wins router needs.
   - **Cursor** accepts `permission` values `allow`, `deny`, `ask` on six
     permission-capable events, `preToolUse` among them, and also treats a
     handler exiting 2 as a block with its stderr as the reason.

   So `secret-read-guard` and `git-default-branch-guard` enforce on all three
   engines, not one. The observe-only ruling for Codex is lifted in §4, and
   the compensating control this item previously asked for — actively
   surfacing a `deny` recorded with `enforced: false`, because it meant a
   guard would have stopped something and could not — is no longer needed for
   Codex. It is worth keeping as a `hookyard doctor` count anyway: §7 still
   has verdict shapes an engine has no slot for, an `ask` rendered to Codex
   among them, and those are the same "computed but not enforced" signal in a
   narrower form.

   **Both are now confirmed live as well.** The contracts above were read off
   each engine's shipped implementation; each has since been watched refusing
   a real tool call. Codex reported `Blocked by hook — hookyard probe deny`
   and "The command was blocked by the PreToolUse hook … It did not run."
   Cursor reported "The shell command did not run. The environment blocked it
   (`hookyard probe deny`)." Claude Code, tested the same way for symmetry,
   reported the block and declined to retry. In all three the handler's reason
   string reached the model, which is what makes a deny legible rather than a
   mysterious failure. This item is closed.
4. **Codex's default hook timeout when an entry declares none (§4).**
   **Resolved by measurement, and the answer is that there is no default.** A
   `UserPromptSubmit` entry declaring no `timeout`, running a hook that ticked
   once a second, ran for the full 180 s of its own loop and was never killed;
   Codex sat at `Working … Running hook` and waited for it. The question
   assumed a generous fallback and the reality is no bound at all, so an
   undeclared timeout on Codex lets one hook stall a turn indefinitely. 180 s
   is where the probe stopped, not where Codex did. §4's rule that hookyard
   always emits an explicit timeout is vindicated, with its reason upgraded
   from "the default is unknown" to "there is no default to rely on".
5. **HookBus's Claude Code publisher license inconsistency (§2).**
   **Still open, carried without re-verification.** Every HookBus fact,
   licenses included, comes from the prior pass's check against the GitHub API
   and hookbus.com, and none of it was re-fetched. It remains a footnote to
   the prior-art evaluation, not something the design depends on.
6. **The exact tool-name mapping for `apply_patch` and Cursor's
   protocol-split events (§7).** **Cursor's half resolved; Codex's half still
   open.** Cursor publishes the whole mapping itself, as a converter from
   Claude Code's tool vocabulary into its own (§7): `Bash`→`Shell`,
   `Edit`→`Write` collapsed, `Read`/`Write`/`Grep`/`WebFetch`/`WebSearch`/
   `Task` identical, `mcp__<server>__<tool>`→`MCP:<tool>`, and `Glob`
   unsupported and silently dropped. That settles the protocol-split
   sub-question — a shell-protocol event's identifier is literally `Shell`
   and MCP calls arrive as `MCP:<tool>`, so no third spelling exists for the
   `protocol` field to disambiguate — and it settles the direction of the
   `Edit`/`Write` collapse for Cursor. Whether **Codex's** single
   `apply_patch` should always map to `Write` or sometimes to `Edit` is
   untouched and stays open. The `Glob` drop is a new obligation rather than
   an answer, and §7 states it: a `match` that renders empty for an engine
   the entry claims must be rejected or warned on, because an unregistered
   handler leaves no trace at all in the record.
7. **The 2.35ms measurement was for the wrong code path (§2, §4.1).**
   **Resolved by this pass's measurement.** The router fan-out this question
   named as never having been measured now has been — a throwaway Go probe
   forking the four real, unmodified `agent-hooks` guards concurrently under
   the router's own consolidation logic. §4.1 reports the
   numbers; they are not restated here. The 2.35ms figure this question was
   raised against is itself superseded, independent of the fan-out result,
   by §4.1's and §2's own measurements of the shape hookyard actually is.

Six items the prior pass's own findings added, each with this pass's status:

- **Should fail-open be conditional for security-classed handlers (§5)?**
  **Still open, with one new input.** §5 accepts that an attacker who can
  shape tool inputs may be able to steer a guard or the router into failing,
  and therefore into allowing, on exactly the call that should have been
  denied. The caps in §4 narrow how cheaply that can be done; they do not
  make it impossible. The obvious next move is a per-handler `critical` flag
  that fails closed for that handler alone, and §5 deliberately does not
  specify it, because it reintroduces the machine-wide-freeze mode on the
  events most likely to fire. What is new is that the trade is no longer
  hypothetical in one engine: **Cursor already has this exact knob**, a
  per-entry `failClosed` field, so the question is partly one of adopting a
  native mechanism rather than inventing one. Two asymmetries make it a
  narrower question than before and not a settled one — Cursor's fail-closed
  check consults only its own enterprise, team, project, user and runtime
  hook sources, so an entry that reaches Cursor via the Claude Code import
  can never be fail-closed there (§8, sink 4); and no equivalent field was
  found for Claude Code or Codex, so a `critical` handler would fail closed
  on one engine and fail open on two. The thing that should decide it is
  still a steered failure actually observed, not an argument in a document.

- **No live hook payload has been captured, for any engine.** **Resolved.**
  Ten payloads were captured from real tool calls across all three engines and
  are committed as fixtures under
  [`fixtures/hook-payloads/`](fixtures/hook-payloads/). §7 now carries the
  observed field table instead of a documentation-derived one, and it corrected
  two things: all three engines send `session_id` (the prior three-way rename
  was wrong, and this simplifies §6's correlation key), while Cursor's `cwd` is
  the **empty string** with the real path only in `workspace_roots[0]` — a
  one-engine silent failure for any guard that reads `cwd`, which the envelope
  now has an explicit fallback rule for. The run also closed items 3 and 4 as
  planned, and settled §8's sink-4 decision by establishing that no payload
  carries registration provenance while every payload does carry an
  unambiguous engine discriminator.
- **Which Cursor keys does hookyard register for `pre_tool` and `post_tool`
  (§7)?** **Resolved: both families exist.** Cursor's shipped event enum
  carries the generic `preToolUse`/`postToolUse`/`postToolUseFailure`
  *and* the protocol-split `beforeShellExecution`/`afterShellExecution`,
  `beforeMCPExecution`/`afterMCPExecution`, `beforeReadFile`, `afterFileEdit`,
  `beforeTabFileRead`, `afterTabFileEdit` — 21 names in total, listed in §7.
  Both families are real, so emitting the deployed generic keys is a choice
  between two working options rather than the only option, and the
  protocol-split collapse §7 recorded as a contingency is not needed. Four of
  the protocol-split names are also on Cursor's permission-capable list
  (item 3), which is the one reason to prefer them: they are narrower
  matchers for the same guard.
- **Does Codex record hook trust as a content hash over its config (§9)?**
  **Confirmed in substance; the exact preimage is still unknown.** The claim
  in lazytmux's comment — that "codex records hook trust as a content hash
  over the config, so a store path that changes every lazytmux rebuild would
  force a fresh `/hooks` trust each bump" — is borne out by the deployed
  config itself: `~/.codex/config.toml` carries a `[hooks.state]` table whose
  keys are `<source>:<event>:<group index>:<hook index>` and whose values are
  `trusted_hash = "sha256:…"`, one per hook entry. The source segment is the
  config path or the plugin identifier (`aeye@aeye:hooks/hooks.json:…`), so
  trust is recorded **per entry, per source**, and Codex's own error text
  compares an expected against a got hash. §9's decision to emit a stable
  profile path is therefore resting on a real mechanism, not a belief.

  What is *not* determined is what the hash is taken over, and a second
  attempt against a freshly-minted entry did not crack it either. The capture
  run produced two hashes whose inputs were fully known — a config this
  document wrote, trusted through Codex's own `/hooks` review — and roughly
  two thousand candidate preimages (the command string; the entry serialized
  as JSON or TOML with and without `timeout`, `type` and a null `matcher`;
  each concatenated with the state key, the config path, and the event name in
  snake and camel case, in both orders, across seven separators; and the whole
  config file's text with and without its state section) reproduced neither.
  Something not modelled here participates — a salt, a version prefix, or a
  canonical form over an internal struct with fields the TOML never shows.

  The behavioural half is settled, and it is the half that matters: editing an
  entry invalidates that entry's trust and Codex re-prompts. Rewriting the
  config announced "2 hooks are new or changed" for the two edited entries,
  and trust had to be granted again before either would run. One adjacent finding is worth carrying for §9: a
  `bypass_hook_trust` config override exists, which is a lever for the
  first-activation problem §10 describes, and whose safety this document has
  not assessed.

- **Do the corrected drift counts (§1) reflect a methodology difference or an
  arithmetic mistake?** **Still open, and not re-examined this pass.** The
  recorded state, unchanged: a recount with `diff | grep -cE '^[<>]'`, over
  files confirmed unmoved since the counts were first taken, reproduces the
  *original* figures (229/150/56) exactly and reproduces the *corrected* ones
  (231/152/58) by no method tried — not `wc -l`, not any other adapter
  pairing — while `session-reset.sh`'s figure of 21 matches no pairing at all.
  Originals reproducible and corrections not, on unchanged files, points at a
  transcription or arithmetic slip in the correction step rather than a
  different counting method, but nobody has located the working that produced
  the corrected numbers, so which it was stays unresolved. It bears on §1's
  drift figures only, and on no decision in the design.
- **Does the profile-gate finding in §11 (claims 2-3) change anything about a
  work-profile host's version of this document?** **Resolved — and it
  retracts the finding that raised it.** The question assumed the checks had
  been run on a personal-profile host. They had not: `lib/host-configs.nix`
  sets `profile = "work"` for `thinkpad-p14s-g5`, which is this host, so the
  codex module is not gated off here and never was. `~/.codex/agents` and
  `~/.codex/AGENTS.md` both resolve, §11's claims 2 and 3 are confirmed
  rather than corrected, and the hypothetical work-profile reading this item
  asked for is simply the actual one. As predicted, nothing in §10's order or
  §11's boundary argument turned on it. What remains true and unchecked is the
  mirror image: the three personal-profile hosts in that file (`thinkpad-p14s-g6`,
  `halo`, `mbp-m4-pro`) *would* show Codex gated off, and no claim here has
  been verified on one of them.

**Four items this pass's own findings add.** All four come from reading the
engines' shipped implementations, and all four are consequences of engines
being more capable than this document assumed rather than less.

- **How does hookyard avoid double-firing on Cursor, given that Cursor reads
  Claude Code's hook config (§8, sink 4)?** **Decided, and specified in §8.**
  The capture settled the fact the choice hung on: no payload carries
  registration provenance, so suppressing the imported duplicate by reading
  provenance from the event is impossible. It is achievable by hookyard
  supplying its own — an engine tag in the argv of each rendered entry — and
  comparing it against the engine detected from the payload's own
  discriminators (`cursor_version`, `prompt_id`/`effort`, `turn_id`). §8 states
  the resulting cross-registration suppression rule and the `suppressed`
  record it emits. The capture also found a second, narrower double-fire
  inside Cursor: `preToolUse` and `beforeShellExecution` both fire for one
  shell call on the allow path, while a deny at `preToolUse` short-circuits
  before the protocol-split event runs — so hookyard registers `preToolUse`
  alone, and the narrower event is available only as an engine-scoped name.
- **All three engines can disable hooks wholesale, and hookyard cannot see it
  (§8).** **Broadened from one engine to three, and given a concrete
  `doctor` requirement.** The capture run met the trust gate on every engine —
  Cursor refused a fresh directory without `--trust`, Codex prompted and
  stated that trusting the directory is what allows hooks to load, and Claude
  Code's equivalent was read from its code. So this is a uniform property, not
  a Claude Code quirk, and §8 now says `hookyard doctor` must report workspace
  trust per engine. What remains genuinely open is narrower: the full gate list
  below is Claude Code's, and the equivalent lists for Codex and Cursor beyond
  workspace trust have not been enumerated.

  Claude Code's own list, for reference: hook capture is skipped entirely —
  not per entry — for an untrusted workspace, `disableAllHooks` in user or
  flag settings, an `allowManagedHooksOnly` policy, safe mode, a plugin-only
  restriction, bare mode, or an unreadable policy file; `localSettings` hooks
  are dropped separately when the workspace is untrusted or
  `settings.local.json` is git-tracked. Every one of these yields guards that
  appear installed and never run, and hookyard's record cannot distinguish
  that from a quiet session, because a handler that was never invoked cannot
  abstain, error, or time out. A smaller loose end from the same reading:
  `projectSettings` does not appear among Claude Code's hook sources at all,
  which would mean project-level `.claude/settings.json` hooks are not honoured
  — plausible, consistent with Cursor importing those separately, and not
  confirmed.
- **Newly opened: what is Claude Code's `defer` permission decision, and does
  hookyard ever render it (§7)?** The accepted value set is
  `allow | deny | ask | defer`, and `defer` is a fourth verdict this document
  has no concept for. §4's lattice has three plus `abstain`, and it is not
  clear whether `defer` is a synonym for one of those, a distinct
  "let another hook decide" state, or something the router should never emit.
  If it is genuinely distinct, it is the first case of an engine having a
  richer verdict vocabulary than hookyard's own, which is the opposite of the
  Codex situation and would need a rule in §4 rather than a translation in §7.
- **Newly opened: should the manifest model non-command handler types (§8)?**
  Claude Code supports three hook types — `command`, `prompt` (an LLM
  evaluates a condition) and `agent` (an agent runs with tools) — with the
  latter two restricted to tool events, and Cursor's importer handles `prompt`
  as well. §8's manifest names "the binary that gets executed", so it models
  `command` only. That is defensible for guards, which are binaries, and it
  means hookyard cannot express an entry that two of three engines support.
  Whether that is a deliberate boundary (like §11's) or an omission is
  undecided, and it should be written down as one or the other rather than
  discovered when someone tries to register a `prompt` hook.

Everything left unverified is accounted for above. **Resolved**, and no
longer a risk anyone carries: Claude Code's settings merge (item 1); all three
engines' deny paths, read off their implementations and then watched enforcing
(item 3); Codex's undeclared-timeout behaviour, which turned out to be no
bound at all rather than a generous default (item 4); the live payload
capture, which also corrected §7's field table and simplified §6's correlation
key; Cursor's native consolidation rule, tool mapping and event families
(items 2 and 6); §8's sink-4 double-firing decision; Codex's trust mechanism
in substance; and the profile-gate question, which retracted the §11
correction that raised it.

Still **open**, in the order it should be closed: the manifest's handling of
non-command handler types, and Claude Code's `defer` verdict — both touch the
envelope and want settling before the router is written; the per-engine gate
lists for Codex and Cursor beyond workspace trust, and whether Claude Code
honours `projectSettings` hooks at all; Codex's `apply_patch` sub-tool
mapping; Claude Code's and Codex's native consolidation rules; whether
fail-open should be conditional for security-classed handlers; the exact
preimage of Codex's trust hash; the prior pass's drift-count arithmetic; and
HookBus's carried facts. None of these blocks starting the implementation,
which is a change from the previous state of this list.
