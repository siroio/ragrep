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
| Filter by tag (repeat = AND) | `ragrep search --tag design --json "q"` |
| Add a new tagged document (stdin body) | `ragrep add --tag design notes/foo.md` |
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
- Paths are stored as slash-separated keys relative to the workspace root
  (the directory containing `.ragrep/`; the root itself is `.`), and the
  default `--db` walks up to the nearest ancestor `.ragrep/index.db` — so
  commands work from any subdirectory, and `get` resolves its path argument
  as a stored key verbatim first (so a search hit's `doc` value works from
  any cwd), then relative to cwd — relative, `./x`, backslash, and absolute
  arguments all resolve to the same key. A workspace can be moved, renamed,
  or copied wholesale and its index stays valid. Paths outside the
  workspace root cannot be indexed or added (error, exit 1).
- **Exit codes**: 0 = success, 1 = error, 2 = no hits / not found.
  Exit 2 is a normal outcome, not a failure — broaden the query, try
  `--mode text` for exact identifiers, or check the path form.
- `hybrid`/`vector` load the embedding model (slow startup); use
  `--mode text` for exact strings like error codes.
- Tagged DBs use a v2 hash format for migration purposes: a DB indexed
  before tag support must go through one `ragrep index` re-run, which
  reindexes every document once, before `--tag` filtering works.

## Common Mistakes

| Mistake | Fix |
|---|---|
| Flags after the query | Put all flags before positional args |
| Error: DB uses the old absolute-key format | Delete `.ragrep/` (or the index db) and re-run `ragrep index` — no auto-migration |
| Treating exit 2 as an error | It means "no hits"; rephrase or switch mode |
| Exit 2 on content you know exists | Index may be stale; re-run `ragrep index <path>` |
| Fetching whole documents first | Start with `--para`, expand only as needed |
| Re-running `ragrep init` | Needed once per machine only |
