// Package codestore is the SQLite-backed store for indexed code symbols
// (code.db), kept separate from the document index (internal/store). It has
// no migration mechanism: a schema-version or embedding-model mismatch means
// the caller must delete the file and re-index.
package codestore

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// schemaVersion is stamped into PRAGMA user_version on creation. There is no
// migration path across versions: bump this and require re-index instead.
const schemaVersion = 1

// ErrReindexRequired is returned by Open when an existing code.db's schema
// version or embedding model/dimension no longer matches what the caller
// expects. Recovery is always "delete the file and re-index" — there is no
// migration.
var ErrReindexRequired = errors.New("code database out of date, delete and re-index")

// ErrNotCodeDatabase is returned by Open when path already contains tables
// but none of them are this package's schema (for example, the caller
// pointed it at the document index.db by mistake).
var ErrNotCodeDatabase = errors.New("not a code-symbol database")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE code_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE symbols (
    id INTEGER PRIMARY KEY,
    key TEXT NOT NULL UNIQUE,
    language TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL,
    signature TEXT NOT NULL,
    documentation TEXT NOT NULL,
    container TEXT NOT NULL,
    path TEXT NOT NULL,
    start_line INTEGER NOT NULL,
    start_character INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    end_character INTEGER NOT NULL,
    body TEXT NOT NULL,
    body_hash TEXT NOT NULL,
    file_hash TEXT NOT NULL,
    index_run_id INTEGER NOT NULL
);

CREATE VIRTUAL TABLE symbol_fts USING fts5(
    name,
    qualified_name,
    signature,
    documentation,
    content='symbols',
    content_rowid='id',
    tokenize='trigram'
);

CREATE VIRTUAL TABLE symbol_vec USING vec0(
    embedding float[768]
);

CREATE TABLE symbol_edges (
    from_key TEXT NOT NULL,
    to_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    source TEXT NOT NULL,
    index_run_id INTEGER NOT NULL,
    UNIQUE(from_key, to_key, kind, source)
);

CREATE TABLE index_runs (
    id INTEGER PRIMARY KEY,
    scope TEXT NOT NULL,
    revision TEXT NOT NULL,
    language TEXT NOT NULL,
    server_name TEXT NOT NULL,
    server_version TEXT NOT NULL,
    model_id TEXT NOT NULL,
    created_at TEXT NOT NULL
);
`

// Open opens (creating if needed) the code-symbol SQLite database at path.
//
// On a fresh file it creates the schema above, stamps PRAGMA user_version
// with schemaVersion, and records modelID/embeddingDim into code_meta. On an
// existing file it validates user_version and the recorded model/dimension
// against what the caller passed in, returning an error wrapping
// ErrReindexRequired on any mismatch. If path already holds tables that
// aren't this schema (e.g. it's the document index.db), it returns an error
// wrapping ErrNotCodeDatabase.
func Open(path, modelID string, embeddingDim int) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}

	userVersion, err := readUserVersion(db)
	if err != nil {
		db.Close()
		return nil, err
	}

	if userVersion == 0 {
		empty, err := isEmpty(db)
		if err != nil {
			db.Close()
			return nil, err
		}
		if !empty {
			db.Close()
			return nil, fmt.Errorf("%w: %s already has tables but no code_meta/user_version stamp", ErrNotCodeDatabase, path)
		}
		if err := createSchema(db, modelID, embeddingDim); err != nil {
			db.Close()
			return nil, err
		}
		return &Store{db: db}, nil
	}

	if userVersion != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("%w: schema version %d, want %d", ErrReindexRequired, userVersion, schemaVersion)
	}

	if err := validateMeta(db, modelID, embeddingDim); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func readUserVersion(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

func isEmpty(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&n)
	return n == 0, err
}

func createSchema(db *sql.DB, modelID string, embeddingDim int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO code_meta(key, value) VALUES ('model_id', ?), ('embedding_dim', ?)`,
		modelID, strconv.Itoa(embeddingDim)); err != nil {
		return err
	}
	// PRAGMA doesn't accept bound parameters; schemaVersion is a package
	// constant, not user input, so formatting it in is safe.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func validateMeta(db *sql.DB, modelID string, embeddingDim int) error {
	rows, err := db.Query(`SELECT key, value FROM code_meta`)
	if err != nil {
		return err
	}
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		meta[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	wantDim := strconv.Itoa(embeddingDim)
	if meta["model_id"] != modelID || meta["embedding_dim"] != wantDim {
		return fmt.Errorf("%w: db has model_id=%q embedding_dim=%q, want model_id=%q embedding_dim=%q",
			ErrReindexRequired, meta["model_id"], meta["embedding_dim"], modelID, wantDim)
	}
	return nil
}
