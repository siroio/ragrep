package codestore

import (
	"database/sql"
	"errors"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"github.com/siroio/ragrep/internal/codeindex"
)

// ErrNotFound is returned by GetSymbol when key isn't indexed.
var ErrNotFound = errors.New("symbol not found")

// ErrStaleLiveKey is returned when a live search key no longer names the
// current unsaved version of its file.
var ErrStaleLiveKey = errors.New("stale live file key")

// LiveFile is the current unsaved state of one path.
type LiveFile struct {
	Key        string
	Path       string
	Hash       string
	Body       string
	Generation uint64
	Deleted    bool
}

// FileState is the live overlay state used to reconcile files after save.
type FileState struct {
	Path       string
	Hash       string
	Generation uint64
	Deleted    bool
}

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
	FTSRank     int     `json:"fts_rank,omitempty"` // 1-based rank in the FTS list, 0 if absent
	VecRank     int     `json:"vec_rank,omitempty"` // 1-based rank in the vector list, 0 if absent
	ExactMatch  bool    `json:"exact_match,omitempty"`
	Score       float64 `json:"score"`
	Live        bool    `json:"live,omitempty"`
	Generation  uint64  `json:"generation,omitempty"`
	ContentHash string  `json:"content_hash,omitempty"`
}

// existingSymbolRow is one row already stored for a file, fetched before a
// file-level upsert transaction so UpsertSymbols can diff old vs. new.
type existingSymbolRow struct {
	id                                                  int64
	name, qualifiedName, signature, documentation, body string
	bodyHash, fileHash                                  string
}

type symbolRowsQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
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
	return existingSymbolRows(s.db, path)
}

