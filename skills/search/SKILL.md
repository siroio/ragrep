---
name: search
description: Use when answering or working requires exploring, cross-checking, or cross-referencing existing materials in the workspace, and not all of the needed text or each material's target files/sections are already provided. This includes cases where the question is clear or another specialized skill is used alongside.
---

# Searching with ragrep

For questions that draw on repository materials, run `ragrep search --json -k 5` once with the question text as the first search command. Do not replace it with `rg` on grounds of speed or "rg is enough". Fetch only the candidates that support each point the answer needs with `ragrep get --para N`, and widen `--context` one step at a time only when headings, `status`, qualifying conditions, or causality are missing. Pass the search result's `para` to `--para N`; do not pass line numbers from `lines`. Fetch line ranges with `ragrep get --lines A-B`.

If there are no hits or unanswered points remain, re-search once with `ragrep` using different query terms. Locate any points still without candidates using `rg`. Use `--mode text` for exact-match candidate searches. After narrowing candidates with ragrep, `rg` may be used to confirm headings, `status`, and exact matches. Fetch body text with `ragrep get`; do not draw conclusions from search snippets.

Judge something "undecided" or "not documented" only after checking the section that directly covers that point; absence from a different section is not evidence. Stop once every point has supporting evidence; read the full document only when paragraph boundaries make the call impossible. Treat exit code `2` as no hits. Give priority to `AGENTS.md` and each material's `status`.
