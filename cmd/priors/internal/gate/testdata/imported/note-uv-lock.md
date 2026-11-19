---
name: note-uv-lock
description: uv lock behaviour on the data project
metadata:
  type: project
---
The data project locks with `uv lock --upgrade-package pandas` when pandas moves, and CI syncs with `uv sync --frozen`. A stale lock fails the job early, which is intended.
