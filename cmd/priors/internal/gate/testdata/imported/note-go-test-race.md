---
name: note-go-test-race
description: race detector requirement for the go packages
metadata:
  type: project
---
The CI job for the Go packages runs `go test -race ./cmd/...` and fails on any data race. The race detector slows the suite roughly threefold.
