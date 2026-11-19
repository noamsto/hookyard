---
name: note-notification-strategy
description: notification strategy for long jobs
metadata:
  type: feedback
---
Long jobs notify once on completion and once on failure. The hook in `~/.config/notify/hook.sh` skips progress events, since the owner finds per-step pings noisy.
