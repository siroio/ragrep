---
name: setup
description: Use when installing, building, initializing, or reindexing ragrep, preparing a shared DB, or investigating command, model, or DB problems.
---

# Setting up ragrep

First check the commands with `ragrep help`. If it is not found, run `go build -o ragrep.exe ./cmd/ragrep` in the ragrep source directory and put the resulting binary on `PATH`. Building requires Go, cgo, and a C compiler.

The first `ragrep init` downloads the model and runtime (about 310 MB) to the user cache. Use `.ragrep/index.db` as the shared DB and do not exclude it from Git management. However, hold off on initializing and indexing the shared DB until you have confirmed a version where document paths in the DB are relative to the project root.

After confirming a supported version, run `ragrep init` and `ragrep index <target>` from the project root. After materials are updated, re-index the same target, adding `--prune` when files have been deleted.

A SQLite DB cannot be content-merged in Git. If the DB was updated on multiple branches, decide which side to adopt and regenerate the index if needed. Specify `--db` or `RAGREP_DB` only when using something other than the default `.ragrep/index.db`.
