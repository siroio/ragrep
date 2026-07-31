---
name: search
description: Use when answering or working requires exploring, cross-checking, or cross-referencing existing materials in the workspace, and not all of the needed text or each material's target files/sections are already provided. This includes cases where the question is clear or another specialized skill is used alongside.
---

# Searching with ragrep

資料の探索が必要なら、まず `ragrep search` を短い主要語句で使う。検索語の分け方、検索回数、本文取得や他の検索手段への切り替えは、資料の構造と未回答点に応じて判断する。

検索結果や `stale: true` は候補であり、断定は現行本文で確認する。再索引やDB変更は、依頼または許可がある場合だけ行う。

明記・未確定・未記述を区別し、上位概念の状態、一般的な行動、別属性の記述を質問された属性へ推測で拡張しない。必要な根拠が揃ったら停止し、確認できない点は不明として示す。`AGENTS.md` と資料の `status` を優先する。
