package codestore

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"github.com/siroio/ragrep/internal/codeindex"
)

// ErrNotFound is returned by GetSymbol when key isn't indexed.
var ErrNotFound = errors.New("symbol not found")

// EmbedFunc mirrors internal/store's EmbedFunc (same shape, separate type
// since the two packages don't import each other) so a caller can point one
// embedder at both stores.
type EmbedFunc func(text string) ([]float32, error)

// SymbolHit is one search result. It deliberately has no Body field: search
// is for locating symbols, not reading their source — call GetSymbol for
// that.
type SymbolHit struct {
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Signature     string `json:"signature"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`

	// Score breakdown.
	FTSRank    int     `json:"fts_rank,omitempty"` // 1-based rank in the FTS list, 0 if absent
	VecRank    int     `json:"vec_rank,omitempty"` // 1-based rank in the vector list, 0 if absent
	ExactMatch bool    `json:"exact_match,omitempty"`
	Score      float64 `json:"score"`
}

// existingSymbolRow is one row already stored for a file, fetched before a
// file-level upsert transaction so UpsertSymbols can diff old vs. new.
type existingSymbolRow struct {
	id                                            int64
	name, qualifiedName, signature, documentation string
	bodyHash, fileHash                            string
}

// UpsertSymbols replaces path's stored symbols with symbols, in one
// transaction.
//
// Fast path: if path already has rows and every one of them has
// file_hash == fileHash, it returns (false, nil) immediately and never
// calls embed — an unchanged file is assumed to have unchanged symbols.
//
// Otherwise it diffs by Symbol.Key: keys no longer present are deleted
// (symbols row, FTS row, vec row, and any symbol_edges row whose from_key
// matches); every symbol in the new list is written, updating the existing
// row in place (preserving id, and therefore symbol_vec's rowid) when its
// key already existed, inserting a fresh row otherwise.
//
// embed is only invoked — recomputing and overwriting that symbol's vector
// — when its stored documentation or body_hash differs from the incoming
// value. Those are the only two fields feeding RenderEmbeddingText that can
// change without the symbol's Key changing too: Key already commits
// language+path+kind+qualified_name+signature, so an update with the same
// Key can only have a different Documentation (doc comment edited without
// touching the declaration) or a different Body/BodyHash. This is the
// re-embed cache rule; unchanged symbols keep their stored vector untouched.
//
// runID is stamped into index_run_id on every inserted or updated symbols
// row (see RecordIndexRun) -- both branches, since an update means this run
// re-produced the row's content just as much as an insert would.
func (s *Store) UpsertSymbols(path, fileHash string, symbols []codeindex.Symbol, runID int64, embed EmbedFunc) (bool, error) {
	existing, err := s.existingSymbolRows(path)
	if err != nil {
		return false, err
	}
	if len(existing) > 0 {
		allSame := true
		for _, row := range existing {
			if row.fileHash != fileHash {
				allSame = false
				break
			}
		}
		if allSame {
			return false, nil
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	incoming := make(map[string]codeindex.Symbol, len(symbols))
	for _, sym := range symbols {
		incoming[sym.Key] = sym
	}

	for key, old := range existing {
		if _, ok := incoming[key]; ok {
			continue
		}
		if err := deleteSymbolRow(tx, key, old); err != nil {
			return false, err
		}
	}

	for _, sym := range symbols {
		old, ok := existing[sym.Key]
		if !ok {
			if err := insertSymbol(tx, fileHash, sym, runID, embed); err != nil {
				return false, err
			}
			continue
		}
		if err := updateSymbol(tx, fileHash, sym, old, runID, embed); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) existingSymbolRows(path string) (map[string]existingSymbolRow, error) {
	rows, err := s.db.Query(`
		SELECT key, id, name, qualified_name, signature, documentation, body_hash, file_hash
		FROM symbols WHERE path=?`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]existingSymbolRow{}
	for rows.Next() {
		var key string
		var row existingSymbolRow
		if err := rows.Scan(&key, &row.id, &row.name, &row.qualifiedName, &row.signature, &row.documentation, &row.bodyHash, &row.fileHash); err != nil {
			return nil, err
		}
		out[key] = row
	}
	return out, rows.Err()
}

// deleteSymbolRow removes everything belonging to a symbol that's no longer
// present in the new symbol list: its FTS entry (via fts5's external-content
// 'delete' command, which needs the old indexed column values to locate and
// remove the right postings), its vec0 row (rowid == symbols.id), any
// symbol_edges row it originates (from_key match — see ReplaceRelations for
// the same convention), and the symbols row itself.
func deleteSymbolRow(tx *sql.Tx, key string, old existingSymbolRow) error {
	if _, err := tx.Exec(`
		INSERT INTO symbol_fts(symbol_fts, rowid, name, qualified_name, signature, documentation)
		VALUES('delete', ?, ?, ?, ?, ?)`,
		old.id, old.name, old.qualifiedName, old.signature, old.documentation); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_vec WHERE rowid=?`, old.id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_edges WHERE from_key=?`, key); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbols WHERE id=?`, old.id); err != nil {
		return err
	}
	return nil
}

func insertSymbol(tx *sql.Tx, fileHash string, sym codeindex.Symbol, runID int64, embed EmbedFunc) error {
	res, err := tx.Exec(`
		INSERT INTO symbols(
			key, language, kind, name, qualified_name, signature, documentation, container,
			path, start_line, start_character, end_line, end_character, body, body_hash,
			file_hash, index_run_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sym.Key, sym.Language, sym.Kind, sym.Name, sym.QualifiedName, sym.Signature, sym.Documentation, sym.Container,
		sym.Path, sym.Range.Start.Line, sym.Range.Start.Character, sym.Range.End.Line, sym.Range.End.Character,
		sym.Body, sym.BodyHash, fileHash, runID)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO symbol_fts(rowid, name, qualified_name, signature, documentation) VALUES (?,?,?,?,?)`,
		id, sym.Name, sym.QualifiedName, sym.Signature, sym.Documentation); err != nil {
		return err
	}
	return embedAndStoreVector(tx, id, sym.EmbeddingText, embed)
}

func updateSymbol(tx *sql.Tx, fileHash string, sym codeindex.Symbol, old existingSymbolRow, runID int64, embed EmbedFunc) error {
	if _, err := tx.Exec(`
		UPDATE symbols SET
			language=?, kind=?, name=?, qualified_name=?, signature=?, documentation=?, container=?,
			path=?, start_line=?, start_character=?, end_line=?, end_character=?, body=?, body_hash=?,
			file_hash=?, index_run_id=?
		WHERE id=?`,
		sym.Language, sym.Kind, sym.Name, sym.QualifiedName, sym.Signature, sym.Documentation, sym.Container,
		sym.Path, sym.Range.Start.Line, sym.Range.Start.Character, sym.Range.End.Line, sym.Range.End.Character,
		sym.Body, sym.BodyHash, fileHash, runID, old.id); err != nil {
		return err
	}

	// Resync FTS unconditionally: 'delete' the old indexed values, then
	// insert the new ones. Cheap at file-transaction scale, and — unlike
	// conditionally resyncing only when name/qualified_name/signature look
	// changed — can never drift out of sync with the symbols row.
	if _, err := tx.Exec(`
		INSERT INTO symbol_fts(symbol_fts, rowid, name, qualified_name, signature, documentation)
		VALUES('delete', ?, ?, ?, ?, ?)`,
		old.id, old.name, old.qualifiedName, old.signature, old.documentation); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO symbol_fts(rowid, name, qualified_name, signature, documentation) VALUES (?,?,?,?,?)`,
		old.id, sym.Name, sym.QualifiedName, sym.Signature, sym.Documentation); err != nil {
		return err
	}

	// Re-embed cache rule: see UpsertSymbols' doc comment.
	if old.documentation == sym.Documentation && old.bodyHash == sym.BodyHash {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM symbol_vec WHERE rowid=?`, old.id); err != nil {
		return err
	}
	return embedAndStoreVector(tx, old.id, sym.EmbeddingText, embed)
}

func embedAndStoreVector(tx *sql.Tx, id int64, embeddingText string, embed EmbedFunc) error {
	v, err := embed(embeddingText)
	if err != nil {
		return err
	}
	blob, err := sqlite_vec.SerializeFloat32(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO symbol_vec(rowid, embedding) VALUES(?,?)`, id, blob)
	return err
}

// GetSymbol returns the full stored record for key, including Body.
// EmbeddingText isn't a stored column; it's recomputed deterministically via
// codeindex.RenderEmbeddingText from the other fields.
func (s *Store) GetSymbol(key string) (codeindex.Symbol, error) {
	var sym codeindex.Symbol
	err := s.db.QueryRow(`
		SELECT key, language, kind, name, qualified_name, signature, documentation, container,
			path, start_line, start_character, end_line, end_character, body, body_hash
		FROM symbols WHERE key=?`, key).Scan(
		&sym.Key, &sym.Language, &sym.Kind, &sym.Name, &sym.QualifiedName, &sym.Signature, &sym.Documentation, &sym.Container,
		&sym.Path, &sym.Range.Start.Line, &sym.Range.Start.Character, &sym.Range.End.Line, &sym.Range.End.Character,
		&sym.Body, &sym.BodyHash)
	if err == sql.ErrNoRows {
		return codeindex.Symbol{}, ErrNotFound
	}
	if err != nil {
		return codeindex.Symbol{}, err
	}
	sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
	return sym, nil
}

// ReplaceRelations replaces the edges originating from every from_key
// mentioned in relations: all existing symbol_edges rows with a matching
// from_key are deleted first, then every relation is inserted. Edges whose
// from_key isn't mentioned in relations are left untouched — this is a
// "replace this key's outgoing edges" operation, not "replace this run's
// edges" or "replace all edges", so a caller that wants to clear a symbol's
// edges entirely must still pass it in relations (with an empty slice that
// still names the key, or by omitting deletion of keys with zero relations
// — callers that stop emitting relations for a from_key must still pass
// that key with zero entries if they want its old edges cleared; simplest
// path is to always resubmit every from_key a run currently knows about).
func (s *Store) ReplaceRelations(runID int64, relations []codeindex.Relation) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	fromKeys := map[string]struct{}{}
	for _, r := range relations {
		fromKeys[r.FromKey] = struct{}{}
	}
	if len(fromKeys) > 0 {
		keys := make([]any, 0, len(fromKeys))
		for k := range fromKeys {
			keys = append(keys, k)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
		if _, err := tx.Exec(`DELETE FROM symbol_edges WHERE from_key IN (`+placeholders+`)`, keys...); err != nil {
			return err
		}
	}

	for _, r := range relations {
		if _, err := tx.Exec(`
			INSERT INTO symbol_edges(from_key, to_key, kind, source, index_run_id) VALUES (?,?,?,?,?)`,
			r.FromKey, r.ToKey, r.Kind, r.Source, runID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RecordIndexRun inserts one index_runs row (see the schema in store.go) and
// returns its id, for callers to pass into UpsertSymbols' runID parameter
// and ReplaceRelations' own runID argument. This is the only write path for
// index_runs -- cmd/ragrep used to insert this row itself via a second raw
// database/sql connection to the same file; that bypassed this package
// entirely and left symbols.index_run_id stamped 0 forever (UpsertSymbols
// had no way to learn the id created here). createdAt is stored in UTC,
// RFC3339 (matching the raw-SQL insert this replaces).
func (s *Store) RecordIndexRun(scope, revision, language, serverName, serverVersion, modelID string, createdAt time.Time) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO index_runs(scope, revision, language, server_name, server_version, model_id, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		scope, revision, language, serverName, serverVersion, modelID, createdAt.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SymbolAt returns the key of the indexed symbol at path whose Range
// contains line (start_line <= line <= end_line) -- the resolution
// primitive codeindex.Resolver needs to turn an LSP location into a stable
// symbol key. When more than one stored symbol's range contains line (an
// outer container and a nested member both covering it), the smallest
// (innermost) range wins, tie-broken by the later start_line -- both push
// the result toward the most specific enclosing declaration rather than a
// loosely-overlapping outer one. ok is false, with no error, when no stored
// symbol at path contains line -- that is not a failure, it just means the
// location isn't (yet) an indexed symbol; the caller must not fabricate a
// key for it (see codeindex.Relation's ToPath/ToPosition fallback).
func (s *Store) SymbolAt(path string, line int) (key string, ok bool, err error) {
	rows, err := s.db.Query(`
		SELECT key, start_line, end_line FROM symbols
		WHERE path=? AND start_line<=? AND end_line>=?`, path, line, line)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	bestSpan, bestStart := 0, 0
	for rows.Next() {
		var k string
		var start, end int
		if err := rows.Scan(&k, &start, &end); err != nil {
			return "", false, err
		}
		span := end - start
		if !ok || span < bestSpan || (span == bestSpan && start > bestStart) {
			key, ok, bestSpan, bestStart = k, true, span, start
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return key, ok, nil
}

// ftsQuery wraps query as a quoted FTS5 phrase so operators/quotes in it
// can't break MATCH syntax (mirrors internal/store's ftsQuery).
func ftsQuery(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

func (s *Store) searchSymbolsTextIDs(query string, k int) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT rowid FROM symbol_fts WHERE symbol_fts MATCH ? ORDER BY bm25(symbol_fts) LIMIT ?`,
		ftsQuery(query), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInt64s(rows)
}

func (s *Store) searchSymbolsVectorIDs(vector []float32, k int) ([]int64, map[int64]float64, error) {
	blob, err := sqlite_vec.SerializeFloat32(vector)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.db.Query(`
		SELECT rowid, distance FROM symbol_vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?`,
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

// exactMatchIDs returns the ids of symbols whose name or qualified_name is
// exactly query (case-sensitive).
func (s *Store) exactMatchIDs(query string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM symbols WHERE name=? OR qualified_name=?`, query, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInt64s(rows)
}

func scanInt64s(rows *sql.Rows) ([]int64, error) {
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

func rankMap(ids []int64) map[int64]int {
	m := make(map[int64]int, len(ids))
	for i, id := range ids {
		m[id] = i + 1 // 1-based
	}
	return m
}

// rrfMerge combines rankings with Reciprocal Rank Fusion (k=60), mirroring
// internal/store's rrfMerge. Returns ids sorted by descending score (ties:
// ascending id) and the score map.
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

// symbolHitsByIDs builds SymbolHits for ids, in the order given. ftsRank and
// vecRank may be nil (meaning "no rank info from that source"); exact marks
// ids that should be reported as exact matches.
func (s *Store) symbolHitsByIDs(ids []int64, ftsRank, vecRank map[int64]int, exact map[int64]bool, scores map[int64]float64) ([]SymbolHit, error) {
	hits := make([]SymbolHit, 0, len(ids))
	for _, id := range ids {
		var h SymbolHit
		err := s.db.QueryRow(`
			SELECT key, kind, name, qualified_name, signature, path, start_line, end_line
			FROM symbols WHERE id=?`, id).Scan(
			&h.Key, &h.Kind, &h.Name, &h.QualifiedName, &h.Signature, &h.Path, &h.StartLine, &h.EndLine)
		if err != nil {
			return nil, err
		}
		h.FTSRank = ftsRank[id]
		h.VecRank = vecRank[id]
		h.ExactMatch = exact[id]
		h.Score = scores[id]
		hits = append(hits, h)
	}
	return hits, nil
}

// SearchSymbolsText ranks by FTS (BM25) alone.
func (s *Store) SearchSymbolsText(query string, k int) ([]SymbolHit, error) {
	ids, err := s.searchSymbolsTextIDs(query, k)
	if err != nil {
		return nil, err
	}
	ftsRank := rankMap(ids)
	scores := make(map[int64]float64, len(ids))
	for id, r := range ftsRank {
		scores[id] = 1.0 / float64(r)
	}
	return s.symbolHitsByIDs(ids, ftsRank, nil, nil, scores)
}

// SearchSymbolsVector ranks by vector distance alone.
func (s *Store) SearchSymbolsVector(vector []float32, k int) ([]SymbolHit, error) {
	ids, dists, err := s.searchSymbolsVectorIDs(vector, k)
	if err != nil {
		return nil, err
	}
	vecRank := rankMap(ids)
	scores := make(map[int64]float64, len(ids))
	for id, d := range dists {
		scores[id] = 1.0 / (1.0 + d)
	}
	return s.symbolHitsByIDs(ids, nil, vecRank, nil, scores)
}

const rrfFetch = 50 // candidates fetched from each ranking before fusion

// SearchSymbolsHybrid fuses FTS and vector rankings with RRF, then applies a
// deterministic pre-RRF-style priority pass: any symbol whose name or
// qualified_name exactly matches query is pinned above every non-exact hit
// (ordered among themselves by fused score, then id), regardless of where
// RRF alone would have placed it. Score still reports the underlying fused
// RRF value (0 for a symbol found only via the exact-match lookup, outside
// both top-rrfFetch lists) — ExactMatch is the flag that explains the pin.
func (s *Store) SearchSymbolsHybrid(query string, vector []float32, k int) ([]SymbolHit, error) {
	textIDs, err := s.searchSymbolsTextIDs(query, rrfFetch)
	if err != nil {
		return nil, err
	}
	vecIDs, _, err := s.searchSymbolsVectorIDs(vector, rrfFetch)
	if err != nil {
		return nil, err
	}
	exactIDs, err := s.exactMatchIDs(query)
	if err != nil {
		return nil, err
	}

	fusedIDs, scores := rrfMerge([][]int64{textIDs, vecIDs})
	exact := make(map[int64]bool, len(exactIDs))
	for _, id := range exactIDs {
		exact[id] = true
	}

	exactOrdered := make([]int64, len(exactIDs))
	copy(exactOrdered, exactIDs)
	sort.Slice(exactOrdered, func(i, j int) bool {
		a, b := exactOrdered[i], exactOrdered[j]
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		return a < b
	})

	ordered := make([]int64, 0, len(exactOrdered)+len(fusedIDs))
	seen := make(map[int64]bool, len(exactOrdered))
	for _, id := range exactOrdered {
		ordered = append(ordered, id)
		seen[id] = true
	}
	for _, id := range fusedIDs {
		if seen[id] {
			continue
		}
		ordered = append(ordered, id)
	}
	if len(ordered) > k {
		ordered = ordered[:k]
	}

	return s.symbolHitsByIDs(ordered, rankMap(textIDs), rankMap(vecIDs), exact, scores)
}
