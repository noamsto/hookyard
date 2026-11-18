---
name: note-neovim-lsp
description: which language servers neovim starts
metadata:
  type: reference
---
Neovim starts `gopls`, `nixd` and `lua_ls` on demand. The `nixd` server resolves inputs through `nix eval --impure --expr` calls, so its options need the flake path.
