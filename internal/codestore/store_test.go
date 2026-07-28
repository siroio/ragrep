package codestore

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

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
	docStore, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	docStore.Close()

	_, err = Open(path, "test-model", 768)
	if !errors.Is(err, ErrNotCodeDatabase) {
		t.Fatalf("Open on document DB: err=%v, want ErrNotCodeDatabase", err)
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
