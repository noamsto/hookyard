# Captured `context.md` shape (one real replay, scrubbed)

Scrubbed excerpt of a `context.md` Recall produced by replaying a real
personal-repo Claude Code transcript (3.3 MB, 88 user turns) through
`capture.py` + `make_context.py`. Repo name, paths, URLs, issue/PR numbers,
crew ids and timestamps are replaced with placeholders; no secret or private
content is reproduced. The repeated `<summary>…</summary>` bullets are the real
output — they are the summarizer's top-8 sentences for that session.

```markdown
# Project Context — repo-b (updated <time>)

_Generated locally by Recall — TextRank (vendored, pure Python)._

## 🎯 Goal
work on open issues

## 🧭 Summary
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>
- <summary>Background command "Re-arm crew watch" completed (exit code 0)</summary>

## ⏭️ Next steps / open threads
- Two things that would use your input when you have a moment: **PR #NN is
  ready for your review**, and the #NN/#NN/#NN cluster still needs the
  canonical-home decision before it can be dispatched at all.
- **And #NN shouldn't wait for either.** It's a real user-facing bug parked
  behind an extraction whose API is undecided …
- Uncommitted changes to wrap up: .recall/

## 📂 Files touched
- /tmp/claude-status/images/diagrams/roster-<id>.png
- /tmp/claude-status/images/diagrams/triage-precedence.png

## 🔧 Commands run
- crew watch --timeout 270
- crew roster 2>&1 | jq -r '.[] | "\(.name)\t\(.color)\t\(.state)…"'
- …and 255 more

## ⏱ Where we left off
Another quiet hour: no open PRs, still the same 2 open issues, crew empty. I'm
going to stop re-arming the watch rather than keep parking indefinitely. …

## 🌿 Git ground-truth
```
Recent commits:
<sha> feat: previous work
<sha> initial
```
```

Contrast: the deterministic sections (`Goal`, `Commands run`, `Where we left
off`) are faithful; the extractive `Summary` collapsed onto one repeated
boilerplate line. A second replay (a multi-issue session) produced a `Summary`
that was mostly raw `<event>{…}</event>` crew-bus JSON.
