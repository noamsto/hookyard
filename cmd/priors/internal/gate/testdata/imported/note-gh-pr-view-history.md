---
name: note-gh-pr-view-history
description: how the PR status was checked during the migration
metadata:
  type: project
---
During the migration the status was checked with `gh pr view 123 --comments` in a loop. That approach was dropped once the merge queue reported status itself.
