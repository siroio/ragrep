# コンバーターとログ

コンバーターは入力 payload をバージョン付き HarborQueue envelope に変換する。`message_id` を保持し、未知の必須フィールドを拒否し、envelope に converter version を出力する。

ログは利用可能なら `request_id`、`queue`、`message_id`、`lease_id` を持つ構造化 JSON にする。payload 本文は既定で記録せず、調査時だけ秘匿化した payload hash を有効にできる。

`hqctl logs --request-id` で broker と consumer のログを一つの操作として追跡する。request id がないのはコンバーターまたはクライアントの境界バグである。
