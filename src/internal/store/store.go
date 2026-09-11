package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// ErrNotFound is returned when a requested document or paragraph doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrReindexRequired means this database was created by an incompatible or
// older document indexer. The caller must build a fresh database and reindex.
var ErrReindexRequired = errors.New("document database requires re-indexing")

// ErrNotDocumentDatabase means path contains a database belonging to another
// store or application.
var ErrNotDocumentDatabase = errors.New("not a document database")

const embedDim = 768
const schemaVersion = 1

const modelRevision = "5090578d9565bb06545b4552f76e6bc2c93e4a66"

func embeddingIdentity() string {
	model := "model_quantized.onnx"
	if runtime.GOOS == "windows" {
		model = "model.onnx"
	}
	return "embeddinggemma-300m-ONNX@" + modelRevision + "/" + model
}

// EmbeddingIdentity returns the pinned graph identity used by document
// indexes on this platform.
func EmbeddingIdentity() string { return embeddingIdentity() }

// EmbeddingDimension returns the vector width required by the document index.
func EmbeddingDimension() int { return embedDim }

// SchemaVersion returns the document index schema version.
func SchemaVersion() int { return schemaVersion }

type EmbedFunc func(text string) ([]float32, error)

type Hit struct {
	Doc           string  `json:"doc"`
	Para          int     `json:"para"`
	Lines         string  `json:"lines"`
	Score         float64 `json:"score"`
	Snippet       string  `json:"snippet"`
	Heading       string  `json:"heading,omitempty"`
	Mtime         int64   `json:"-"`
	Stale         bool    `json:"stale,omitempty"`
	Body          string  `json:"body,omitempty"`
	BodyTruncated bool    `json:"body_truncated,omitempty"`
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE ragrep_meta(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS documents(
  id INTEGER PRIMARY KEY,
  path TEXT UNIQUE NOT NULL,
  content TEXT NOT NULL,
  mtime INTEGER NOT NULL,
  hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS paragraphs(
  id INTEGER PRIMARY KEY,
  doc_id INTEGER NOT NULL,
  seq INTEGER NOT NULL,
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  text TEXT NOT NULL,
  heading TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_para_doc ON paragraphs(doc_id, seq);
CREATE TABLE IF NOT EXISTS doc_tags(
  doc_id INTEGER NOT NULL,
  tag TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_doc_tags ON doc_tags(tag, doc_id);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(text, tokenize='trigram');
CREATE VIRTUAL TABLE IF NOT EXISTS vec USING vec0(embedding float[768]);
`

// Open opens (creating if needed) the SQLite index at path and ensures schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", storeDSN(path))
	if err != nil {
		return nil, err
	}
	version, err := readUserVersion(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if version == 0 {
		empty, err := isEmpty(db)
		if err != nil {
			db.Close()
			return nil, err
		}
		if !empty {
			legacy, err := hasTable(db, "documents")
			if err != nil {
				db.Close()
				return nil, err
			}
			if legacy {
				db.Close()
				return nil, fmt.Errorf("%w: unversioned document index at %s", ErrReindexRequired, path)
			}
			db.Close()
			return nil, fmt.Errorf("%w: %s already has tables", ErrNotDocumentDatabase, path)
		}
		if err := createSchema(db); err != nil {
			db.Close()
			return nil, err
		}
	} else if version != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("%w: schema version %d, want %d", ErrReindexRequired, version, schemaVersion)
	} else if err := validateMeta(db); err != nil {
		db.Close()
		return nil, err
	} else if err := validateSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Check validates an existing document index without creating or modifying it.
func Check(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrNotDocumentDatabase
	}
	db, err := sql.Open("sqlite3", storeReadOnlyDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	version, err := readUserVersion(db)
	if err != nil {
		return err
	}
	if version == 0 {
		empty, err := isEmpty(db)
		if err != nil {
			return err
		}
		if empty {
			return ErrReindexRequired
		}
		legacy, err := hasTable(db, "documents")
		if err != nil {
			return err
		}
		if legacy {
			return ErrReindexRequired
		}
		return ErrNotDocumentDatabase
	}
	if version != schemaVersion {
		return ErrReindexRequired
	}
	if err := validateMeta(db); err != nil {
		return err
	}
	return validateSchema(db)
}

func readUserVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow(`PRAGMA user_version`).Scan(&version)
	return version, err
}

func isEmpty(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&count)
	return count == 0, err
}

func hasTable(db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	return count != 0, err
}

func createSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ragrep_meta(key, value) VALUES ('embedding_identity', ?), ('embedding_dim', ?)`, embeddingIdentity(), fmt.Sprint(embedDim)); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func validateMeta(db *sql.DB) error {
	present, err := hasTable(db, "ragrep_meta")
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: metadata table is missing", ErrNotDocumentDatabase)
	}
	rows, err := db.Query(`SELECT key, value FROM ragrep_meta`)
	if err != nil {
		return err
	}
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return err
		}
		meta[key] = value
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	wantDim := fmt.Sprint(embedDim)
	if meta["embedding_identity"] != embeddingIdentity() || meta["embedding_dim"] != wantDim {
		return fmt.Errorf("%w: db has embedding_identity=%q embedding_dim=%q, want embedding_identity=%q embedding_dim=%q", ErrReindexRequired, meta["embedding_identity"], meta["embedding_dim"], embeddingIdentity(), wantDim)
	}
	return nil
}

func validateSchema(db *sql.DB) error {
	required := map[string][]string{
		"documents":  {"id", "path", "content", "mtime", "hash"},
		"paragraphs": {"id", "doc_id", "seq", "start_line", "end_line", "text", "heading"},
		"doc_tags":   {"doc_id", "tag"},
		"fts":        {"text"},
		"vec":        {"embedding"},
	}
	for table, want := range required {
		columns, err := tableColumns(db, table)
		if err != nil {
			return fmt.Errorf("%w: inspect %s schema: %w", ErrReindexRequired, table, err)
		}
		for _, column := range want {
			if !columns[column] {
				return fmt.Errorf("%w: %s is missing required column %s", ErrReindexRequired, table, column)
			}
		}
	}
	var createSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='vec'`).Scan(&createSQL); err != nil {
		return fmt.Errorf("%w: inspect vec definition: %w", ErrReindexRequired, err)
	}
	if !strings.Contains(strings.ToLower(createSQL), "embedding float["+fmt.Sprint(embedDim)+"]") {
		return fmt.Errorf("%w: vec embedding dimension is not %d", ErrReindexRequired, embedDim)
	}
	return nil
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// HashContent returns the versioned content hash used for change detection.
// "v4\x00" versions it: bumping the prefix forces every doc to re-index once
// on the next `index` run (v2 = frontmatter tags, so pre-tags DBs get
// doc_tags populated; v3 = heading breadcrumbs are now part of the embed
// text, so every doc must re-embed; v4 = Windows switched to the fp32 model
// on DirectML, so its embeddings changed).
func HashContent(content string) string {
	h := sha256.Sum256([]byte("v4\x00" + content))
	return hex.EncodeToString(h[:])
}

// UpsertDoc indexes content under relPath, keyed by its own content hash.
func (s *Store) UpsertDoc(relPath, content string, mtime int64, embed EmbedFunc) (bool, error) {
	return s.UpsertDocContext(context.Background(), relPath, content, mtime, embed)
}

// UpsertDocContext is UpsertDoc with cancellation propagated through its transaction.
func (s *Store) UpsertDocContext(ctx context.Context, relPath, content string, mtime int64, embed EmbedFunc) (bool, error) {
	return s.UpsertDocWithHashContext(ctx, relPath, content, mtime, HashContent(content), embed)
}

// UpsertDocWithHash is UpsertDoc with a caller-supplied hash: used by the
// document-converter index path, which hashes the ORIGINAL (pre-conversion)
// file bytes so an unchanged source file skips re-running the converter, not
// just re-embedding.
func (s *Store) UpsertDocWithHash(relPath, content string, mtime int64, hash string, embed EmbedFunc) (bool, error) {
	return s.UpsertDocWithHashContext(context.Background(), relPath, content, mtime, hash, embed)
}

// UpsertDocWithHashContext is UpsertDocWithHash with cancellation propagated through its transaction.
func (s *Store) UpsertDocWithHashContext(ctx context.Context, relPath, content string, mtime int64, hash string, embed EmbedFunc) (bool, error) {
	return s.upsertDocWithHashContext(ctx, relPath, content, mtime, hash, embed, nil)
}

func (s *Store) upsertDocWithHashContext(ctx context.Context, relPath, content string, mtime int64, hash string, embed EmbedFunc, beforeCommit func()) (bool, error) {
	var docID int64
	var oldHash string
	var oldMtime int64
	err := s.db.QueryRowContext(ctx, `SELECT id, hash, mtime FROM documents WHERE path=?`, relPath).Scan(&docID, &oldHash, &oldMtime)
	if err == nil && oldHash == hash {
		if oldMtime != mtime {
			// Content is unchanged but the on-disk mtime moved (git checkout,
			// touch, re-save): refresh it so markStale doesn't flag this doc
			// forever -- without this, `ragrep index` never sees a "change"
			// to clear the stale flag.
			if _, err := s.db.ExecContext(ctx, `UPDATE documents SET mtime=? WHERE id=?`, mtime, docID); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	exists := err == nil

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if exists { // existing doc: purge old paragraphs from all indexes
		for _, q := range []string{
			`DELETE FROM fts WHERE rowid IN (SELECT id FROM paragraphs WHERE doc_id=?)`,
			`DELETE FROM vec WHERE rowid IN (SELECT id FROM paragraphs WHERE doc_id=?)`,
			`DELETE FROM paragraphs WHERE doc_id=?`,
			`DELETE FROM doc_tags WHERE doc_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, docID); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET content=?, mtime=?, hash=? WHERE id=?`,
			content, mtime, hash, docID); err != nil {
			return false, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `INSERT INTO documents(path, content, mtime, hash) VALUES(?,?,?,?)`,
			relPath, content, mtime, hash)
		if err != nil {
			return false, err
		}
		docID, _ = res.LastInsertId()
	}

	fmLines := frontmatterLineCount(content)
	paraSrc := content
	if fmLines > 0 {
		// Blank out the frontmatter lines (same line count, so StartLine/
		// EndLine of later paragraphs stay aligned with the original file)
		// so splitParas never fuses frontmatter with the first body block --
		// robust even when there's no blank line after the closing "---".
		paraSrc = blankLines(content, fmLines)
	}
	for _, p := range splitParas(paraSrc) {
		res, err := tx.ExecContext(ctx, `INSERT INTO paragraphs(doc_id, seq, start_line, end_line, text, heading) VALUES(?,?,?,?,?,?)`,
			docID, p.Seq, p.StartLine, p.EndLine, p.Text, p.Heading)
		if err != nil {
			return false, err
		}
		paraID, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO fts(rowid, text) VALUES(?,?)`, paraID, p.Text); err != nil {
			return false, err
		}
		title := p.Heading
		if title == "" {
			title = "none"
		}
		v, err := embed("title: " + title + " | text: " + p.Text)
		if err != nil {
			return false, fmt.Errorf("embed %s#%d: %w", relPath, p.Seq, err)
		}
		blob, err := sqlite_vec.SerializeFloat32(v)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vec(rowid, embedding) VALUES(?,?)`, paraID, blob); err != nil {
			return false, err
		}
	}

	seenTags := map[string]bool{}
	for _, tag := range append(ParseTags(content), autoTags(relPath)...) {
		if seenTags[tag] {
			continue
		}
		seenTags[tag] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO doc_tags(doc_id, tag) VALUES(?,?)`, docID, tag); err != nil {
			return false, err
		}
	}

	if beforeCommit != nil {
		beforeCommit()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ftsQuery turns the user query into an FTS5 MATCH expression: each
// whitespace-separated word becomes a quoted phrase (so operators/quotes
// cannot break the syntax), joined with OR so bm25 ranks paragraphs
// matching more words higher.
func ftsQuery(q string) string {
	words := strings.Fields(q)
	if len(words) == 0 {
		return `""`
	}
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, " OR ")
}

