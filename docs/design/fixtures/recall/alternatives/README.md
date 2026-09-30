# Alternatives to `recall` for session continuity — raw numbers

Source data for §10 of [`recall-evaluation.md`](../../recall-evaluation.md). Every
candidate was pinned, installed and run against the **same replayed personal
transcripts** as the `recall` evaluation in the parent doc, under the same
measurements (install friction, digest quality, latency/size, redaction, engine
coverage, hookyard fit, licence/maintenance). This directory holds the scrubbed
numbers; the parent doc holds the comparison and the revised verdict.

## Transcripts replayed

The five personal-repo Claude Code transcripts of the `recall` evaluation,
re-identified by byte size (the set is the same; one grew slightly since
#126 was written). Only `~/.claude/projects/-home-noams-Data-git-noamsto-*`
was read — no `factify`/work session. Raw paths and UUIDs are intentionally
not recorded here; codenames match the parent doc.

| codename | transcript bytes at replay | user turns | what it settled |
| --- | ---: | ---: | --- |
| repo-a | 3 815 066 | 12 | pi hook extension consolidation across five repos |
| repo-b | 3 299 826 | 15 | lazytmux remote-bridge keepalive / passthrough (4 PRs) |
| repo-c | 2 997 530 | 11 | tmux-og Linear picker / enrich card; a flaky macOS bats test |
| repo-d | 5 420 070 | 16 | lazytmux OSC 9;4 progress thread (spike → #524 → #526) |
| repo-e | 31 063 449 | 49 | tmux-remux recording tape + mini-map (PR #97) |

Total 44.4 MB. `replay-metrics.json` in the parent directory still carries the
`recall` numbers for the same set.

## Isolation

Every command in this evaluation was run with `HOME` and `XDG_CACHE_HOME` /
`XDG_CONFIG_HOME` / `XDG_DATA_HOME` pointed at a scratch directory under the
crew artifacts dir, and with `GOPATH` / `GOMODCACHE` / `GOCACHE` there too.
Verified three ways:

1. each tool's own resolved path — `deja doctor`/`deja sources`,
   `remem doctor`/`remem install`, and claudemem's `--store` default — reported
   the scratch directory, never the real `~/.cache`, `~/.claude`, `~/.codex` or
   `~/.pi` (the reported paths are quoted per candidate below);
2. Python/Rust/Go toolchain caches were redirected by the same exported vars;
3. a stub `claude` / `codex` / `curl` on a prepended `PATH` for the `remem`
   runs, so its LLM executor could not reach the network even by accident.

No engine hook was ever installed into a real config: `remem install` was run
only with `HOME` in the scratch dir, and nothing else calls an installer.

**Redaction values** are exactly the fake placeholders from
[`../redaction-cases.md`](../redaction-cases.md); no real token is present in
any file here or in the run logs.

## Per-candidate files

- `measurements.json` — pinned versions and every measurement 1–7 per candidate.
- `redaction-matrix.md` — the 17-case fake-secret matrix, per candidate.
- `port-our-own.md` — size/shape estimate for a Go port of `recall`'s
  deterministic sections, read from `recall` source.

## How each was run (commands, scrubbed)

- **deja-vu** — `go build ./cmd/deja` at the pinned commit; `HOME=… deja index
  --rebuild --quiet` then `deja "<question>"` and `deja hook-context --plain`
  from each project directory.
- **remem** — prebuilt `remem-linux-x64.tar.gz` from the pinned release,
  checksum verified; `HOME=… remem install --target claude`, then a Stop-hook
  payload fed to `remem summarize --host claude-code` and `remem worker --once`
  with the stub `claude`/`codex` on `PATH`; `remem raw search` for the offline
  archive.
- **claudemem** — `go build .` at the pinned commit; `HOME=… claudemem session
  save …`, `claudemem search …`, `claudemem note add …`, `claudemem context
  inject`.
