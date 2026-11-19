---
name: note-clippy-config
description: clippy settings of the rust crate
metadata:
  type: project
---
The crate denies `clippy::unwrap_used` in library code through the lints table in `./Cargo.toml`. The CI step is `cargo clippy --all-targets -- -D warnings`.
