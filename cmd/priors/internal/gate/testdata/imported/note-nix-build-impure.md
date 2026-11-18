---
name: note-nix-build-impure
description: nix build of the vllm flake fails without impure evaluation
metadata:
  type: project
---
Building with `nix build .#vllm --impure` works because the flake reads `$HOME` during evaluation. A pure build fails in the sandbox with an access error, and the impure result depends on the host environment.
