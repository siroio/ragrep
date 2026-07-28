---
name: searching-with-ragrep
description: Use when answering questions from a document corpus in a repo that has a ragrep index (.ragrep/index.db) or a docs directory to index - semantic/RAG search, when keyword grep misses related content, or when creating/updating a ragrep index. Triggers: "search the docs", "where is X documented", 検索, ドキュメント検索, RAG retrieval.
---

# Searching with ragrep

## Overview

`ragrep` is a hybrid (vector + full-text) search CLI over indexed text files.
Hits are paragraph-level. You are the Retrieval Planner: search broad, fetch
the smallest unit that answers, expand only if insufficient (Adaptive Context
Expansion). Prefer it over plain grep when the query is conceptual rather
than an exact string.

## Setup

```
ragrep init            # once per machine: downloads model + runtime (~310MB)
ragrep index docs/     # index text files recursively (re-run after edits)
ragrep index --prune docs/   # also drop deleted files from the index
```

## Quick Reference

| Task | Command |
|---|---|
| Hybrid search (default, best) | `ragrep search --json "auth error"` |
| Full-text only (fast, no model) | `ragrep search --mode text -k 5 "ERR_AUTH"` |
| Vector only | `ragrep search --mode vector "concept"` |
| Get paragraph N | `ragrep get --para 4 docs/auth.md` |
| Paragraph ± N context | `ragrep get --para 4 --context 2 docs/auth.md` |
| Line range | `ragrep get --lines 12-18 docs/auth.md` |
| Whole document | `ragrep get docs/auth.md` |
| Alternate index | `--db PATH` or env `RAGREP_DB` (default `.ragrep/index.db`) |

`-k N` limits results (default 10, all modes). `search --json` prints a JSON
array of `{doc, para, lines, score, snippet}`. `--json` exists only on
`search`; `get` always prints plain text.

## Retrieval Workflow

1. `ragrep search --json "<query keywords>"`
2. Pick the promising hit; pass its `doc` value verbatim as the `get` path.
3. `ragrep get --para N <doc>` → not enough? add/raise `--context` →
   still not enough? `ragrep get <doc>` (whole document).

## Rules

- **Flags go BEFORE positional args**: `ragrep search --json "q"`, never
  `ragrep search "q" --json`.
- **Run all commands from the directory where `index` was run** (normally the
  repo root). Document keys are relative to the index-time cwd, and the
  default `--db .ragrep/index.db` is also cwd-relative — from a subdirectory
  both mismatch. `cd` back rather than juggling `--db`.
- Separators and trailing slashes are normalized (`docs\auth.md` ==
  `docs/auth.md`), but relative vs absolute is not: `docs` and
  `D:\...\docs` are different keys.
- **Exit codes**: 0 = success, 1 = error, 2 = no hits / not found.
  Exit 2 is a normal outcome, not a failure — broaden the query, try
  `--mode text` for exact identifiers, or check the path form.
- `hybrid`/`vector` load the embedding model (slow startup); use
  `--mode text` for exact strings like error codes.

## Common Mistakes

| Mistake | Fix |
|---|---|
| Flags after the query | Put all flags before positional args |
| Running from a subdirectory → exit 2 / missing DB | `cd` to the index-time cwd (repo root) |
| Indexed as `docs/`, get with absolute path → exit 2 | Copy the `doc` value from search hits |
| Treating exit 2 as an error | It means "no hits"; rephrase or switch mode |
| Exit 2 on content you know exists | Index may be stale; re-run `ragrep index <path>` |
| Fetching whole documents first | Start with `--para`, expand only as needed |
| Re-running `ragrep init` | Needed once per machine only |
