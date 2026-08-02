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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Migration for DBs created before the heading column existed; fails
	// with "duplicate column" on already-migrated/fresh DBs (ignored), any
	// real breakage surfaces on the next query.
	db.Exec(`ALTER TABLE paragraphs ADD COLUMN heading TEXT NOT NULL DEFAULT ''`)
	return &Store{db: db}, nil
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
	return s.UpsertDocWithHash(relPath, content, mtime, HashContent(content), embed)
}

// UpsertDocWithHash is UpsertDoc with a caller-supplied hash: used by the
// document-converter index path, which hashes the ORIGINAL (pre-conversion)
// file bytes so an unchanged source file skips re-running the converter, not
// just re-embedding.
func (s *Store) UpsertDocWithHash(relPath, content string, mtime int64, hash string, embed EmbedFunc) (bool, error) {
	var docID int64
	var oldHash string
	var oldMtime int64
	err := s.db.QueryRow(`SELECT id, hash, mtime FROM documents WHERE path=?`, relPath).Scan(&docID, &oldHash, &oldMtime)
	if err == nil && oldHash == hash {
		if oldMtime != mtime {
			// Content is unchanged but the on-disk mtime moved (git checkout,
			// touch, re-save): refresh it so markStale doesn't flag this doc
			// forever -- without this, `ragrep index` never sees a "change"
			// to clear the stale flag.
			if _, err := s.db.Exec(`UPDATE documents SET mtime=? WHERE id=?`, mtime, docID); err != nil {
				return false, err
			}
		}
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
			`DELETE FROM doc_tags WHERE doc_id=?`,
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
		res, err := tx.Exec(`INSERT INTO paragraphs(doc_id, seq, start_line, end_line, text, heading) VALUES(?,?,?,?,?,?)`,
			docID, p.Seq, p.StartLine, p.EndLine, p.Text, p.Heading)
		if err != nil {
			return false, err
		}
		paraID, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO fts(rowid, text) VALUES(?,?)`, paraID, p.Text); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO vec(rowid, embedding) VALUES(?,?)`, paraID, blob); err != nil {
			return false, err
		}
	}

	seenTags := map[string]bool{}
	for _, tag := range append(ParseTags(content), autoTags(relPath)...) {
		if seenTags[tag] {
			continue
		}
		seenTags[tag] = true
		if _, err := tx.Exec(`INSERT INTO doc_tags(doc_id, tag) VALUES(?,?)`, docID, tag); err != nil {
			return false, err
		}
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

// rrfMerge combines rankings with Reciprocal Rank Fusion (k=60), weighting
// the text list (lists[0]) and vector list (lists[1]) per rrfTextWeight and
// rrfVecWeight. Returns ids sorted by descending score (ties: ascending id)
// and the score map.
func rrfMerge(lists [][]int64) ([]int64, map[int64]float64) {
	weights := [2]float64{rrfTextWeight, rrfVecWeight}
	scores := map[int64]float64{}
	for i, l := range lists {
		for r, id := range l {
			scores[id] += weights[i] / float64(60+r+1)
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
