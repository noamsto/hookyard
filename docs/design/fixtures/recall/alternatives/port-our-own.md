# Port-our-own: a Go reimplementation of `recall`'s deterministic sections

Not built, as the task requires. The estimate below is read from `recall`
source at `e65cb1e406fea99bee4931e7597715c47b3d2272` (1683 Python lines of
non-test code total).

## What would be ported

| piece | recall source | LOC | Go shape |
| --- | --- | ---: | --- |
| Claude transcript parser + renderers | `scripts/parse_transcript.py` | 172 | `internal/memory/transcript.go` — Claude JSONL → events |
| deterministic digest build | `scripts/make_context.py` `build()` (minus summarizer/next-steps) | ~60 | `internal/memory/digest.go` — Goal / Files / Commands / Where-we-left-off / Git |
| git ground-truth | `scripts/common.py` `git_info` + `git_uncommitted` | ~40 | reuse `internal/` git exec, neutralized as recall does |
| Pi / Codex parsers | `scripts/harness_opencode.py` is the template | ~80 each | one `TranscriptParser` per engine; hookyard supplies `session_file` |
| redaction | `scripts/redact.py` | 35 | extend with the 4 missed shapes |
| **total** | | | **~400–600 Go lines + tests** |

## What it fixes, and what it keeps

- **Fixes:** file capture from `Bash` edits (`sed`/`cat >`), which recall misses
  because it only reads `Edit`/`Write`/`Read` `file_path` args; an explicit 8 K
  cap on the injected digest; the four redaction gaps in
  [`redaction-matrix.md`](redaction-matrix.md); and the noisy extractive summary
  (drop it — the deterministic sections are the useful part).
- **Keeps:** no retrieval, no durable/cross-repo facts, per-project scope. It
  closes none of the failures `recall-evaluation.md` §7 records.
- **Maintenance:** the parser is the only fragile surface, and each engine's
  transcript format drift becomes ours to track — against `deja-vu`, which
  already tracks thirty-five harnesses.
