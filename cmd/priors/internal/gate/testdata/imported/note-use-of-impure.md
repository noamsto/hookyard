---
name: note-use-of-impure
description: why the flake no longer needs the impure flag
metadata:
  type: reference
---
Use of `--impure` was dropped once the flake stopped reading the host's environment; the lock file now pins everything it needs.
