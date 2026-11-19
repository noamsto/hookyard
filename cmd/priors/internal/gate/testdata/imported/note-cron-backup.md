---
name: note-cron-backup
description: nightly backup job
metadata:
  type: project
---
The nightly backup runs from a systemd timer at 03:30, and `systemctl list-timers --all` shows the next run. It writes a tarball into `/mnt/backup/nightly`.
