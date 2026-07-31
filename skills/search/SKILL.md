---
name: search
description: Use when answering or working requires exploring, cross-checking, or cross-referencing existing materials in the workspace, and not all of the needed text or each material's target files/sections are already provided. This includes cases where the question is clear or another specialized skill is used alongside.
---

# Searching with ragrep

資料の探索が必要なら、まず `ragrep search` を短い主要語句で使う。検索語の分け方、検索回数、本文取得や他の検索手段への切り替えは、資料の構造と未回答点に応じて判断する。

回答の主張には、その主張を直接確認できる資料・節を根拠として添える。

検索結果や `stale: true` は候補であり、断定は現行本文で確認する。再索引やDB変更は、依頼または許可がある場合だけ行う。

資料に明記されたことと未確定のことを区別し、根拠のない補完をしない。未確認の点は、必要な場合だけ確認範囲を添えて示す。必要な根拠が揃ったら停止し、確認できない点は不明として示す。`AGENTS.md` と資料の `status` を優先する。
