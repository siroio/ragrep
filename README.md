# ragrep

固定チャンクに依存せず、テキストを段落単位で索引・検索する RAG 検索 CLI です。文書検索とコードシンボル検索を分け、必要な根拠だけを取得・検証する用途を想定しています。

## Quick Start

```powershell
git clone https://github.com/siroio/ragrep
cd ragrep
go -C src build -o ../ragrep.exe ./cmd/ragrep # Unix では .exe を省略

ragrep init                         # DB 作成とモデル・ランタイムの取得（初回のみ）
ragrep index docs/                  # テキスト文書を再帰的に索引
ragrep search --json "認証エラー"     # 段落単位で検索
ragrep get --para 4 --context 2 docs/auth.md
```

ビルドには Go 1.26.8、cgo、有効な C ツールチェーンが必要です。初回に取得するアセットはユーザーキャッシュに保存されます（Windows: `%LocalAppData%\ragrep`、Linux: `~/.cache/ragrep`、macOS: `~/Library/Caches/ragrep`）。中断した場合も `ragrep init` を再実行すれば不足分だけを取得します。

`ragrep init` は固定 revision の Hugging Face から埋め込みモデルと tokenizer を、Windows では NuGet から DirectML 対応ランタイムを、その他の対応 OS では GitHub Releases から ONNX Runtime を取得します。HTTP クライアントは各ホストのリダイレクトに従いますが、取得後に固定 SHA-256 を検証し、検証に失敗したファイルはキャッシュへ公開しません。社内プロキシやミラーを使う場合も、これらの外部ホストへの接続とリダイレクトを許可し、ハッシュ検証を無効化しないでください。

## 配布物の作成とインストール

配布物はホスト OS 向けに cgo を有効にして作成します。再現可能な ZIP、`RELEASE.json`、LICENSE、簡潔な INSTALL.md、SHA-256 ファイルを生成し、既存の出力は上書きしません。

```powershell
python tools/release/release.py build --source . --output .tmp/ragrep-windows-amd64.zip --target windows-amd64 --version 0.1.0
```

作成時の revision、dirty 状態、Go バージョン、target、明示指定した asset 情報を `RELEASE.json` に記録します。dirty source は clean release として扱いません。候補 target は Windows amd64/arm64、Linux amd64、macOS arm64 ですが、cgo クロスビルドは行わず各ホストで個別に作成してください。ZIP には依存物の notices も含まれます。ZIP を展開して `ragrep` を PATH に置き、対象ワークスペースで `ragrep init` を実行します。

このリポジトリで確認した実行環境は Windows amd64 です。その他の target は候補であり、この README では動作確認済みとは扱いません。

## 主な機能

- 文書を段落単位で索引し、`hybrid`（ベクトル + 全文）、`vector`、`text` 検索を提供
- 検索結果から必要な段落・行範囲・文書全体を取得
- frontmatter のタグ、削除済み文書の prune、明示設定した PDF/Office 変換コマンドに対応
- 文書 DB とは別の `code.db` でコードシンボルを検索し、LSP で関係を確認
- ローカル stdio MCP から、文書・コード検索の 9 tools を提供

## ワークスペースと DB

文書 DB の解決優先順位は、明示した `--db` > `RAGREP_DB` > cwd から親方向へ最初に見つかった `.ragrep` 内の `config.json` の `db`（未設定なら同じルートの `.ragrep/index.db`）> cwd の `.ragrep/index.db` です。文書キーはワークスペースルートからの相対スラッシュパスで保存されるため、サブディレクトリから実行しても同じ索引を利用できます。

`ragrep version` はビルド version、commit、ビルド日時を表示します。`ragrep doctor` は解決されたワークスペースと DB、文書 DB の互換性、埋め込みアセット、daemon 接続、設定済み言語サーバーを読み取り専用で確認します。`--json` で機械処理向けに出力できます。互換性エラーや不足アセットは終了コード 1、daemon と任意の言語サーバーの警告だけなら終了コード 0 です。doctor は daemon を起動せず、アセットや設定を作成しません。

文書用の `index.db` とコード用の `code.db` は分離されます。旧形式の絶対パスキーや互換性のないメタデータを持つ DB は自動移行されません。SQLite DB は内容マージに向かないため、ブランチ間で競合した場合は一方を採用して再生成します。

文書本文、段落、検索用スニペット、コード本文などの派生内容は、DB に保存されます（既定はワークスペースの `.ragrep/index.db` / `.ragrep/code.db`、指定により別の場所にも保存できます）。設定は `.ragrep/config.json`、daemon の検出情報と共有ワークスペース一覧はユーザーの設定・キャッシュ領域に保存されます。これらは暗号化されないローカルデータとして扱い、共有端末やバックアップではアクセス権を制限してください。

