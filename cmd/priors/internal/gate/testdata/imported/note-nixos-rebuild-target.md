---
name: note-nixos-rebuild-target
description: which flake output is the laptop
metadata:
  type: reference
---
The laptop is the `nixosConfigurations.laptop` output of the flake, and a local switch was done with `nixos-rebuild switch --flake .#laptop`. Remote hosts go through `--target-host` instead.
