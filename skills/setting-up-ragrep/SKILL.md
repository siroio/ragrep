---
name: setting-up-ragrep
description: Use when ragrepの導入、ビルド、初期化、索引更新、共有DBの準備、またはコマンド・モデル・DBの不具合を確認するときに使用する。
---

# Setting up ragrep

最初に `ragrep help` でコマンドを確認する。見つからない場合はragrepのソースディレクトリで `go build -o ragrep.exe ./cmd/ragrep` を実行し、生成物へ `PATH` を通す。ビルドにはGo、cgo、Cコンパイラが必要になる。

初回の `ragrep init` はモデルとランタイム約310 MBをユーザーキャッシュへ取得する。共有DBには `.ragrep/index.db` を使い、Git管理から除外しない。ただし、DB内の文書パスがプロジェクトルート相対になる版を確認するまでは、共有DBの初期化と索引作成を保留する。

対応版の確認後、プロジェクトルートから `ragrep init` と `ragrep index <対象>` を実行する。資料更新後は同じ対象を再索引し、削除されたファイルがある場合は `--prune` を付ける。

SQLite DBはGit上で内容をマージできない。複数ブランチでDBが更新された場合は採用する側を決め、必要なら索引を再生成する。`--db` または `RAGREP_DB` は、既定の `.ragrep/index.db` 以外を使う場合だけ指定する。
