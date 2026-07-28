---
name: searching-code-with-ragrep
description: Use when a task requires locating or verifying code symbols (functions, methods, types) in a repo that has a ragrep code index (.ragrep/code.db) and the needed symbols/files are not already named in the request - finding where something is implemented, tracing callers/callees/references, or checking whether a claim about the code still holds. Do not use when every file and symbol the task needs is already given, or for general programming-language questions unrelated to this codebase. Triggers: "where is X implemented", "what calls Y", "find the function that...", シンボル検索, 呼び出し元.
---

# Searching code with ragrep

## Overview

`ragrep code` is a hybrid (vector + full-text) search over indexed code
symbols (functions, methods, types), backed by a separate database
(`.ragrep/code.db`, never the document `index.db`). Search only produces
**candidates** — a language server (LSP) verifies relations (definition,
references, callers, callees, tests) on demand. Treat yourself as the
Retrieval Planner: search broad, verify with LSP before trusting a relation,
fetch a symbol's body only for the 1-3 symbols you actually need to read.

## Setup

Requires a language server registered per-language in `.ragrep/config.json`;
only explicitly configured servers are ever launched — none is
auto-downloaded or installed.

```
{"servers": {"go": "gopls"}}
```

```
ragrep code index --language go <path>...   # index symbols (re-run after edits)
```

## Quick Reference

| Task | Command |
|---|---|
| Search symbols (candidates, no body) | `ragrep code search --json -k 5 "<query>"` |
| Fetch one symbol's metadata | `ragrep code get --symbol <key>` |
| Fetch metadata + body | `ragrep code get --symbol <key> --body` |
| LSP-verify a relation (1 hop) | `ragrep code expand --symbol <key> --relation definition\|references\|callers\|callees\|tests` |
| Assemble a budgeted context pack + manifest | `ragrep code pack --query "<q>" [--select <key>]... [--budget N] --json` |
| Re-check a saved manifest against the workspace | `ragrep code verify --manifest <file> --json` |

## Retrieval Workflow

1. Translate the request into English identifier-like keywords — identifiers
   and doc comments in the index are English, so querying in another
   language directly loses recall.
2. `ragrep code search --json -k 5 "<keywords>"` for candidates (metadata
   only, no body).
3. Before trusting a relation (this symbol's definition/references/
   callers/callees/tests), verify it with `ragrep code expand --symbol <key>
   --relation <relation>` — an LSP-backed query, not a guess from the search
   hit alone.
4. `ragrep code get --symbol <key> --body` for at most 1-3 symbols whose
   full body you actually need to read.

`ragrep code pack --query ... --select <key> --json` does steps 2 and 4 (plus
relations) in one call and emits a stale-detectable manifest for later
`code verify`.

## Ordering

- **spec → plan**: use `searching-with-ragrep` on the docs first to pin down
  the requirement, then this skill to locate the code it touches.
- **plan → implementation**: before reading code from a saved pack, run
  `ragrep code verify --manifest <file>` to re-resolve stable keys and catch
  files that changed since the pack was built — do not read straight off an
  old manifest.

## Rules

- **Flags go BEFORE positional args**, same as document `ragrep`.
- Search and pack results are **candidates**, not verified facts — a hit's
  relations are only confirmed by `code expand` (LSP) or an actual
  build/test run.
- Re-index (`ragrep code index --language <lang> <path>...`) when files
  changed since the last run; `code verify` on a saved manifest reports
  staleness without re-indexing.
- Only language servers registered in `.ragrep/config.json`'s `servers` map
  are launched — an unregistered language fails with a clear error, nothing
  is fetched automatically.

## Common Mistakes

| Mistake | Fix |
|---|---|
| Querying in the request's original (non-English) language | Translate intent into English identifier-style keywords first |
| Trusting a search hit's implied relation without verifying | Run `code expand --relation ...` before relying on it |
| Fetching full bodies for every candidate | `get --body` only the 1-3 symbols you'll actually read |
| Reading code straight from an old pack/manifest | `code verify --manifest` first; re-index if stale |
| Expecting an unregistered language to "just work" | Register its server in `.ragrep/config.json` `servers` — none is auto-installed |
