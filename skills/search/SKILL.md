---
name: search
description: Use when answering or working requires exploring, cross-checking, or cross-referencing existing materials in the workspace, and not all of the needed text or each material's target files/sections are already provided. This includes cases where the question is clear or another specialized skill is used alongside.
---

# Searching with ragrep

If materials need exploring, first use `ragrep search` with a short query of the main terms. Choose query splitting, search count, text retrieval, and whether/when to switch to other search methods based on the material's structure and unanswered points.

Support each claim with the material or section that directly confirms it.

Treat results and `stale: true` as candidates; verify current text before asserting. Reindex or change the DB only when requested or authorized.

Separate what materials explicitly state from what remains unresolved; do not fill gaps without evidence. For unverified points, state the checked scope only when needed. Stop when evidence suffices; mark unconfirmed points as unknown. Prioritize `AGENTS.md` and the material's `status`.
