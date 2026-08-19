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

ビルドには cgo と C ツールチェーンが必要です。初回に取得するアセットはユーザーキャッシュに保存されます（Windows: `%LocalAppData%\ragrep`、Linux: `~/.cache/ragrep`、macOS: `~/Library/Caches/ragrep`）。中断した場合も `ragrep init` を再実行すれば不足分だけを取得します。

## 主な機能

- 文書を段落単位で索引し、`hybrid`（ベクトル + 全文）、`vector`、`text` 検索を提供
- 検索結果から必要な段落・行範囲・文書全体を取得
- frontmatter のタグ、削除済み文書の prune、明示設定した PDF/Office 変換コマンドに対応
- 文書 DB とは別の `code.db` でコードシンボルを検索し、LSP で関係を確認
- ローカル stdio MCP から、文書・コード検索の 9 tools を提供

## ワークスペースと DB

文書 DB の解決優先順位は、明示した `--db` > `RAGREP_DB` > cwd から親方向へ最初に見つかった `.ragrep` 内の `config.json` の `db`（未設定なら同じルートの `.ragrep/index.db`）> cwd の `.ragrep/index.db` です。文書キーはワークスペースルートからの相対スラッシュパスで保存されるため、サブディレクトリから実行しても同じ索引を利用できます。

文書用の `index.db` とコード用の `code.db` は分離されます。旧形式の絶対パスキーを持つ DB は自動移行されないため、DB を削除して再索引してください。SQLite DB は内容マージに向かないため、ブランチ間で競合した場合は一方を採用して再生成します。

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

PDF/Office などは `.ragrep/config.json` の `converters` に明示登録したコマンドの標準出力を索引します。未登録の変換コマンドを自動で取得・実行することはありません。

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

`ragrep mcp serve` はポートを開かず、次の 9 tools を公開します。

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

`skills/` には [Agent Skills](https://agentskills.io) 形式の手順書を同梱しています。

| スキル | 内容 |
|---|---|
| `skills/search/` | 文書検索、取得、段階的なコンテキスト拡張 |
| `skills/code-search/` | コード候補検索、LSP 関係確認、pack 検証 |
| `skills/setup/` | ビルド、init、索引運用、トラブルシュート |
| `skills/add-docs/` | `ragrep add` によるタグ付き新規文書の追加 |

各エージェントのプラグイン機構で直接インストールできます。

| エージェント | コマンド |
|---|---|
| Claude Code | `/plugin marketplace add siroio/ragrep` → `/plugin install ragrep@ragrep` |
| Codex | `codex plugin marketplace add siroio/ragrep` → `codex plugin add ragrep@ragrep` |
| Gemini CLI | `gemini extensions install https://github.com/siroio/ragrep` |
| GitHub Copilot CLI | `copilot plugin marketplace add siroio/ragrep` → `copilot plugin install ragrep@ragrep` |
| Kimi Code | `/plugins install https://github.com/siroio/ragrep` |
| Factory Droid | `droid plugin marketplace add https://github.com/siroio/ragrep` → `droid plugin install ragrep@ragrep` |
| Cursor / その他 | `npx skills add siroio/ragrep`（[skills.sh](https://skills.sh) で対話的に選択） |
