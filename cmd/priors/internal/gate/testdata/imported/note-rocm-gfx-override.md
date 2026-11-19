---
name: note-rocm-gfx-override
description: selecting the integrated GPU under ROCm
metadata:
  type: reference
---
The machine has a discrete and an integrated GPU. With `ROCR_VISIBLE_DEVICES=0` in the service environment, ROCm sees only the integrated gfx11 part.
