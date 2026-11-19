---
name: note-env-in-unit
description: environment of the inference service
metadata:
  type: project
---
We set `ROCR_VISIBLE_DEVICES=0` in the unit file rather than in a shell profile. That way `systemctl cat vllm` shows the whole environment.
