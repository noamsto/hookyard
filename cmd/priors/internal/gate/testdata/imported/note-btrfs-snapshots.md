---
name: note-btrfs-snapshots
description: btrfs snapshot retention
metadata:
  type: reference
---
Snapshots of the home subvolume are taken hourly by `snapper -c home create` from a timer and kept for two days. Weekly snapshots are kept for a month and then pruned.