func snippet(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// tagFilter builds an "AND rowid IN (...)" SQL fragment (plus its bound args)
// that restricts fts/vec rowids to paragraphs whose document carries every
// tag in tags (AND semantics; tags are lowercased since ParseTags stores them
// lowercased). Returns ("", nil) when tags is empty, meaning no filtering.
func tagFilter(tags []string) (string, []any) {
	if len(tags) == 0 {
		return "", nil
	}
	frag := `AND rowid IN (SELECT id FROM paragraphs WHERE doc_id IN (
		SELECT doc_id FROM doc_tags WHERE tag IN (` + strings.TrimSuffix(strings.Repeat("?,", len(tags)), ",") + `)
		GROUP BY doc_id HAVING COUNT(DISTINCT tag)=?))`
	args := make([]any, 0, len(tags)+1)
	for _, t := range tags {
		args = append(args, strings.ToLower(t))
	}
	args = append(args, len(tags))
	return frag, args
}

func (s *Store) hitsByParaIDs(ids []int64, scores map[int64]float64) ([]Hit, error) {
	var hits []Hit
	for _, id := range ids {
		var h Hit
		var start, end int
		var text string
		err := s.db.QueryRow(`
			SELECT d.path, p.seq, p.start_line, p.end_line, p.text, p.heading, d.mtime
			FROM paragraphs p JOIN documents d ON d.id = p.doc_id
			WHERE p.id=?`, id).Scan(&h.Doc, &h.Para, &start, &end, &text, &h.Heading, &h.Mtime)
		if err != nil {
			return nil, err
		}
		h.Lines = fmt.Sprintf("%d-%d", start, end)
		h.Score = scores[id]
		h.Snippet = snippet(text)
		hits = append(hits, h)
	}
	return hits, nil
}

// searchTextIDs returns paragraph ids ordered by BM25 rank.
func (s *Store) searchTextIDs(query string, k int, tags []string) ([]int64, error) {
	frag, targs := tagFilter(tags)
	args := append([]any{ftsQuery(query)}, targs...)
	args = append(args, k)
	rows, err := s.db.Query(
		`SELECT rowid FROM fts WHERE fts MATCH ? `+frag+` ORDER BY bm25(fts) LIMIT ?`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) SearchText(query string, k int, tags []string) ([]Hit, error) {
	ids, err := s.searchTextIDs(query, k, tags)
	if err != nil {
		return nil, err
	}
	scores := map[int64]float64{}
	for r, id := range ids {
		scores[id] = 1.0 / float64(r+1)
	}
	return s.hitsByParaIDs(ids, scores)
}

// rrfTextWeight and rrfVecWeight weight the two candidate lists passed to
// rrfMerge (lists[0]=text, lists[1]=vector; see SearchHybrid). Measured
// across multilingual eval sets, vector recall far exceeds trigram-FTS
// recall for natural-language queries, so text-side noise must not outrank
// vector-side confidence. The 0.5/1.0 ratio is a coarse design constant,
// deliberately not tuned finely (overfitting guard).
const (
	rrfTextWeight = 0.5
	rrfVecWeight  = 1.0
)

// rrfMerge combines rankings with weighted reciprocal rank (1/r), weighting
// the text list (lists[0]) and vector list (lists[1]) per rrfTextWeight and
// rrfVecWeight. Returns ids sorted by descending score (ties: ascending id)
// and the score map.
func rrfMerge(lists [][]int64) ([]int64, map[int64]float64) {
	weights := [2]float64{rrfTextWeight, rrfVecWeight}
	scores := map[int64]float64{}
	for i, l := range lists {
		for r, id := range l {
			scores[id] += weights[i] / float64(r+1)
		}
	}
	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids, scores
}

// searchVectorIDs returns paragraph ids ordered by ascending distance.
func (s *Store) searchVectorIDs(qvec []float32, k int, tags []string) ([]int64, map[int64]float64, error) {
	blob, err := sqlite_vec.SerializeFloat32(qvec)
	if err != nil {
		return nil, nil, err
	}
	frag, targs := tagFilter(tags)
	args := append([]any{blob}, targs...)
	args = append(args, k)
	rows, err := s.db.Query(
		`SELECT rowid, distance FROM vec WHERE embedding MATCH ? `+frag+` ORDER BY distance LIMIT ?`,
		args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	dists := map[int64]float64{}
	for rows.Next() {
		var id int64
		var d float64
		if err := rows.Scan(&id, &d); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		dists[id] = d
	}
	return ids, dists, rows.Err()
}

func (s *Store) SearchVector(qvec []float32, k int, tags []string) ([]Hit, error) {
	ids, dists, err := s.searchVectorIDs(qvec, k, tags)
	if err != nil {
		return nil, err
	}
	scores := map[int64]float64{}
	for id, d := range dists {
		scores[id] = 1.0 / (1.0 + d)
	}
	return s.hitsByParaIDs(ids, scores)
}

const rrfFetch = 50 // candidates fetched from each ranking before fusion

func (s *Store) SearchHybrid(query string, qvec []float32, k int, tags []string) ([]Hit, error) {
	textIDs, err := s.searchTextIDs(query, rrfFetch, tags)
	if err != nil {
		return nil, err
	}
	vecIDs, _, err := s.searchVectorIDs(qvec, rrfFetch, tags)
	if err != nil {
		return nil, err
	}
	ids, scores := rrfMerge([][]int64{textIDs, vecIDs})
	if len(ids) > k {
		ids = ids[:k]
	}
	return s.hitsByParaIDs(ids, scores)
}

// ExpandHitBodies fills the leading hits with their indexed paragraph text,
// stopping once the aggregate rune budget is exhausted.
func (s *Store) ExpandHitBodies(hits []Hit, top, budget int) ([]Hit, error) {
	if top > len(hits) {
		top = len(hits)
	}
	used := 0
	for i := 0; i < top; i++ {
		body, err := s.GetParas(hits[i].Doc, hits[i].Para, 0)
		if err != nil {
			return nil, err
		}
		runes := []rune(body)
		remaining := budget - used
		if len(runes) > remaining {
			hits[i].Body = string(runes[:remaining])
			hits[i].BodyTruncated = true
			break
		}
		hits[i].Body = body
		used += len(runes)
	}
	return hits, nil
}

// FirstPath returns the path key of any one indexed document ("" if none).
func (s *Store) FirstPath() (string, error) {
	var p string
	err := s.db.QueryRow(`SELECT path FROM documents LIMIT 1`).Scan(&p)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return p, err
}

// ListPaths returns the stored path of every indexed document.
func (s *Store) ListPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM documents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// DeleteDoc removes a document and its paragraphs/fts/vec rows. ErrNotFound
// if relPath isn't indexed.
func (s *Store) DeleteDoc(relPath string) error {
	var docID int64
	err := s.db.QueryRow(`SELECT id FROM documents WHERE path=?`, relPath).Scan(&docID)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM paragraphs WHERE doc_id=?)`,
		`DELETE FROM vec WHERE rowid IN (SELECT id FROM paragraphs WHERE doc_id=?)`,
		`DELETE FROM paragraphs WHERE doc_id=?`,
		`DELETE FROM doc_tags WHERE doc_id=?`,
	} {
		if _, err := tx.Exec(q, docID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM documents WHERE id=?`, docID); err != nil {
		return err
	}
	return tx.Commit()
}

// DocHash returns the stored content hash for relPath, or "" if the
// document isn't indexed yet.
func (s *Store) DocHash(relPath string) (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT hash FROM documents WHERE path=?`, relPath).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

// TouchDoc refreshes relPath's stored mtime without touching content, hash,
// or paragraphs. Used by index paths that skip re-processing an unchanged
// file (e.g. the document-converter skip, which never calls
// UpsertDocWithHash at all when the source hash matches) but must still
// clear staleness once the on-disk mtime moves.
func (s *Store) TouchDoc(relPath string, mtime int64) error {
	_, err := s.db.Exec(`UPDATE documents SET mtime=? WHERE path=?`, mtime, relPath)
	return err
}

func (s *Store) GetDoc(relPath string) (string, error) {
	var content string
	err := s.db.QueryRow(`SELECT content FROM documents WHERE path=?`, relPath).Scan(&content)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return content, err
}

func (s *Store) ParagraphCount(relPath string) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(p.id) FROM documents d
		LEFT JOIN paragraphs p ON p.doc_id = d.id
		WHERE d.path=? GROUP BY d.id`, relPath).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	return count, err
}

func (s *Store) GetParas(relPath string, seq, context int) (string, error) {
	rows, err := s.db.Query(`
		SELECT p.text FROM paragraphs p JOIN documents d ON d.id = p.doc_id
		WHERE d.path=? AND p.seq BETWEEN ? AND ? ORDER BY p.seq`,
		relPath, seq-context, seq+context)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	found := false
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", err
		}
		parts = append(parts, t)
		found = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	// Verify the requested seq itself exists (context rows alone don't count).
	var n int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM paragraphs p JOIN documents d ON d.id = p.doc_id
		WHERE d.path=? AND p.seq=?`, relPath, seq).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrNotFound
	}
	return strings.Join(parts, "\n\n"), rows.Err()
}
