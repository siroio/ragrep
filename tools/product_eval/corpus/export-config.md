# エクスポートと設定

`hqctl export --queue name --since timestamp` は改行区切り JSON を出力する。出力には message id、投入時刻、payload、acknowledgement 状態を含めるが、active lease token は含めない。出力先のアクセスも制限する。

設定は `harborqueue.yaml` から読み込み、その後に環境変数がファイル値を上書きする。秘密情報は `HQ_TOKEN_FILE` または secret manager から取得し、YAML に直接 token を書くと検証で拒否される。

削除前にレコード数と checksum を確認してエクスポートをテストする。エクスポートはコピー操作であり、メッセージを acknowledgement したり削除したりしない。
