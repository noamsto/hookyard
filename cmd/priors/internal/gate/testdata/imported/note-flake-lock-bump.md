---
name: note-flake-lock-bump
description: flake.lock update cadence
metadata:
  type: project
---
The `flake.lock` is bumped weekly by a scheduled job that runs `nix flake update --commit-lock-file`. Pinned inputs such as `nixpkgs-stable` are excluded from the bump.
