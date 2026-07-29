---
name: search
description: Use when answering or working requires exploring, cross-checking, or cross-referencing existing materials in the workspace, and not all of the needed text or each material's target files/sections are already provided. This includes cases where the question is clear or another specialized skill is used alongside.
---

# Searching with ragrep

For questions that draw on repository materials, run `ragrep search --json -k 5` once with the question text as the first search command. Do not replace it with `rg` on grounds of speed or "rg is enough". Fetch only the candidates that support each point the answer needs with `ragrep get --para N`, and widen `--context` one step at a time only when headings, `status`, qualifying conditions, or causality are missing. Pass the search result's `para` to `--para N`; do not pass line numbers from `lines`. Fetch line ranges with `ragrep get --lines A-B`.

Each hit's `heading` field (a breadcrumb like `Auth > Errors`, omitted when the paragraph has none) shows which section a snippet belongs to — read it alongside the snippet to judge relevance before fetching. When more than a handful of candidates matter, run `search --json -k 10`, read every snippet and heading, and pick the 1-3 most relevant hits yourself with `get`; do not blindly `get` the top hit.

If there are no hits or unanswered points remain, re-search once with `ragrep` using different query terms. If the first query returns nothing useful, rephrase and retry with 2-3 alternative wordings — synonyms, English<->Japanese, broader terms — before concluding the index has no answer. Locate any points still without candidates using `rg`. Use `--mode text` for exact-match candidate searches. After narrowing candidates with ragrep, `rg` may be used to confirm headings, `status`, and exact matches. Fetch body text with `ragrep get`; do not draw conclusions from search snippets.

Judge something "undecided" or "not documented" only after checking the section that directly covers that point; absence from a different section is not evidence. Scores are only comparable within one result list: a large gap between hit 1 and hit 2 means high confidence, while a flat list means the query was too vague — rephrase rather than trusting rank 1. A hit marked `stale: true` in JSON (or `[stale]` in text output) means the source file changed since indexing; re-run `ragrep index` before trusting its content. Stop once every point has supporting evidence; read the full document only when paragraph boundaries make the call impossible. Treat exit code `2` as no hits. Give priority to `AGENTS.md` and each material's `status`.

Every answer must cite its evidence as `doc#para` (e.g. `docs/auth.md#3`). If no hit supports a point, say the index does not contain it — never answer from memory alone.
