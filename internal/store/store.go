package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// ErrNotFound is returned when a requested document or paragraph doesn't exist.
var ErrNotFound = errors.New("not found")

const embedDim = 768

type EmbedFunc func(text string) ([]float32, error)

type Hit struct {
	Doc     string  `json:"doc"`
	Para    int     `json:"para"`
	Lines   string  `json:"lines"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

type Store struct{ db *sql.DB }

const schema = `
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
  text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_para_doc ON paragraphs(doc_id, seq);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(text, tokenize='trigram');
CREATE VIRTUAL TABLE IF NOT EXISTS vec USING vec0(embedding float[768]);
`

// Open opens (creating if needed) the SQLite index at path and ensures schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) UpsertDoc(relPath, content string, mtime int64, embed EmbedFunc) (bool, error) {
	h := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(h[:])

	var docID int64
	var oldHash string
	err := s.db.QueryRow(`SELECT id, hash FROM documents WHERE path=?`, relPath).Scan(&docID, &oldHash)
	if err == nil && oldHash == hash {
		return false, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	exists := err == nil

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if exists { // existing doc: purge old paragraphs from all indexes
		for _, q := range []string{
			`DELETE FROM fts WHERE rowid IN (SELECT id FROM paragraphs WHERE doc_id=?)`,
			`DELETE FROM vec WHERE rowid IN (SELECT id FROM paragraphs WHERE doc_id=?)`,
			`DELETE FROM paragraphs WHERE doc_id=?`,
		} {
			if _, err := tx.Exec(q, docID); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(`UPDATE documents SET content=?, mtime=?, hash=? WHERE id=?`,
			content, mtime, hash, docID); err != nil {
			return false, err
		}
	} else {
		res, err := tx.Exec(`INSERT INTO documents(path, content, mtime, hash) VALUES(?,?,?,?)`,
			relPath, content, mtime, hash)
		if err != nil {
			return false, err
		}
		docID, _ = res.LastInsertId()
	}

	for _, p := range splitParas(content) {
		res, err := tx.Exec(`INSERT INTO paragraphs(doc_id, seq, start_line, end_line, text) VALUES(?,?,?,?,?)`,
			docID, p.Seq, p.StartLine, p.EndLine, p.Text)
		if err != nil {
			return false, err
		}
		paraID, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO fts(rowid, text) VALUES(?,?)`, paraID, p.Text); err != nil {
			return false, err
		}
		v, err := embed("title: none | text: " + p.Text)
		if err != nil {
			return false, fmt.Errorf("embed %s#%d: %w", relPath, p.Seq, err)
		}
		blob, err := sqlite_vec.SerializeFloat32(v)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(`INSERT INTO vec(rowid, embedding) VALUES(?,?)`, paraID, blob); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// ftsQuery wraps the user query as a quoted FTS5 phrase so that
// operators/quotes in the query cannot break the MATCH syntax.
func ftsQuery(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func snippet(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

func (s *Store) hitsByParaIDs(ids []int64, scores map[int64]float64) ([]Hit, error) {
	var hits []Hit
	for _, id := range ids {
		var h Hit
		var start, end int
		var text string
		err := s.db.QueryRow(`
			SELECT d.path, p.seq, p.start_line, p.end_line, p.text
			FROM paragraphs p JOIN documents d ON d.id = p.doc_id
			WHERE p.id=?`, id).Scan(&h.Doc, &h.Para, &start, &end, &text)
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
func (s *Store) searchTextIDs(query string, k int) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT rowid FROM fts WHERE fts MATCH ? ORDER BY bm25(fts) LIMIT ?`,
		ftsQuery(query), k)
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

func (s *Store) SearchText(query string, k int) ([]Hit, error) {
	ids, err := s.searchTextIDs(query, k)
	if err != nil {
		return nil, err
	}
	scores := map[int64]float64{}
	for r, id := range ids {
		scores[id] = 1.0 / float64(r+1)
	}
	return s.hitsByParaIDs(ids, scores)
}

// rrfMerge combines rankings with Reciprocal Rank Fusion (k=60).
// Returns ids sorted by descending score (ties: ascending id) and the score map.
func rrfMerge(lists [][]int64) ([]int64, map[int64]float64) {
	scores := map[int64]float64{}
	for _, l := range lists {
		for r, id := range l {
			scores[id] += 1.0 / float64(60+r+1)
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
func (s *Store) searchVectorIDs(qvec []float32, k int) ([]int64, map[int64]float64, error) {
	blob, err := sqlite_vec.SerializeFloat32(qvec)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.db.Query(
		`SELECT rowid, distance FROM vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?`,
		blob, k)
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

func (s *Store) SearchVector(qvec []float32, k int) ([]Hit, error) {
	ids, dists, err := s.searchVectorIDs(qvec, k)
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

func (s *Store) SearchHybrid(query string, qvec []float32, k int) ([]Hit, error) {
	textIDs, err := s.searchTextIDs(query, rrfFetch)
	if err != nil {
		return nil, err
	}
	vecIDs, _, err := s.searchVectorIDs(qvec, rrfFetch)
	if err != nil {
		return nil, err
	}
	ids, scores := rrfMerge([][]int64{textIDs, vecIDs})
	if len(ids) > k {
		ids = ids[:k]
	}
	return s.hitsByParaIDs(ids, scores)
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

func (s *Store) GetDoc(relPath string) (string, error) {
	var content string
	err := s.db.QueryRow(`SELECT content FROM documents WHERE path=?`, relPath).Scan(&content)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return content, err
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
