---
name: note-journal-persist
description: journald persistence on the server
metadata:
  type: reference
---
The journal on the server is persistent under `/var/log/journal` and capped at 2G by `SystemMaxUse`. A past boot is read with `journalctl -u foo -b -1`.
