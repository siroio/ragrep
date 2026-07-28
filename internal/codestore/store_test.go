package codestore

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

// encodeFloat32 packs v as vec0's float32 blob format: contiguous
// little-endian float32 bytes, no header. Hand-rolled (rather than importing
// sqlite-vec-go-bindings' SerializeFloat32) so this test package only
// depends on codestore's own production imports — importing the vec
// bindings package here would silently re-register its WASM binary via this
// test file and mask a missing blank import in store.go itself.
func encodeFloat32(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func TestOpenFreshCreatesSchemaAndStampsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "test-model", 768)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var userVersion int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if userVersion != schemaVersion {
		t.Fatalf("user_version = %d, want %d", userVersion, schemaVersion)
	}

	rows, err := s.db.Query(`SELECT key, value FROM code_meta`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	rows.Close()
	if got["model_id"] != "test-model" || got["embedding_dim"] != "768" {
		t.Fatalf("code_meta = %v, want model_id=test-model embedding_dim=768", got)
	}

	// Symbol tables must exist and be queryable.
	for _, table := range []string{"symbols", "symbol_fts", "symbol_vec", "symbol_edges", "index_runs"} {
		if _, err := s.db.Exec(`SELECT * FROM ` + table + ` LIMIT 0`); err != nil {
			t.Fatalf("table %s missing/broken: %v", table, err)
		}
	}
}

func TestOpenReopenSameModelSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "test-model", 768)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, "test-model", 768)
	if err != nil {
		t.Fatalf("reopen with same model/dim should succeed: %v", err)
	}
	s2.Close()
}

func TestOpenVersionMismatchRequiresReindex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "test-model", 768)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	bumpUserVersion(t, path, 999)

	_, err = Open(path, "test-model", 768)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("Open after version bump: err=%v, want ErrReindexRequired", err)
	}
}

func TestOpenModelMismatchRequiresReindex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "model-a", 768)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(path, "model-b", 768)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("Open with different model_id: err=%v, want ErrReindexRequired", err)
	}
}

func TestOpenDimMismatchRequiresReindex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "model-a", 768)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(path, "model-a", 512)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("Open with different embedding_dim: err=%v, want ErrReindexRequired", err)
	}
}

func TestOpenRejectsDocumentDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	// Minimal shape of internal/store's schema (see internal/store/store.go):
	// a "documents" table, no code_meta, user_version left at SQLite's
	// default of 0 (internal/store never touches that pragma). This is
	// hand-rolled rather than calling internal/store.Open so this test
	// package stays independent of internal/store's imports.
	if _, err := db.Exec(`CREATE TABLE documents(id INTEGER PRIMARY KEY, path TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path, "test-model", 768)
	if !errors.Is(err, ErrNotCodeDatabase) {
		t.Fatalf("Open on document DB: err=%v, want ErrNotCodeDatabase", err)
	}
}

func TestOpenRejectsNonPositiveEmbeddingDim(t *testing.T) {
	for _, dim := range []int{0, -1} {
		path := filepath.Join(t.TempDir(), "code.db")
		if _, err := Open(path, "test-model", dim); err == nil {
			t.Fatalf("Open with embeddingDim=%d should fail", dim)
		}
	}
}

// TestOpenParameterizesVectorDimension guards against symbol_vec's dimension
// being hardcoded (it used to always be float[768] regardless of the
// embeddingDim argument): open with dim=3, then confirm vec0 itself enforces
// that width rather than the schema's original 768.
func TestOpenParameterizesVectorDimension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.db")
	s, err := Open(path, "test-model", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.db.Exec(`INSERT INTO symbol_vec(rowid, embedding) VALUES(1, ?)`, encodeFloat32([]float32{1, 2, 3})); err != nil {
		t.Fatalf("insert with matching dim 3 should succeed: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO symbol_vec(rowid, embedding) VALUES(2, ?)`, encodeFloat32([]float32{1, 2, 3, 4})); err == nil {
		t.Fatal("insert with mismatched dim 4 should fail when table dim is 3")
	}
}

// bumpUserVersion opens path directly (bypassing codestore) and sets
// PRAGMA user_version, simulating a stale/foreign schema stamp.
func bumpUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatal(err)
	}
}
