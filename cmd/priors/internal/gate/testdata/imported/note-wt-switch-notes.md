---
name: note-wt-switch-notes
description: worktree layout produced by wt switch
metadata:
  type: reference
---
A branch created with `wt switch -c fix/x` gets a sibling directory under the worktrees root. The main checkout stays on the default branch the whole time, which `wt list` confirms.
