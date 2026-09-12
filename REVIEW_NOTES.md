# Review notes — hookyard #11 (decide the shape of the aeye migration)

| invariant/family | finding or thread IDs | observed head | fix commit | proof | disposition | rounds used |
| --- | --- | --- | --- | --- | --- | --- |
| doctor `competing writer` check must not conflate an unreadable handler table with an empty one | reviewer HIGH (round 1) | pending | pending | `go test ./internal/doctor -race` + new `TestCompetingWriterUnknownOnMissingTable` | fixed (targeted re-review next) | 1 |
