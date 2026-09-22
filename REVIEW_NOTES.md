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

Base `8c802d5` (`origin/main`). Ledger opened before the independent review batch.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |

recurrence_escalation: unused
