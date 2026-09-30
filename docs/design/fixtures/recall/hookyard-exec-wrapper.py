#!/usr/bin/env python3
"""Minimal hookyard exec handler that delivers Recall's context.md.

hookyard's handler wire protocol is `{"hookSpecificOutput":{"additionalContext":
...}}` (internal/router/handler.go, classify). Recall's own SessionStart hook
prints markdown instead, so it cannot be a hookyard handler as-is; this wrapper
reads the file Recall already wrote and re-emits it in hookyard's shape.

hookyard adds **no** untrusted-data fence of its own on session_start: its
Claude render (internal/verdict/render.go, renderClaudeCodeAdvisoryOnly) passes
the advice string through unchanged. Recall's own session_start.py wraps the
digest in `BEGIN/END recall context (untrusted data)` markers precisely because
context.md is transcript-derived and can carry injected text; this wrapper must
reproduce those markers, or the hookyard delivery path silently drops recall's
injection fence.

Verified against hookyard's own router:

    hookyard route --registered-for claude-code --event session_start \
        --state-dir <state-dir> < claude-SessionStart.json

with a 5104-byte context.md, the router emitted 5184 bytes of
`additionalContext` (5826 bytes of JSON), fence included. Fail-open: an absent
context.md exits 0 with no output, so a missing digest leaves the session
exactly as it was.

Add to a manifest (yard mode) as:

    {"handlers":[{"id":"recall-context","exec":"<abs path to this file>",
      "events":["session_start"],"engines":["claude-code","pi","codex"],
      "timeout_ms":4000,"lane":"verdict"}]}

No budget sharing is automatic: hookyard's only caps are the 4.3 s handler
sub-budget and its 64 KiB stdout cap. Recall's digest reached 8190 bytes on a
large session, so the memory layer must cap it to avoid competing with §4.4
tier 1 (memory-layer §9 decision 5).
"""

import json
import os
import sys

try:
    payload = json.load(sys.stdin)
except Exception:
    payload = {}

# The handler runs with the engine's payload on stdin; the project is its cwd.
cwd = payload.get("cwd") or os.getcwd()
try:
    context = open(os.path.join(cwd, ".recall", "context.md"), encoding="utf-8").read().strip()
except OSError:
    sys.exit(0)  # fail open: nothing to inject

# Preserve recall's own fence: hookyard's session_start render adds none.
context = ("===== BEGIN recall context (untrusted data) =====\n"
           + context
           + "\n===== END recall context =====")

print(json.dumps({"hookSpecificOutput": {"additionalContext": context}}))
