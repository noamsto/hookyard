# Review notes — hookyard #11 (decide the shape of the aeye migration)

Rebased onto `origin/main` `1b68143` (PR #39) on 2026-09-12; commit SHAs below
are the post-rebase ones.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| doctor `competing writer` check must not conflate an unreadable handler table with an empty one | reviewer HIGH (round 1) | 022988a | 022988a | `go test ./... -race`, new `TestCompetingWriterUnknownOnMissingTable`; targeted re-review accepted (round 2) | fixed | 2 |

recurrence_escalation: unused

---

# Review notes — hookyard #63 (bound Codex block duplication: structural strip anchor + doctor entry counts)

Base `dfbf33c` (`origin/main`). Ledger opened at head `0413433` immediately before
the review batch; updated before each push.

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| WriteCodex converges to exactly one hookyard-owned Codex hook command after a TOML rewrite / duplicated legacy blocks | review round 1 pending | 0413433 | pending | `go test ./internal/render/ ./internal/doctor/ ./cmd/hookyard/ -count=1` | open | 1 |
| doctor reports the Codex hookyard entry count and names every distinct router path with its runnable state | review round 1 pending | 0413433 | pending | same | open | 1 |

recurrence_escalation: unused
