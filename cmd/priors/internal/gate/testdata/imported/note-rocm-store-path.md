---
name: note-rocm-store-path
description: where the ROCm libraries live in the nix store
metadata:
  type: reference
---
The ROCm libraries resolve to `/nix/store/0c9z1k4m7q2w5x8b3v6n1p4r7t0y2h5j-rocm-6.2.0/lib` and the hash changes with every nixpkgs bump. The path is found with `nix path-info -r .#rocm` rather than hardcoded.
