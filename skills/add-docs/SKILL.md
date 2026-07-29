---
name: add-docs
description: Use when adding a new document to a ragrep-indexed corpus or tagging documents for filtered retrieval - frontmatter `tags:`, `ragrep add`, `search --tag`. Triggers: "add a note to the docs", "tag this document", ドキュメント追加, タグ付け.
---

# Adding documents with ragrep

## Overview

`ragrep add` creates a brand-new indexed document in one step: it reads the
body from stdin, writes it to the given path, and indexes it immediately —
no separate `ragrep index` call needed. It refuses to touch a path that
already exists. For editing an existing document, see Updating below. For
the search side of tags, see the search skill.

## Frontmatter tags

A tags block is plain YAML frontmatter at the top of the file:

```
---
tags: [design, api]
---

body text...
```

Block-list form also works:

```
---
tags:
  - design
  - api
---

body text...
```

Tags are lowercased at index time, so `Design` and `design` are the same tag
for filtering purposes.

## Quick Reference

| Task | Command |
|---|---|
| Add a new document, tagged (stdin body) | `ragrep add --tag design --tag api notes/foo.md` (see below) |
| Add a new document, no tags (stdin body) | `ragrep add notes/foo.md` (see below) |
| Update an existing document | edit the file, then `ragrep index notes/foo.md` |
| Search filtered by tag | `ragrep search --tag design "query"` |
| Search filtered by two tags (AND) | `ragrep search --tag design --tag api "query"` |

## Adding a new document

```
echo "body text" | ragrep add --tag design --tag api notes/foo.md
```

- The document body comes from stdin, not a flag or positional arg.
- `--tag` is repeatable. It only gets auto-injected as a `---\ntags: [...]\n---`
  block when the piped body has **no** frontmatter of its own. If the body
  already starts with a `---` frontmatter block, `--tag` is ignored and
  whatever tags are already in that block are what gets indexed.
- Tag values must not contain commas or `]` — both are part of the inline
  `tags: [a, b]` syntax and would corrupt the parsed list.
- `ragrep add` refuses to overwrite a file that already exists at `<path>` —
  it is for creating new documents only.
- The file is written and indexed as part of the same command; there is no
  need to follow up with `ragrep index`.

## Updating an existing document

`ragrep add` won't touch an existing file, so updates are a two-step, plain
edit + reindex, same as any other tracked document:

```
$EDITOR notes/foo.md
ragrep index notes/foo.md
```

Change the `tags:` frontmatter by hand the same way; the new tags take
effect on that `ragrep index` run.

## Filtering search by tag

```
ragrep search --tag design "query"
```

- `--tag` is repeatable on `search`; repeating it is an AND, not an OR — a
  hit must carry every named tag.
- Works in every search mode (`text`, `vector`, `hybrid`), not just hybrid.

## Rules

- **Flags go BEFORE positional args**, same as every other ragrep subcommand:
  `ragrep search --tag design "q"`, never `ragrep search "q" --tag design`.
  On `add`, all `--tag` flags come before the path, which is the sole
  positional: `ragrep add --tag design --tag api notes/foo.md`. Go's flag
  parser stops at the first non-flag argument, so `ragrep add notes/foo.md
  --tag design` parses `--tag` as a second positional arg, not a flag, and
  fails.
- **Exit codes** follow the same contract as the rest of ragrep: 0 = success,
  1 = error (includes refusing to overwrite an existing file on `add`),
  2 = no hits / not found (`search`/`get`).
- Paths outside the workspace root (the directory containing `.ragrep/`)
  cannot be added or indexed — error, exit 1.

## Common Mistakes

| Mistake | Fix |
|---|---|
| Putting `--tag` after the path (`ragrep add notes/foo.md --tag design`) | Flags go before the path: `ragrep add --tag design notes/foo.md` — Go's flag parser stops at the first positional arg |
| Expecting `--tag` to add tags to a body that already has frontmatter | It won't — edit the existing `tags:` block by hand instead |
| Using `ragrep add` to update a file | It refuses existing paths; edit + `ragrep index <path>` instead |
| Forgetting the body is read from stdin | Pipe or redirect it in (see fenced example above) |
| Expecting `--tag design --tag api` to be an OR | It's an AND — every named tag must be present |
