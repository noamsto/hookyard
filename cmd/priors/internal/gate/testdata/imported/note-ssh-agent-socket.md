---
name: note-ssh-agent-socket
description: ssh agent socket location
metadata:
  type: reference
---
The ssh agent socket is exposed at `$XDG_RUNTIME_DIR/ssh-agent.socket` by the user service, checked with `systemctl --user status ssh-agent`. Remote sessions forward it, so signing works on the build host.
