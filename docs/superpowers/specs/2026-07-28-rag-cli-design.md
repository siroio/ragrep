# rag CLI 設計書（技術選定・仕様）

基準文書: `基本設計書.md`（Adaptive Retrieval Unit Architecture）

## 目的

固定チャンクに依存しないRAG用の検索・取得CLIを作る。CLIは決定論的な検索システムに徹し、
Planner（取得単位の判断）と回答生成は呼び出し側のLLMエージェント（Claude Code等）が担う。

- 外部サービス不要・APIキー不要（埋め込みモデルはバイナリ内のONNX Runtimeで実行）
- インデックスはSQLite単一ファイル
- パーサレス: Markdownパーサ・AST解析は行わない

## スコープ外

- LLMによる回答生成（`ask`コマンド）— 将来拡張
- Markdown構造解析・コードAST解析によるSection/Function単位 — 将来拡張
- PDF/バイナリ形式の取り込み — テキストファイルのみ対象

## 技術スタック

| 項目 | 選定 | 理由 |
|---|---|---|
| 言語 | Go 1.23+ | シングルバイナリ配布・クロスコンパイル容易 |
| SQLite | `github.com/ncruces/go-sqlite3` | Pure Go（WASM/wazero）でCGo不要。FTS5同梱 |
| ベクトル検索 | `sqlite-vec`（`github.com/asg017/sqlite-vec-go-bindings/ncruces`） | FTS5と同一DBファイルに同居。専用ベクトルDB不要 |
| 埋め込み推論 | ONNX Runtime（`github.com/yalue/onnxruntime_go`） | 共有ライブラリは`rag init`時にGitHub ReleasesからDLしキャッシュ（go:embed不要でビルド単純化）。配布は単一バイナリ |
| 埋め込みモデル | `multilingual-e5-small`（quantized ONNX, 384次元） | 日本語対応・約120MB。初回`init`時にHugging FaceからDLしキャッシュ |
| トークナイザ | `github.com/eliben/go-sentencepiece`（Pure Go） | XLM-Rの`sentencepiece.bpe.model`を読み込み、fairseqオフセット(+1)でHF語彙IDへ変換 |
| CLI | 標準ライブラリ`flag` + サブコマンド分岐 | cobra等は不使用（YAGNI） |
| 日本語全文検索 | FTS5 trigramトークナイザ | 形態素解析なしで日本語部分一致が可能 |

キャッシュ配置: モデル・DLLは `~/.cache/rag/`（Windows: `%LOCALAPPDATA%\rag\`）。
DBファイルはカレントの `.rag/index.db`（`--db`で上書き可）。

## データモデル（パーサレス）

原文をそのまま保存し、取得単位は構造解析ではなく機械的に導出する。

- **Line** = 行（1始まり行番号）
- **Paragraph** = 空行区切りブロック（インデックスの基本単位）
- **Document** = ファイル全体

```sql
CREATE TABLE documents (
  id      INTEGER PRIMARY KEY,
  path    TEXT UNIQUE NOT NULL,   -- 相対パス
  content TEXT NOT NULL,          -- 原文そのまま
  mtime   INTEGER NOT NULL,
  hash    TEXT NOT NULL           -- 差分再インデックス用
);

CREATE TABLE paragraphs (
  id         INTEGER PRIMARY KEY,
  doc_id     INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL,    -- 文書内の段落番号（0始まり）
  start_line INTEGER NOT NULL,
  end_line   INTEGER NOT NULL,
  text       TEXT NOT NULL
);

-- fts/vec とも rowid = paragraphs.id
CREATE VIRTUAL TABLE fts USING fts5(text, tokenize='trigram');
CREATE VIRTUAL TABLE vec USING vec0(embedding float[384]);
```

埋め込み・FTSのインデックス単位は段落。長すぎる段落は埋め込み時に510トークンで
切り詰める（FTSは全文を保持するためハイブリッド検索で後半もヒットする）。

## コマンド仕様

### `rag init`
`.rag/index.db`作成、ONNX Runtime DLL展開、埋め込みモデルDL。冪等。

### `rag index <path>...`
- テキストファイルを再帰的に取り込み（バイナリはスキップ、`.gitignore`は考慮しない）
- hash/mtime比較で変更ファイルのみ再インデックス
- 段落分割 → 埋め込み生成 → FTS/vec登録を1トランザクションで実行

### `rag search <query>`
```
rag search "認証エラー" [--mode hybrid|vector|text] [-k 10] [--json]
```
- `text`: FTS5 (trigram) BM25
- `vector`: sqlite-vec KNN（クエリを埋め込み）
- `hybrid`（既定）: 両者をRRF（k=60）でマージ
- 出力（`--json`時）: `[{doc, para, lines: "12-18", score, snippet}]`
  ヒットは段落単位。エージェントはこの結果を見て取得単位を判断する。

### `rag get <path>`
```
rag get docs/auth.md                     # Document全体
rag get docs/auth.md --para 4            # 段落4
rag get docs/auth.md --para 4 --context 2  # 前後±2段落（Adaptive Expansion）
rag get docs/auth.md --lines 12-18       # 行範囲
```
常に原文を返す。拡張の梯子は `Line → Paragraph → ±N段落 → Document`。

### 終了コード
0=成功、1=エラー、2=ヒットなし（エージェントが分岐しやすいように）。

## 設計書との対応

| 基本設計書の概念 | 本CLIでの実現 |
|---|---|
| Retrieval Planner | 外部LLMエージェント（CLIは担わない） |
| Retrieval Unit | Line / Paragraph / ±N段落 / Document |
| Deterministic Retrieval | `rag get`（構造解析なしの機械的導出） |
| Adaptive Context Expansion | `--context N` → `--para`なし（Document）への段階拡張 |
| Section / Function / AST | スコープ外（将来拡張） |

## エラー処理

- モデル未取得で`search --mode vector|hybrid` → `rag init`を促すメッセージで終了コード1
- 未インデックスDB → 同上
- 埋め込み推論失敗 → `--mode text`へのフォールバックはしない（明示エラー。無言の品質劣化を避ける）

## テスト

- 段落分割・行番号計算のユニットテスト（境界: 連続空行・末尾改行なし・CRLF）
- RRFマージのユニットテスト
- E2E: 小コーパスで `init → index → search → get` を通すスモークテスト
  （埋め込みは実モデルで実行。CI不要、ローカル`go test`で完結）

## 将来拡張（実装しない）

- Markdown見出しベースのSection単位 / コードAST（Function単位）
- Retrieval Unit Tree
- `ask`コマンド（LLM内蔵）
- ウォッチモード・インクリメンタル更新デーモン
