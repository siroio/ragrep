# rag

固定チャンクに依存しないRAG検索・取得CLI。詳細設計は
`docs/superpowers/specs/2026-07-28-rag-cli-design.md` を参照。

## Install / Build

    go build -o rag.exe .

## Usage

    rag init                     # モデル・ランタイムをDL（初回のみ、~200MB）
    rag index docs/              # テキストファイルをインデックス
    rag search --json "認証エラー"          # ハイブリッド検索（段落単位でヒット）
    rag search --mode text -k 5 "ERR_AUTH"  # 全文検索のみ
    rag get docs/auth.md                     # Document全体
    rag get --para 4 --context 2 docs/auth.md  # 段落4±2（Adaptive Expansion）
    rag get --lines 12-18 docs/auth.md       # 行範囲

フラグは位置引数より前に置く。終了コード: 0=成功 / 1=エラー / 2=ヒットなし。

## LLMエージェントからの利用

エージェント（Claude Code等）がRetrieval Plannerを担う:

1. `rag search --json "<質問のキーワード>"` で段落単位のヒットを得る
2. ヒットのdoc/paraを見て必要な取得単位を判断する
3. `rag get --para N [--context N]` → 不足なら `--context` を増やす → それでも不足なら `rag get <doc>` （Adaptive Context Expansion）
