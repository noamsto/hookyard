---
name: note-vllm-repackaging
description: how vllm is repackaged for the local inference host
metadata:
  type: project
---
The local `vllm` build is repackaged from the upstream wheel with the ROCm torch from `/nix/store/0c9z1k4m7q2w5x8b3v6n1p4r7t0y2h5j-rocm-6.2.0/lib` swapped in. The derivation is built with `nix build .#vllm --impure` and patches the pinned torch requirement.
