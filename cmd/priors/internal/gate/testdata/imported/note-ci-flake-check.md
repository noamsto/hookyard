---
name: note-ci-flake-check
description: what the nightly CI job does
metadata:
  type: project
---
The CI job runs `nix flake check --all-systems` nightly and reports failures to the team channel. A failed run is reproduced locally with the same flags.