func existingSymbolRows(q symbolRowsQueryer, path string) (map[string]existingSymbolRow, error) {
	rows, err := q.Query(`
		SELECT key, id, name, qualified_name, signature, documentation, body, body_hash, file_hash
		FROM symbols WHERE path=?`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]existingSymbolRow{}
	for rows.Next() {
		var key string
		var row existingSymbolRow
		if err := rows.Scan(&key, &row.id, &row.name, &row.qualifiedName, &row.signature, &row.documentation, &row.body, &row.bodyHash, &row.fileHash); err != nil {
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
		INSERT INTO symbol_fts(symbol_fts, rowid, name, qualified_name, signature, documentation, body)
		VALUES('delete', ?, ?, ?, ?, ?, ?)`,
		old.id, old.name, old.qualifiedName, old.signature, old.documentation, old.body); err != nil {
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
		INSERT INTO symbol_fts(rowid, name, qualified_name, signature, documentation, body) VALUES (?,?,?,?,?,?)`,
		id, sym.Name, sym.QualifiedName, sym.Signature, sym.Documentation, sym.Body); err != nil {
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
		INSERT INTO symbol_fts(symbol_fts, rowid, name, qualified_name, signature, documentation, body)
		VALUES('delete', ?, ?, ?, ?, ?, ?)`,
		old.id, old.name, old.qualifiedName, old.signature, old.documentation, old.body); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO symbol_fts(rowid, name, qualified_name, signature, documentation, body) VALUES (?,?,?,?,?,?)`,
		old.id, sym.Name, sym.QualifiedName, sym.Signature, sym.Documentation, sym.Body); err != nil {
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

// ListPaths returns the distinct paths of every currently indexed symbol --
// the store-side counterpart to cmd/ragrep's discoverCodeFiles walk, letting
// a caller (e.g. `code index`'s default pruning pass) diff "what's stored"
// against "what this run actually saw on disk".
func (s *Store) ListPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT path FROM symbols`)
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

// HasSymbolsForPath reports whether path has a durable symbol.
func (s *Store) HasSymbolsForPath(path string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM symbols WHERE path=?)`, filepath.ToSlash(path)).Scan(&exists)
	return exists, err
}

// PutLiveFile stores the current unsaved content for path, suppressing its
// durable symbols until the live state is removed after a matching save.
func (s *Store) PutLiveFile(filePath, hash, body string, generation uint64) error {
	return s.putLiveFile(LiveFile{
		Path: filePath, Hash: hash, Body: body, Generation: generation,
	})
}

// PutLiveDeletion stores a tombstone for a deleted unsaved path. Tombstones
// have no FTS row, but still suppress durable symbols for their path.
func (s *Store) PutLiveDeletion(filePath, previousHash string, generation uint64) error {
	return s.putLiveFile(LiveFile{
		Path: filePath, Hash: previousHash, Generation: generation, Deleted: true,
	})
}

func (s *Store) putLiveFile(file LiveFile) error {
	file.Path = filepath.ToSlash(file.Path)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var rowID int64
	var oldPath, oldBody string
	var oldDeleted bool
	err = tx.QueryRow(`SELECT rowid, path, body, deleted FROM live_files WHERE path=?`, file.Path).Scan(&rowID, &oldPath, &oldBody, &oldDeleted)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && !oldDeleted {
		if _, err := tx.Exec(`INSERT INTO live_fts(live_fts, rowid, path, body) VALUES('delete', ?, ?, ?)`, rowID, oldPath, oldBody); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO live_files(path, hash, body, generation, deleted) VALUES(?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET hash=excluded.hash, body=excluded.body, generation=excluded.generation, deleted=excluded.deleted`,
		file.Path, file.Hash, file.Body, file.Generation, file.Deleted); err != nil {
		return err
	}
	if file.Deleted {
		return tx.Commit()
	}
	if err := tx.QueryRow(`SELECT rowid FROM live_files WHERE path=?`, file.Path).Scan(&rowID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO live_fts(rowid, path, body) VALUES(?,?,?)`, rowID, file.Path, file.Body); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveLiveFileIfHash removes a live overlay only if it still describes the
// content identified by expectedHash.
func (s *Store) RemoveLiveFileIfHash(filePath, expectedHash string) (bool, error) {
	filePath = filepath.ToSlash(filePath)
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var rowID int64
	var storedPath, hash, body string
	var deleted bool
	err = tx.QueryRow(`SELECT rowid, path, hash, body, deleted FROM live_files WHERE path=?`, filePath).Scan(&rowID, &storedPath, &hash, &body, &deleted)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if hash != expectedHash || deleted {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO live_fts(live_fts, rowid, path, body) VALUES('delete', ?, ?, ?)`, rowID, storedPath, body); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM live_files WHERE rowid=?`, rowID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// GetLiveFileByPath returns path's live snapshot only when its hash still
// matches expectedHash.
func (s *Store) GetLiveFileByPath(filePath, expectedHash string) (LiveFile, error) {
	var file LiveFile
	err := s.db.QueryRow(`SELECT path, hash, body, generation, deleted FROM live_files WHERE path=?`, filepath.ToSlash(filePath)).Scan(
		&file.Path, &file.Hash, &file.Body, &file.Generation, &file.Deleted)
	if err == sql.ErrNoRows || err == nil && file.Hash != expectedHash {
		return LiveFile{}, ErrStaleLiveKey
	}
	if err != nil {
		return LiveFile{}, err
	}
	if !file.Deleted {
		file.Key = liveKey(file.Generation, file.Hash, file.Path)
	}
	return file, nil
}

// ListFileStates returns the live overlay state for every dirty path.
func (s *Store) ListFileStates() ([]FileState, error) {
	rows, err := s.db.Query(`SELECT path, hash, generation, deleted FROM live_files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFileStates(rows)
}

// ListWorkspaceFileStates returns the visible hash baseline used by a
// workspace refresh. A live row overrides durable symbols for the same path.
func (s *Store) ListWorkspaceFileStates() ([]FileState, error) {
	rows, err := s.db.Query(`
		SELECT path, hash, generation, deleted FROM live_files
		UNION ALL
		SELECT path, MIN(file_hash), 0, false FROM symbols s
		WHERE NOT EXISTS (SELECT 1 FROM live_files lf WHERE lf.path=s.path)
		GROUP BY path
		ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFileStates(rows)
}

func scanFileStates(rows *sql.Rows) ([]FileState, error) {
	var states []FileState
	for rows.Next() {
		var state FileState
		if err := rows.Scan(&state.Path, &state.Hash, &state.Generation, &state.Deleted); err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

// DeleteSymbolsForPath removes every symbol stored for path -- each one's
// symbols row, FTS entry, vec row, and any symbol_edges row it originates
// (see deleteSymbolRow) -- in one transaction. A no-op, not an error, when
// path has no stored symbols. This is `code index`'s pruning primitive: a
// file that's been deleted (or moved out of an indexed root) is no longer
// discovered by a re-index, so its stale symbols would otherwise linger in
// search forever.
func (s *Store) DeleteSymbolsForPath(path string) error {
	existing, err := s.existingSymbolRows(path)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, old := range existing {
		if err := deleteSymbolRow(tx, key, old); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteSymbolsForPathIfLiveHash removes path's durable symbols and deletion
// tombstone only while the tombstone still matches expectedHash.
func (s *Store) DeleteSymbolsForPathIfLiveHash(path, expectedHash string) (bool, error) {
	path = filepath.ToSlash(path)
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var hash string
	var deleted bool
	err = tx.QueryRow(`SELECT hash, deleted FROM live_files WHERE path=?`, path).Scan(&hash, &deleted)
	if err == sql.ErrNoRows || err == nil && (hash != expectedHash || !deleted) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	existing, err := existingSymbolRows(tx, path)
	if err != nil {
		return false, err
	}
	for key, old := range existing {
		if err := deleteSymbolRow(tx, key, old); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM live_files WHERE path=? AND hash=? AND deleted`, path, expectedHash); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
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

// GetVisibleSymbol returns a durable symbol only while its path has no live
// replacement or deletion tombstone.
func (s *Store) GetVisibleSymbol(key string) (codeindex.Symbol, error) {
	sym, err := s.GetSymbol(key)
	if err != nil {
		return codeindex.Symbol{}, err
	}
	var stale bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM live_files WHERE path=?)`, sym.Path).Scan(&stale); err != nil {
		return codeindex.Symbol{}, err
	}
	if stale {
		return codeindex.Symbol{}, ErrStaleLiveKey
	}
	return sym, nil
}

// GetLiveFile returns the current unsaved file for a provisional live key.
func (s *Store) GetLiveFile(key string) (codeindex.Symbol, error) {
	generation, hash, filePath, ok := parseLiveKey(key)
	if !ok {
		return codeindex.Symbol{}, ErrStaleLiveKey
	}

	var file LiveFile
	err := s.db.QueryRow(`SELECT path, hash, body, generation, deleted FROM live_files WHERE path=?`, filePath).Scan(
		&file.Path, &file.Hash, &file.Body, &file.Generation, &file.Deleted)
	if err == sql.ErrNoRows || err == nil && (file.Deleted || file.Generation != generation || file.Hash != hash) {
		return codeindex.Symbol{}, ErrStaleLiveKey
	}
	if err != nil {
		return codeindex.Symbol{}, err
	}
	file.Key = key
	return liveSymbol(file), nil
}

// ReplaceRelations replaces fromKey's edges of exactly the given kinds:
// every existing symbol_edges row matching (from_key=fromKey AND kind IN
// kinds) is deleted first, then every relation in relations is inserted.
// This is scoped by (fromKey, kind), not "replace all of fromKey's edges" —
// a caller that only just queried one relation kind (e.g. `code expand
// --relation callers`) must not wipe out a DIFFERENT kind's edges from an
// earlier call (e.g. previously-saved references/tests) that this call
// simply didn't touch.
//
// kinds must be passed explicitly rather than inferred from relations'
// own Kind values: the caller must still clear a kind's stale edges even
// when this batch produced zero relations of that kind (e.g. a second
// `--relation references` query that now finds nothing must still drop the
// old references/tests edges, not leave them stranded with no way to ever
// be cleared). Every relations[i].Kind is expected to be one of kinds, and
// every relations[i].FromKey is expected to equal fromKey — both are the
// caller's responsibility, not validated here.
//
// The references/tests pairing: cmd/ragrep's `code expand` treats
// "references" and "tests" as one replacement group (both come from a
// single textDocument/references query — see
// codeindex.ReferenceRelations), passing kinds=["references","tests"]
// together regardless of which of the two the user asked for; "callers",
// "callees", and "definition" are each their own one-kind group.
func (s *Store) ReplaceRelations(runID int64, fromKey string, kinds []string, relations []codeindex.Relation) error {
	if len(kinds) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	args := make([]any, 0, len(kinds)+1)
	args = append(args, fromKey)
	for _, k := range kinds {
		args = append(args, k)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	if _, err := tx.Exec(`DELETE FROM symbol_edges WHERE from_key=? AND kind IN (`+placeholders+`)`, args...); err != nil {
		return err
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

// IndexRun is one recorded index_runs row (see RecordIndexRun).
type IndexRun struct {
	ID            int64
	Scope         string
	Revision      string
	Language      string
	ServerName    string
	ServerVersion string
	ModelID       string
	CreatedAt     string
}

// LatestIndexRun returns the most recently recorded index_runs row (highest
// id) whose scope has the "index:" prefix, for a caller that needs the
// index's current identity (revision/server/model -- e.g. `code pack`'s
// manifest header) without tracking a specific run id of its own.
// Non-"index:"-scoped rows (e.g. cmd/ragrep's `code expand`, which records
// its own run scoped "expand:...") are deliberately excluded: without this
// filter, an expand call -- which never (re)indexes any symbol -- would win
// over the last real index run just by having a higher id, letting a
// manifest advertise a revision at which symbols were never actually
// indexed. ErrNotFound when no index-scoped run has been recorded yet.
func (s *Store) LatestIndexRun() (IndexRun, error) {
	var r IndexRun
	err := s.db.QueryRow(`
		SELECT id, scope, revision, language, server_name, server_version, model_id, created_at
		FROM index_runs WHERE scope LIKE 'index:%' ORDER BY id DESC LIMIT 1`).Scan(
		&r.ID, &r.Scope, &r.Revision, &r.Language, &r.ServerName, &r.ServerVersion, &r.ModelID, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return IndexRun{}, ErrNotFound
	}
	if err != nil {
		return IndexRun{}, err
	}
	return r, nil
}

// FindByQualifiedName returns every stored symbol at path whose
// qualified_name exactly matches qualifiedName -- the store-backed
// implementation of coderetrieval.SymbolFinder, used to re-resolve a
// manifest entry whose stable key no longer exists (see
// coderetrieval.ResolveRef).
func (s *Store) FindByQualifiedName(qualifiedName, path string) ([]codeindex.Symbol, error) {
	rows, err := s.db.Query(`
		SELECT key, language, kind, name, qualified_name, signature, documentation, container,
			path, start_line, start_character, end_line, end_character, body, body_hash
		FROM symbols WHERE qualified_name=? AND path=?`, qualifiedName, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []codeindex.Symbol
	for rows.Next() {
		var sym codeindex.Symbol
		if err := rows.Scan(&sym.Key, &sym.Language, &sym.Kind, &sym.Name, &sym.QualifiedName, &sym.Signature, &sym.Documentation, &sym.Container,
			&sym.Path, &sym.Range.Start.Line, &sym.Range.Start.Character, &sym.Range.End.Line, &sym.Range.End.Character,
			&sym.Body, &sym.BodyHash); err != nil {
			return nil, err
		}
		sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
		out = append(out, sym)
	}
	return out, rows.Err()
}

// RelationsFrom returns every stored symbol_edges row originating at
// fromKey (one hop) -- the read counterpart to ReplaceRelations, satisfying
// coderetrieval.RelationGetter when assembling a context pack (`code
// pack`). ToPath/ToPosition are always zero on the returned values:
// symbol_edges only ever persists edges that already resolved to an
// indexed ToKey (see codeindex.Relation's own doc comment on those fields).
func (s *Store) RelationsFrom(fromKey string) ([]codeindex.Relation, error) {
	rows, err := s.db.Query(`SELECT from_key, to_key, kind, source FROM symbol_edges WHERE from_key=?`, fromKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []codeindex.Relation
	for rows.Next() {
		var r codeindex.Relation
		if err := rows.Scan(&r.FromKey, &r.ToKey, &r.Kind, &r.Source); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SymbolFileHash returns the file_hash recorded for key's symbol row -- the
// hash of the file's content as of the index run that last wrote this row,
// the same content the row's Body reflects. Building a
// coderetrieval.SymbolRef from this (rather than re-reading and re-hashing
// the file from disk) keeps a manifest's recorded hash consistent with what
// the store actually served, even if the file has since changed again on
// disk without being reindexed. ErrNotFound when key isn't indexed,
// mirroring GetSymbol.
func (s *Store) SymbolFileHash(key string) (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT file_hash FROM symbols WHERE key=?`, key).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return hash, err
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

// ftsQuery turns the user query into an FTS5 MATCH expression: each
// whitespace-separated word becomes a quoted phrase (so operators/quotes
// cannot break the syntax), joined with OR so bm25 ranks paragraphs
// matching more words higher (mirrors internal/store's ftsQuery).
func ftsQuery(query string) string {
	words := strings.Fields(query)
	if len(words) == 0 {
		return `""`
	}
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, " OR ")
}

// SearchLiveText finds current unsaved file content. Its hits describe the
// whole file because no LSP symbol range exists for this provisional state.
func (s *Store) SearchLiveText(query string, k int) ([]SymbolHit, error) {
	if k <= 0 {
		return nil, nil
	}
	exactPath := filepath.ToSlash(query)
	var exact LiveFile
	err := s.db.QueryRow(`
		SELECT path, hash, body, generation
		FROM live_files WHERE path=? AND NOT deleted`, exactPath).Scan(
		&exact.Path, &exact.Hash, &exact.Body, &exact.Generation)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	var hits []SymbolHit
	if err == nil {
		exact.Key = liveKey(exact.Generation, exact.Hash, exact.Path)
		hit := liveHit(exact)
		hit.ExactMatch = true
		hits = append(hits, hit)
		if len(hits) == k {
			return hits, nil
		}
	}

	rows, err := s.db.Query(`
		SELECT lf.path, lf.hash, lf.body, lf.generation
		FROM live_fts
		JOIN live_files lf ON lf.rowid=live_fts.rowid
		WHERE live_fts MATCH ? AND NOT lf.deleted AND lf.path<>?
		ORDER BY bm25(live_fts) LIMIT ?`, ftsQuery(query), exact.Path, k-len(hits))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var file LiveFile
		if err := rows.Scan(&file.Path, &file.Hash, &file.Body, &file.Generation); err != nil {
			return nil, err
		}
		file.Key = liveKey(file.Generation, file.Hash, file.Path)
		hit := liveHit(file)
		hit.ExactMatch = liveExactMatch(query, file.Path, file.Body)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func liveKey(generation uint64, hash, filePath string) string {
	return "live:" + strconv.FormatUint(generation, 10) + ":" + hash + ":" + filePath
}

func parseLiveKey(key string) (uint64, string, string, bool) {
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 || parts[0] != "live" || parts[2] == "" || parts[3] == "" {
		return 0, "", "", false
	}
	generation, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, "", "", false
	}
	return generation, parts[2], parts[3], true
}

func liveHit(file LiveFile) SymbolHit {
	return SymbolHit{
		Key:         file.Key,
		Kind:        "file",
		Name:        path.Base(file.Path),
		Path:        file.Path,
		EndLine:     strings.Count(file.Body, "\n") + 1,
		Live:        true,
		Generation:  file.Generation,
		ContentHash: file.Hash,
	}
}

func liveSymbol(file LiveFile) codeindex.Symbol {
	sym := codeindex.Symbol{
		Key:           file.Key,
		Kind:          "file",
		Name:          path.Base(file.Path),
		QualifiedName: file.Path,
		Path:          file.Path,
		Range: codeindex.Range{
			End: codeindex.Position{Line: strings.Count(file.Body, "\n") + 1},
		},
		Body:     file.Body,
		BodyHash: file.Hash,
	}
	sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
	return sym
}

func liveExactMatch(query, filePath, body string) bool {
	if query == filePath {
		return true
	}
	if !asciiIdentifier(query) {
		return false
	}
	for _, token := range strings.FieldsFunc(body, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token == query {
			return true
		}
	}
	return false
}

func asciiIdentifier(query string) bool {
	for i, r := range query {
		if r > unicode.MaxASCII || !(r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || i > 0 && '0' <= r && r <= '9') {
			return false
		}
	}
	return query != ""
}

func (s *Store) searchSymbolsTextIDs(query string, k int) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT symbol_fts.rowid FROM symbol_fts
		JOIN symbols s ON s.id=symbol_fts.rowid
		WHERE symbol_fts MATCH ?
			AND NOT EXISTS (SELECT 1 FROM live_files lf WHERE lf.path=s.path)
		ORDER BY bm25(symbol_fts) LIMIT ?`,
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
		SELECT symbol_vec.rowid, distance FROM symbol_vec
		WHERE embedding MATCH ?
			AND rowid IN (
				SELECT s.id FROM symbols s
				WHERE NOT EXISTS (SELECT 1 FROM live_files lf WHERE lf.path=s.path))
		ORDER BY distance LIMIT ?`,
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

// exactMatchIDs returns symbols whose name, qualified name, or path exactly
// matches query (case-sensitive).
func (s *Store) exactMatchIDs(query string) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT id FROM symbols s
		WHERE (name=? OR qualified_name=? OR path=?)
			AND NOT EXISTS (SELECT 1 FROM live_files lf WHERE lf.path=s.path)`, query, query, filepath.ToSlash(query))
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

func prioritizeExactIDs(exactIDs, candidates []int64, scores map[int64]float64, k int) ([]int64, map[int64]bool) {
	exact := make(map[int64]bool, len(exactIDs))
	for _, id := range exactIDs {
		exact[id] = true
	}
	sort.Slice(exactIDs, func(i, j int) bool {
		a, b := exactIDs[i], exactIDs[j]
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		return a < b
	})

	ordered := make([]int64, 0, len(exactIDs)+len(candidates))
	seen := make(map[int64]bool, len(exactIDs))
	for _, id := range exactIDs {
		ordered = append(ordered, id)
		seen[id] = true
	}
	for _, id := range candidates {
		if !seen[id] {
			ordered = append(ordered, id)
		}
	}
	if len(ordered) > k {
		ordered = ordered[:k]
	}
	return ordered, exact
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

// SearchSymbolsTextExact ranks by FTS while pinning and marking exact symbol
// names and qualified names ahead of non-exact hits.
func (s *Store) SearchSymbolsTextExact(query string, k int) ([]SymbolHit, error) {
	exactIDs, err := s.exactMatchIDs(query)
	if err != nil {
		return nil, err
	}
	return s.searchSymbolsTextExact(query, k, exactIDs)
}

func (s *Store) searchSymbolsTextExact(query string, k int, exactIDs []int64) ([]SymbolHit, error) {
	textIDs, err := s.searchSymbolsTextIDs(query, rrfFetch)
	if err != nil {
		return nil, err
	}
	textRanks := rankMap(textIDs)
	scores := make(map[int64]float64, len(textIDs))
	for id, rank := range textRanks {
		scores[id] = 1.0 / float64(rank)
	}
	ordered, exact := prioritizeExactIDs(exactIDs, textIDs, scores, k)
	return s.symbolHitsByIDs(ordered, textRanks, nil, exact, scores)
}

// SearchSymbolsAuto skips vector search for an exact symbol name or qualified
// name match, otherwise using the same hybrid search as semantic queries.
func (s *Store) SearchSymbolsAuto(query string, k int, vector func() ([]float32, error)) ([]SymbolHit, bool, error) {
	exactIDs, err := s.exactMatchIDs(query)
	if err != nil {
		return nil, false, err
	}
	if len(exactIDs) > 0 {
		hits, err := s.searchSymbolsTextExact(query, k, exactIDs)
		return hits, false, err
	}

	v, err := vector()
	if err != nil {
		return nil, false, err
	}
	hits, err := s.SearchSymbolsHybrid(query, v, k)
	return hits, true, err
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
	ordered, exact := prioritizeExactIDs(exactIDs, fusedIDs, scores, k)

	return s.symbolHitsByIDs(ordered, rankMap(textIDs), rankMap(vecIDs), exact, scores)
}
