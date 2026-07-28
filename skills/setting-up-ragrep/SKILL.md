---
name: setting-up-ragrep
description: Use when ragrep is not yet available or broken - the ragrep binary is missing, `ragrep init` fails, cgo/C compiler build errors, onnxruntime download or DLL problems, deciding what to index or gitignore, or preparing a new repo for ragrep search. Triggers: "command not found: ragrep", "cgo: C compiler", ビルドできない, セットアップ.
---

# Setting up ragrep

## Overview

One-time setup has two layers: build the binary (per machine), then
`ragrep init` in each repo. The first `init` on a machine downloads ~310MB
of model assets to a per-user cache; later `init` runs in other repos find
the cache and only create that repo's DB. For searching, see the
searching-with-ragrep skill.

## Build

```
git clone https://github.com/siroio/ragrep && cd ragrep
go build -o ragrep.exe ./cmd/ragrep    # drop .exe on unix
```

- **cgo is required** (`github.com/yalue/onnxruntime_go`): needs Go plus a C
  toolchain (mingw-w64/MSVC on Windows, gcc/clang elsewhere) and
  `CGO_ENABLED=1` (the default when a compiler is present).
- Supported platforms: windows/amd64, windows/arm64, linux/amd64,
  darwin/arm64. Anything else fails at `init` with an unsupported-platform
  error.
- Put the binary on PATH or call it by absolute path from other repos.

## Init and asset cache

```
ragrep init        # creates .ragrep/index.db + downloads assets if missing
```

Downloads on first run: onnxruntime 1.26.0 shared library (GitHub releases)
and the embeddinggemma-300m quantized ONNX model + tokenizer (Hugging Face).
Assets land in the per-user cache, NOT the repo:

| OS | Cache dir |
|---|---|
| Windows | `%LocalAppData%\ragrep` |
| Linux | `~/.cache/ragrep` |
| macOS | `~/Library/Caches/ragrep` |

Downloads are atomic per file (written to a temp file, then renamed), so an
interrupted download leaves no partial asset. Re-running `ragrep init` skips
completed files and re-fetches only the missing ones.

## Preparing a repo

1. `ragrep init` from the repo root.
2. Add `.ragrep/` to `.gitignore` (the index DB is derived data).
3. `ragrep index docs/` (or whichever roots hold the text).
4. Re-run `ragrep index <path>` after edits; add `--prune` when files were
   deleted.

Indexing is recursive and silently skips: dot-directories (`.git` etc.),
files over 10MB, and binary files — anything with a NUL byte in the first
8KB, which catches images, archives, and most PDFs. "skipped" in the summary
line counts these — it is not an error. ragrep indexes plain text only;
convert PDFs to text first if you need them searchable.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `command not found: ragrep` | Build it (above) and put it on PATH |
| Build error mentioning cgo / "C compiler ... not found" | Install a C toolchain (mingw-w64 on Windows), retry |
| `init` download fails midway | Check network/proxy; re-run `ragrep init` (resumes) |
| Corrupt model / weird embedding errors | Delete the cache dir, re-run `ragrep init` |
| unsupported platform error | Only the four GOOS/GOARCH pairs above are supported |
| `search --mode hybrid` slow to start | Normal: it loads the ONNX model; use `--mode text` for exact strings |
| Expected file missing from results | It was skipped: >10MB, binary, or under a dot-directory |
