---
name: setup
description: Use when installing, building, initializing, or reindexing ragrep, preparing a shared DB, or investigating command, model, or DB problems.
---

# Setting up ragrep

First check the commands with `ragrep help`. If it is not found, run `go -C src build -o ../ragrep.exe ./cmd/ragrep` from the ragrep repository root and put the resulting binary on `PATH`. Building requires Go, cgo, and a C compiler.

The first `ragrep init` downloads the model and runtime assets to the user cache. The total size depends on the host platform and selected runtime. If a download is interrupted, run `ragrep init` again to fetch only missing assets.

Use `.ragrep/index.db` as the shared workspace DB. `.ragrep/` contains generated local state and should remain excluded from Git. If a DB reports a schema or model incompatibility, keep the original DB and documents, then build and check a replacement at another path:

```
ragrep index --db .ragrep/index-rebuild.db <target>
ragrep search --db .ragrep/index-rebuild.db --mode text "check query"
```

After confirming the replacement, set the configured `db` to that path and restart the shared daemon. Do not delete or overwrite the original DB until the switch is verified.

From the project root, run `ragrep init` and `ragrep index <target>`. After materials are updated, re-index the same target, adding `--prune` when files have been deleted.

A SQLite DB cannot be content-merged in Git. If the DB was updated on multiple branches, decide which side to adopt and regenerate the index if needed. Specify `--db` or `RAGREP_DB` when using something other than the default `.ragrep/index.db`.
