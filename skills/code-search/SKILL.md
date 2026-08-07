---
name: code-search
description: >-
  Use when a task requires locating or verifying code symbols (functions,
  methods, types) in a repo that has a ragrep code index (.ragrep/code.db) and
  the needed symbols/files are not already named in the request - finding where
  something is implemented, tracing callers/callees/references, or checking
  whether a claim about the code still holds. Do not use when every file and
  symbol the task needs is already given, or for general programming-language
  questions unrelated to this codebase. Triggers: "where is X implemented",
  "what calls Y", "find the function that...", シンボル検索, 呼び出し元.
---

# Searching code with ragrep

## Overview

Use the daemon-backed `.ragrep/code.db` to find current code symbols. Search
results are candidates; source, LSP expansion, and tests provide evidence.

## Workflow

Choose exactly one starting action:

- If the request names every needed file or symbol, inspect those paths or
  symbols directly with a source-reading command such as `rg` or
  `Get-Content`. Do not run semantic search.
- If anything must still be located, make this the first command:

```
ragrep code search --mode auto --json -k 5 "<query>"
```

Then follow the response:

- On `workspace_syncing`, run the same full `ragrep code search --mode auto
  --json -k 5 "<query>"` command again. Waiting alone is not recovery. Never
  substitute cached output.
- On `stale_live_key`, discard the key and body and run the same full search
  command again with the original query. Do not repeat `code get` with the
  stale key, use document `ragrep search`, guess, or implement from memory.
- Use hits only when the JSON response has `fresh: true`.

Select at most three hits and fetch only bodies you need:

`ragrep code get --symbol <key> --body`

Expand only the relation needed by the task:

`ragrep code expand --symbol <key> --relation <relation>`

Use one relation: `definition`, `references`, `callers`, `callees`, or `tests`.

## Rules

- Put flags before positional arguments.
- Exact symbol and path queries in `auto` mode avoid vector inference.
- Treat search hits as candidates. Confirm a relation with `code expand` and
  confirm behavior with source and tests.
- Only explicitly configured language servers run; none is installed
  automatically.
