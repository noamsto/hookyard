# Review notes — hookyard #11 (decide the shape of the aeye migration)

Rebased onto `origin/main` `1b68143` (PR #39) on 2026-09-12; commit SHAs below
are the post-rebase ones.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| doctor `competing writer` check must not conflate an unreadable handler table with an empty one | reviewer HIGH (round 1) | 022988a | 022988a | `go test ./... -race`, new `TestCompetingWriterUnknownOnMissingTable`; targeted re-review accepted (round 2) | fixed | 2 |

recurrence_escalation: unused

---

# Review notes — hookyard #56 (doctor report rendering and JSON)

Ledger opened before the independent review batch.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| doctor TTY detail lines stay bounded even for a long comma-delimited segment | Go reviewer MEDIUM (round 1); targeted reviewer HIGH (round 2); dispatcher late packing directive | 93ffda3 | 93ffda3 | full deterministic gate passed; first lines use 88 runes and continuations reserve indentation; targeted re-review accepted (round 4) | fixed | 4 |
| rendered doctor TTY rows, including check details and fixes, must not exceed 88 columns and continuations start under their detail column | follow-up defect from PR #69; Go review approved; test-runner's long-label concern refuted by all production check labels (max 24 runes) | 14f97d4 | 14f97d4 | `TestRenderDoctorTTYLinesFitTerminalWidth`; full deterministic gate; independent Go review and targeted test review passed | fixed | 1 |

recurrence_escalation: unused

---

# Review notes — hookyard #63 (bound Codex block duplication: structural strip anchor + doctor entry counts)

Base `dfbf33c` (`origin/main`). Ledger opened at head `0413433` immediately before
the review batch; updated before each push.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| WriteCodex preserves foreign bytes verbatim (no global whitespace normalization) | reviewer HIGH round 1 | 4909c78 | dc76d6e | `TestWriteCodexPreservesForeignMultilineStringVerbatim`; `go test ./internal/render/ ./internal/doctor/ ./cmd/hookyard/ -count=1`; `golangci-lint run ./...`; targeted re-review accepted | fixed | 2 |
| strip keeps a matcher table that a surviving foreign sibling still needs | reviewer HIGH round 1 | 4909c78 | dc76d6e | `TestWriteCodexKeepsSharedMatcherForForeignSibling`; same gate; targeted re-review accepted | fixed | 2 |
| codexRegistration distinguishes distinct events from same-event duplication | reviewer MEDIUM round 1 | 4909c78 | dc76d6e | `TestCodexRegistrationPassesWhenDistinctEvents`; same gate; targeted re-review accepted | fixed | 2 |

recurrence_escalation: unused

---

# Review notes — hookyard #72 (pi pre_tool advisory channel)

recurrence_escalation: unused

| invariant/family | finding | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| pi pre_tool advisory lands at the call's next model request, once per call | TS(promoted) HIGH-1: steer trickles under pi's default steeringMode one-at-a-time (N parallel calls → N turns) | cfb56a0 | b675462 | pi 0.86.1 PendingMessageQueue.drain + settings.md default verified | fixed: mechanism B (append to same call's tool_result), dispatcher-approved; probe D/E + 4 live tests; round-2 re-review accepted the invariant | 2 |
| same | TS MED-2: queued steer overrides tool terminate | cfb56a0 | b675462 | runLoop continue condition verified | fixed by B (no steer) | 2 |
| record delivered truthfulness | TS MED-3: abort/dequeue clearAllQueues drops steer while record says delivered | cfb56a0 | b675462 | source-cited | fixed by B (no steer queue); residual cases (abort before exec, foreign later block, non-array content, later replacing extension) documented in §11.1 | 2 |
| live test proves record delivered | Go HIGH: Delivered check skipped when record missing | cfb56a0 | b675462 | code read | fixed; round-2 re-review verified | 2 |
| public fixtures never leak machine data | Py HIGH: elide() only redacts str text/content keys | cfb56a0 | b675462 | code read; current samples clean | fixed; round-2 re-review executed elide() on every block shape | 2 |
| fixture evidence honest | Py MED: exact-equality prompt match | cfb56a0 | b675462 | code read | fixed | 2 |
| fixture evidence honest | Shell CRIT: readiness loop has no failure path | cfb56a0 | b675462 | code read | fixed; shellcheck clean | 2 |
| fixture portability | Shell HIGH: realpath -m GNU-only | cfb56a0 | b675462 | code read | fixed | 2 |
| post_tool router input is the tool's own output | round-2 promoted HIGH: pre advice flushed before post handlers leaked into post_tool router's tool_response | b675462 | c060e66 | TestPiBridgePostToolRouterPayloadExcludesThePreToolAdvisory + live ordering test; round-3 (dispatcher-authorized, final) re-review accepted | fixed | 3 |
| docs accuracy | round-2 MED: caveat direction, incomplete dropped-advice list, stale steer wording; round-3 MED: "in that order" ambiguity | b675462 | c060e66 + follow-up | round-3 re-review | fixed | 3 |

---

# Review notes — hookyard #76 (pi 0.87 turn-end boundary: outcome, evidence refresh, agent_before_settle continuation)

Base `8c802d5` (`origin/main`). Ledger opened before the independent review batch; round 1 (go-reviewer@opus, typescript-reviewer, security-reviewer, test-runner) launched at head `a8291f3`.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| record top-level run outcome must not share a name with handlers[].outcome's vocabulary | go-reviewer MEDIUM-1 (round 1) | a8291f3 | 699a418 | `TestToRecordTurnOutcome`; full gate; live continuation test; round-2 targeted re-review confirmed | fixed | 2 |
| serve event filter/label for canonical `turn_end` must not include pi's per-turn `pi:turn_end` records | go-reviewer MEDIUM-2 (round 1); round-2 HIGH on the attempted fix (canonical-only rule hid router-error records, label mislabelled them) + 2 MEDIUM | 699a418 | reverted to a8291f3 bytes | serve files byte-identical to round-1-reviewed a8291f3; full gate green; §11.2 documents the gap | deferred (#78) | 2 |
| a turn_end continuation must never carry pre_tool's "Blocked by hookyard" default text | go-reviewer MEDIUM-3 (round 1) | a8291f3 | 699a418 | `TestRenderPiTurnEnd` empty-reason row; `TestRunRoutePiTurnEndDenyWithNoReasonFallsBackToFixedReason`; round-2 targeted re-review confirmed | fixed | 2 |
| consumer map: doctor `piBridgeDrift` is a consumer (flags stale bridges), not absent | go-reviewer map note (round 1) | a8291f3 | n/a | doctor.go:878 read; behavior correct | fixed (map corrected) | 1 |

recurrence_escalation: unused

---

# Review notes — hookyard #81 (route pi agent_settled and codex/cursor session-end signals)

Ledger opened before the independent review batch; base `origin/main` `8c802d5`.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| docs must not assert the --registered-for fallback is Claude-only once it is widened | reviewer MEDIUM (round 1), hookyard.md §7/§12 | bb5a8a8 | 3463666 | doc-only; round-2 targeted re-review approved each fix | fixed | 1 |
| the consumer-contract link must not imply codex/cursor payloads are captured | reviewer MEDIUM (round 1), README.md | bb5a8a8 | 3463666 | doc-only; round-2 targeted re-review approved each fix | fixed | 1 |
| the fallback comment must not imply canonical codex session_start is rescued | reviewer MEDIUM (round 1), cmd/hookyard/main.go | bb5a8a8 | 3463666 | comment-only; round-2 targeted re-review verified condition byte-identical | fixed | 1 |

recurrence_escalation: unused

---

# Review notes — hookyard #112 (serve flow view: UX pass, smooth motion/clicks, slow dots)

Two plan-critic rounds ran (revision cap reached): round 1 found 4 blocking gaps
(a stray-dot regression against check14, `allow` wrongly assumed non-loud, `flash()`
invisible on engine nodes, incomplete tester-pass traces); round 2 found 3 more
after those were fixed (a flicker-back race in the optimistic selection, a
double-release bug in the drop-oldest eviction sketch, missing after-traces for
scenarios c/d) — all folded into the final plan rather than a third round.
Ledger opened before the independent review batch; base `origin/main` `7325ff2`.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| a fan-out flight's dot must not teleport across a node's own plate at a leg boundary | tester-pass finding (own investigation, not a reviewer) | e816cde | e816cde | live-browser measurement: ~110 leg transitions in 20s showed a consistent 223-227px single-frame jump before the fix; e2e check18 after: max jump 27.1px (n=1040 frames); check14 unaffected (289 pulse samples, all on-band) | fixed | n/a (pre-review finding) |
| engine→outcome pulse travel must be ~5s ±0.5s | acceptance #3 | e816cde | e816cde | e2e check17: 5088-5144ms across runs (target 4500-5500) | fixed | n/a |
| a click must show feedback before its refetch resolves | tester-pass finding + acceptance #4 | e816cde | e816cde | e2e check19: `.selected` on node+label true immediately, fetch still held | fixed | n/a |
| the 128-dot pool must degrade under a burst without going deaf to new calls | task's "likely suspects" list (drop oldest, not freeze) | e816cde | e816cde | e2e check4 (saturates the pool: `dropped 1687`), full drain asserted (see next row) | fixed | n/a |
| pool eviction must not leak a slot or double-release a flight | typescript-reviewer HIGH (round 1, blocking) | 0bc216e | 0bc216e | check4 waits for `dots===0 && activeCircles()===0` post-burst (independent signals: busyCount bookkeeping vs. DOM circle count) and asserts `dropped>0`; targeted re-review (round 2) approved | fixed | 2 |
| `strayDots()`'s on-plate exemption must not weaken the check for non-bridging dots elsewhere | typescript-reviewer MEDIUM (round 1) | 0bc216e | 0bc216e | `data-bridging="1"` scopes the exemption to the actual bridge window only; targeted re-review (round 2) traced the attribute's set/clear on every draw/hide path, found no stuck-on/off case; approved | fixed | 2 |
| trail rendering during a bridge uses a different distance basis than a normal leg (cosmetic: trail briefly shrinks then "pops" back to length) | typescript-reviewer MEDIUM (round 1) | e816cde | n/a | reviewer's own verdict did not mark this blocking; the ~140ms window makes it imperceptible in practice | deferred (documented in PR body, not filed as an issue — cosmetic only) | 1 |

recurrence_escalation: unused

---

# Review notes — hookyard #174 (read profile, org lists, store ids and trust_root from the root-owned trust file)

Review base `6b93791` (merge-base with `origin/main`); harness reviewer roster, no repo-local reviewers.
Batch (round 1): go-reviewer, security-reviewer, nix-reviewer + yaml-reviewer, targeted test-runner (all opus).
The diverse-engine one-shot (cursor, grok-4.7-high; codex was not in budget) returned no output and was
dropped; its first attempt was stopped by the secret-read guard on the prompt's wording.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| a non-string `trust_root` is off, not a missing config (§4.4) | go-reviewer + security-reviewer MEDIUM (round 1); targeted re-review MEDIUM (round 2: table-shaped values still fail via undecoded keys) | 3246e1c | 3246e1c (scalars, arrays) | `TestLoadTrustRoot`, `TestReadTrust` trust_root cases; round 2 reproduced the table case with BurntSushi v1.6.0 | partly fixed; table-shaped values deferred to a follow-up issue (fails closed, root-only file, no round left) | 2 |
| the trust walk resolves `..` after symlinks, as the kernel does | go-reviewer LOW, security-reviewer LOW (round 1) | 3246e1c | 3246e1c | `TestCheckTrustPathFollowsSymlinkBeforeDotDot` red with `filepath.Clean` restored, green with the fix; round 2 edge-case table | fixed | 2 |
| the "unparsable" trust test asserts the parse error, not any error | go-reviewer LOW (round 1) | 3246e1c | 3246e1c | asserts `toml: line 1 (last key "profile"): expected value` | fixed | 2 |
| the check-only seamed derivation is named apart from production | nix-reviewer LOW (round 1) | 3246e1c | 3246e1c | check inputs `priors-0.1.0.drv`, `priors-priorstest-0.1.0.drv` | fixed | 2 |
| `priors search` reports a config (trust) failure | security-reviewer LOW, go-reviewer note (round 1) | d9aa3ee | — | `cmd_search.go:36-39` returns 0 silently; predates this branch, by design "prints no hits" | deferred to a follow-up issue | 1 |

recurrence_escalation: unused