### 文書 DB の安全な再生成

DB の schema/model 不一致が表示された場合は、元の `index.db` と文書ファイルを保持したまま、同じワークスペースの別パスへ新しい DB を作成します。バイナリ更新後は共有 daemon を再起動してから切り替えてください。

```powershell
ragrep index --db .ragrep/index-rebuild.db docs/
ragrep search --db .ragrep/index-rebuild.db --mode text "確認用の語句"
```

索引と検索結果を確認できたら `.ragrep/config.json` の `db` を新しい DB に変更します。中断しても元の DB と原本は残るため、同じ手順を再実行できます。元の DB の削除・上書き・自動リネームは行いません。更新時は旧バイナリ、設定、旧 DB を先に退避して保持し、切り替え後に `ragrep daemon stop` と `ragrep daemon start` を実行してください。問題があれば設定の `db` とバイナリを旧版へ戻し、daemon を再起動してロールバックします。

## 文書の索引・検索・取得

```powershell
ragrep index docs/
ragrep index --prune docs/                       # 削除済み文書も除去
ragrep index --include-code docs/                # 通常除外するソース拡張子も文書として索引
ragrep search --mode text -k 5 "ERR_AUTH"
ragrep search --tag design --tag api "認証"       # 複数タグは AND
ragrep search --json --expand-top 2 --expand-budget 4000 "認証エラー"
ragrep get docs/auth.md
ragrep get --para 4 --context 2 docs/auth.md
ragrep get --lines 12-18 docs/auth.md
```

`search --json` の基本フィールドは `doc`、`para`、`lines`、`score`、`snippet` です。必要に応じて `heading` と `stale` も出力します。`--expand-top` と `--expand-budget` は両方を正数で指定し、`--json` と組み合わせてください。選択された上位ヒットには `body` が追加され、必要に応じて `body_truncated` が付きます。

`ragrep search` は共有 daemon を必要に応じて透過的に起動します。状態確認や明示的な操作には次を使います。

```powershell
ragrep daemon start
ragrep daemon status
ragrep daemon stop
```

1 つの daemon は複数ワークスペースを扱えます。ワークスペースごとに document/code DB の状態は分離されます。明示的に管理する場合は daemon を起動してから使用します。

```powershell
ragrep workspace add
ragrep workspace add D:\Works\other-project
ragrep workspace list
ragrep workspace remove D:\Works\other-project
```

終了コードは `0` が成功、`1` がエラー、`2` が非エラーの否定結果（ヒットなし、未検出、ワークスペース一覧が空、検証未通過）です。インデックスはドットディレクトリ、10 MB 超、バイナリファイルをスキップします。位置引数の前後どちらにフラグを書いても受け付けます。

### 追加とタグ

```powershell
echo "本文" | ragrep add --tag design --tag api notes/foo.md
ragrep search --tag design "query"
```

`ragrep add` は stdin の本文から新規文書を作成して即時索引します。既存ファイルは上書きしません。既存文書は編集後に `ragrep index <path>` を実行してください。`---` で囲んだ YAML frontmatter の `tags: [design, api]` またはブロックリストも索引できます。タグは小文字化され、繰り返した `--tag` は AND 条件です。

### 文書変換

PDF/Office などは `.ragrep/config.json` の `converters` に明示登録したコマンドの標準出力を索引します。未登録の変換コマンドを自動で取得・実行することはありません。登録した converter は入力ファイルを外部プログラムへ渡し、その標準出力を索引します。コマンドと実行権限、入力データの取り扱いは利用者が管理してください。

```json
{
  "converters": {
    ".pdf": ["pdftotext", "{input}", "-"],
    ".docx": ["pandoc", "{input}", "-t", "plain"]
  }
}
```

## コード検索

コード検索は文書検索とは別の `code.db` を使います。コード DB の解決優先順位は `--db` > `.ragrep/config.json` の `code_db` > `.ragrep/code.db` です。現在 `code index` が対応する言語は Go のみです。使用する言語サーバーは `.ragrep/config.json` の `servers` に明示設定してください。未設定のサーバーは起動・自動インストールされず、他の言語のサーバーを設定するだけでは `code index` の対応言語になりません。

```json
{"servers": {"go": "gopls"}}
```

```powershell
ragrep code index --language go .
ragrep code search --mode auto --json -k 5 "parse config"
ragrep code get --symbol <key> --body
ragrep code expand --symbol <key> --relation references
ragrep code pack --query "parse config" --select <key> --json
ragrep code verify --manifest pack.json --json
```

