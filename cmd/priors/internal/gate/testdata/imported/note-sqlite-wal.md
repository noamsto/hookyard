---
name: note-sqlite-wal
description: sqlite journal mode for the local cache
metadata:
  type: reference
---
The local cache opens `~/.cache/app/cache.db` in WAL mode, so a `-wal` and `-shm` file sit beside it. A copy of only the main file taken while a process is open loses recent writes.
