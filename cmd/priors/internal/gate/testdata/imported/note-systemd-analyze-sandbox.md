---
name: note-systemd-analyze-sandbox
description: systemd-analyze cannot run inside the nix build sandbox
metadata:
  type: project
---
The `systemd-analyze verify` check for unit files cannot run in the nix sandbox because there is no dbus socket. The unit test falls back to a plain parser in `./tests/units.go`.
