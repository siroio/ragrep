# ragrep

固定チャンクに依存しないRAG検索・取得CLI。テキストファイルを段落単位でインデックスし、
ハイブリッド検索（ベクトル + 全文）と Adaptive Context Expansion による取得を提供する。

- 埋め込みモデル: embeddinggemma-300m（quantized ONNX, 768次元）
- インデックス: SQLite（デフォルトはcwdから親方向に最も近い `.ragrep/index.db` を自動発見。
  `--db PATH` か環境変数 `RAGREP_DB` で明示指定でき、ワークスペース切り替えに使える。
  優先順位: `--db` > `RAGREP_DB` > 祖先の `.ragrep/index.db` > cwd の `.ragrep/index.db`）
  ワークスペースルートはDBパスから決まる（親ディレクトリ名が `.ragrep` ならその親、
  それ以外はDBファイル自身のあるディレクトリ）ため、`RAGREP_DB` /`--db` で切り替え先の
  ワークスペース自身の `.ragrep/index.db` を指す分には問題ないが、ワークスペース外の
  共有場所を指すDBパスを使うと索引対象がルート外と判定され失敗する。
- 対応プラットフォーム: windows/amd64, windows/arm64, linux/amd64, darwin/arm64

## Install / Build

```
git clone https://github.com/siroio/ragrep && cd ragrep
go build -o ragrep.exe ./cmd/ragrep    # unixは .exe 不要
```

ビルドにはcgo（Cツールチェーン）が必要（`github.com/yalue/onnxruntime_go`がcgo必須のため）。
Windowsは mingw-w64 か MSVC を入れて `CGO_ENABLED=1` でビルドする。

## Setup

```
ragrep init    # DB作成 + モデル・ランタイムをDL（初回のみ、~310MB）
```

アセットはリポジトリではなくユーザーキャッシュに入る
（Windows: `%LocalAppData%\ragrep` / Linux: `~/.cache/ragrep` / macOS: `~/Library/Caches/ragrep`）。
ダウンロードはファイル単位でアトミックなので、中断しても `ragrep init` を再実行すれば
足りないファイルだけ再取得される。リポジトリ側では `.ragrep/` を `.gitignore` に追加しておくこと。

## Usage

```
ragrep index docs/                            # テキストファイルをインデックス（再帰）
ragrep index --prune docs/                    # 削除済みファイルもインデックスから除去
ragrep search --json "認証エラー"              # ハイブリッド検索（段落単位でヒット）
ragrep search --mode text -k 5 "ERR_AUTH"     # 全文検索のみ（モデル不要で速い）
ragrep search --tag design "認証エラー"        # tagで絞り込み（複数指定でAND、全モード対応）
echo "本文" | ragrep add --tag design --tag api notes/foo.md  # 新規文書をstdinから追加＋即索引
ragrep get docs/auth.md                       # Document全体
ragrep get --para 4 --context 2 docs/auth.md  # 段落4±2（Adaptive Expansion）
ragrep get --lines 12-18 docs/auth.md         # 行範囲
```

- フラグは位置引数より前に置く。`-k N` で件数指定（デフォルト10）。
- `search --json` は `{doc, para, lines, score, snippet}` のJSON配列を出力。
- 終了コード: 0=成功 / 1=エラー / 2=ヒットなし・get未検出（2はエラーではない）。
- インデックスはドットディレクトリ・10MB超・バイナリファイルを自動スキップする。
- ドキュメントキーはワークスペース（`.ragrep/` を含むディレクトリ、ルート自身は `.`）
  からの相対スラッシュパスに正規化されるため、相対・`./x`・絶対のどの形で渡しても
  同じキーに解決され、サブディレクトリからの実行でも動く。ワークスペースごと
  移動・リネーム・コピーしてもインデックスは有効なまま使え、cwdの最寄り
  `.ragrep` に自動で切り替わる。ワークスペースルート外のパスは索引・追加できない
  （エラー、終了コード1）。
  **破壊的変更**: 旧形式（絶対パスキー）でインデックスしたDBは明示エラーで拒否
  される。`.ragrep/`（またはindex db）を削除して `ragrep index` を再実行すること
  （自動移行は行われない）。
- frontmatterに `---` / `tags: [a, b]`（またはブロックリスト） / `---` を書くとタグが付き、
  小文字化されて索引される。`ragrep add` は `--tag` 指定時、frontmatterが無い本文にのみ
  このブロックを自動付与する（既存ファイルへの上書きは拒否、更新はファイル編集＋
  `ragrep index <path>` の再実行で行う）。

## Agent Skills

`skills/` に [Agent Skills](https://agentskills.io) 形式のスキルを同梱。
エージェント（Claude Code / Codex / Copilot CLI / Gemini CLI等）がRetrieval Plannerとして
ragrepを使うための手順書で、スキルディレクトリへコピーするだけで使える。

| スキル | 内容 |
|---|---|
| `skills/searching-with-ragrep/` | 検索→取得→コンテキスト拡張のワークフロー |
| `skills/setting-up-ragrep/` | ビルド・init・インデックス運用・トラブルシュート |
| `skills/adding-documents-with-ragrep/` | `ragrep add`によるtag付き新規文書の追加 |

コピー先:

- Claude Code: `.claude/skills/`（プロジェクト）または `~/.claude/skills/`（全体）
- Codex / Copilot CLI / Gemini CLI: `~/.agents/skills/`

基本ワークフロー（Adaptive Context Expansion）:

1. `ragrep search --json "<質問のキーワード>"` で段落単位のヒットを得る
2. ヒットの `doc`/`para` を見て必要な取得単位を判断する
3. `ragrep get --para N` → 不足なら `--context` を増やす → それでも不足なら `ragrep get <doc>`
