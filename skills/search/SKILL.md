---
name: search
description: Use when answering or working requires exploring, cross-checking, or cross-referencing existing materials in the workspace, and not all of the needed text or each material's target files/sections are already provided. This includes cases where the question is clear or another specialized skill is used alongside.
---

# Searching with ragrep

資料を参照する作業では、質問を「別の資料領域を探す必要がある独立論点」だけに分ける。同じ人物・機能・案件についての属性群や確認項目は一つの論点にまとめる。各独立論点から依頼表現を除いた短い検索語句を作り、`ragrep search --json -k 5` で探す。検索回数を先に固定せず、本文取得後も残った未回答論点だけを語句を変えて再検索する。完全一致の候補探索には `--mode text`、索引で見つからない候補の確認には `rg` を使ってよい。

検索スニペットは候補探索にだけ使い、断定は本文に基づける。一つの文書に多数の回答項目が集約されていると分かったら、重複する `--para` 取得を続けず、必要な節を一度の `--lines`、または文書を一度の取得で読む。それ以外は必要な候補だけを `ragrep get --para N` で取得し、見出し・`status`・限定条件・因果が不足する場合だけ `--context` を1段ずつ広げる。`stale: true` の候補は探索専用とし、再索引が依頼済みまたは許可済みなら再索引して検索し直し、それ以外は現行作業ツリーのファイルを直接確認する。

「明記あり」「未確定」「未記述」は、質問された属性そのものへの記述で判定する。上位概念の状態、一般的な行動、別属性の記述を個別属性へ拡張しない。全論点に根拠が揃ったら停止する。根拠は `doc#para` で示すが、作業ツリーを直接確認した場合や一度取得済みの全文・行範囲を根拠にする場合は `path:lines` でよく、引用形式を揃えるためだけに再取得しない。終了コード `2` はヒットなしとして扱い、`AGENTS.md` と資料の `status` を優先する。