`--mode` は `auto`、`text`、`hybrid` を選べます。検索結果は候補です。候補を検索し、必要な本文を `code get --body` で読み、`code expand` で LSP の relation（`definition`、`references`、`callers`、`callees`、`tests`）を確認してから pack を組み立て、利用直前に `code verify` で検証してください。`-k` は正数なら任意に指定でき、省略時は各コマンドの既定値を使います。

LSP は `.ragrep/config.json` の `servers` に登録した実行ファイルだけを起動します。サーバーのインストール、更新、権限、対応機能は ragrep の管理範囲外です。現在 `code index` の対応言語は Go だけで、関係取得は接続したサーバーが対応する機能に依存します。 現在の実環境検証は Windows amd64 の小さい Go パッケージに限られます。80ファイルの索引試行は完了を確認できず中断しており、大きいプロジェクトとすべての relation の保証はしていません。

## MCP server

stdio MCP クライアントには次のように設定します。`cwd` を既定ワークスペースとし、`root` は別ワークスペースを明示するときだけ使います。

```json
{
  "mcpServers": {
    "ragrep": {
      "command": "ragrep",
      "args": ["mcp", "serve"],
      "cwd": "/absolute/path/to/workspace"
    }
  }
}
```

MCP 接続は stdio を使います。検索時には共有 daemon が loopback ポートを使用します。次の 9 tools を公開します。Codex CLI v0.153.4（Windows amd64）では、candidate 配布バイナリを使った `search_documents` → `read_document` の実接続を確認済みです。その他のクライアントと agent plugin の実接続は未検証です。

- 文書: `search_documents`、`read_document`、`add_document`、`reindex_documents`
- コード: `search_code`、`read_code_symbol`、`inspect_code_relation`、`build_code_context`、`verify_code_context`

`add_document` は新規作成専用で既存文書を上書きしません。`reindex_documents` は指定パスを再索引します。コード索引の更新は `ragrep code index` を使います。

## 評価

`ragrep eval` は JSONL のクエリと正解ドキュメントの組から recall@k を測定します。

```json
{"query":"認証エラーの対処法","doc":"docs/auth.md","para":2}
```

```powershell
ragrep eval cases.jsonl --mode hybrid -k 10
```

`para` を省略すると文書単位、数値を指定すると段落単位で評価します。出力はミスしたケースと `recall@10: 0.667 (2/3)` 形式の要約です。

## Agent Skills

文書探索の取得手順と実入力トークンの小規模測定を製品評価で確認しました。現時点の測定ではトークン削減を示せず、質問品質の維持も確認できていないため、同梱スキルのトークン削減を保証しません。

`skills/` には [Agent Skills](https://agentskills.io) 形式の手順書を同梱しています。

| スキル | 内容 |
|---|---|
| `skills/search/` | 文書検索、取得、段階的なコンテキスト拡張 |
| `skills/code-search/` | コード候補検索、LSP 関係確認、pack 検証 |
| `skills/setup/` | ビルド、init、索引運用、トラブルシュート |
| `skills/add-docs/` | `ragrep add` によるタグ付き新規文書の追加 |

以下はクライアント別のインストール例です。各クライアントでの動作は未検証のため、使用中のバージョンのプラグイン機構を確認してください。

| エージェント | コマンド |
|---|---|
| Claude Code | `/plugin marketplace add siroio/ragrep` → `/plugin install ragrep@ragrep` |
| Codex | `codex plugin marketplace add siroio/ragrep` → `codex plugin add ragrep@ragrep` |
| Gemini CLI | `gemini extensions install https://github.com/siroio/ragrep` |
| GitHub Copilot CLI | `copilot plugin marketplace add siroio/ragrep` → `copilot plugin install ragrep@ragrep` |
| Kimi Code | `/plugins install https://github.com/siroio/ragrep` |
| Factory Droid | `droid plugin marketplace add https://github.com/siroio/ragrep` → `droid plugin install ragrep@ragrep` |
| Cursor / その他 | `npx skills add siroio/ragrep`（[skills.sh](https://skills.sh) で対話的に選択） |

## セキュリティとサポート

外部ダウンロード、converter、LSP、MCP クライアントの実行境界は利用者の環境に依存します。問い合わせ・障害報告は [GitHub Issues](https://github.com/siroio/ragrep/issues) へ、`ragrep version`、OS/アーキテクチャ、実行したサブコマンド、`ragrep doctor --json` の結果、再現手順を添えてください。原文、本文、パス、設定値、daemon token、認証情報、組織名などは必ず伏せ、秘密を含む DB やログは添付しないでください。公開 issue に機密を含む詳細を送らないでください。
