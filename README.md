# ragrep

固定チャンクに依存しないRAG検索・取得CLI。テキストファイルを段落単位でインデックスし、
ハイブリッド検索（ベクトル + 全文）と Adaptive Context Expansion による取得を提供する。

- 埋め込みモデル: embeddinggemma-300m（quantized ONNX, 768次元）
- インデックス: SQLite（`.ragrep/index.db`。`--db PATH` か環境変数 `RAGREP_DB` で変更可。ワークスペース共有や切り替えに使える）
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
ragrep get docs/auth.md                       # Document全体
ragrep get --para 4 --context 2 docs/auth.md  # 段落4±2（Adaptive Expansion）
ragrep get --lines 12-18 docs/auth.md         # 行範囲
```

- フラグは位置引数より前に置く。`-k N` で件数指定（デフォルト10）。
- `search --json` は `{doc, para, lines, score, snippet}` のJSON配列を出力。
- 終了コード: 0=成功 / 1=エラー / 2=ヒットなし・get未検出（2はエラーではない）。
- インデックスはドットディレクトリ・10MB超・バイナリファイルを自動スキップする。
- `index`/`get` は常にインデックス時と同じカレントディレクトリから相対パスで実行すること。
  ドキュメントキーは入力パスそのままのため、`docs` と `D:\...\docs` は別キーになる。

## Agent Skills

`skills/` に [Agent Skills](https://agentskills.io) 形式のスキルを同梱。
エージェント（Claude Code / Codex / Copilot CLI / Gemini CLI等）がRetrieval Plannerとして
ragrepを使うための手順書で、スキルディレクトリへコピーするだけで使える。

| スキル | 内容 |
|---|---|
| `skills/searching-with-ragrep/` | 検索→取得→コンテキスト拡張のワークフロー |
| `skills/setting-up-ragrep/` | ビルド・init・インデックス運用・トラブルシュート |

コピー先:

- Claude Code: `.claude/skills/`（プロジェクト）または `~/.claude/skills/`（全体）
- Codex / Copilot CLI / Gemini CLI: `~/.agents/skills/`

基本ワークフロー（Adaptive Context Expansion）:

1. `ragrep search --json "<質問のキーワード>"` で段落単位のヒットを得る
2. ヒットの `doc`/`para` を見て必要な取得単位を判断する
3. `ragrep get --para N` → 不足なら `--context` を増やす → それでも不足なら `ragrep get <doc>`
