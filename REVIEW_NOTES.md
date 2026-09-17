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
| doctor TTY detail lines stay bounded even for a long comma-delimited segment | Go reviewer MEDIUM (round 1) | eb7f937 | pending | `go build ./...`, `go vet ./...`, `go test ./... -race`, `golangci-lint run ./...`, `nix flake check`; targeted re-review pending | fixed | 1 |

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
