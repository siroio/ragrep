package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesAndReopensDocumentMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened.Close()
}

func TestOpenRejectsMetadataMismatchWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE ragrep_meta SET value='foreign-model' WHERE key='embedding_identity'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("err=%v, want ErrReindexRequired", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("mismatch open mutated database")
	}
}

func TestOpenRejectsEmbeddingDimensionMismatchWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE ragrep_meta SET value='512' WHERE key='embedding_dim'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(path)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("err=%v, want ErrReindexRequired", err)
	}
}

func TestOpenRejectsSchemaVersionMismatchWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("err=%v, want ErrReindexRequired", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("schema mismatch open mutated database")
	}
}

func TestOpenRejectsMatchingMetadataWithMissingRequiredTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE paragraphs`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("err=%v, want ErrReindexRequired", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("schema mismatch open mutated database")
	}
}

func TestOpenRejectsMatchingMetadataWithWrongVectorDimension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE vec; CREATE VIRTUAL TABLE vec USING vec0(embedding float[3])`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("err=%v, want ErrReindexRequired", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("schema mismatch open mutated database")
	}
}

func TestOpenRejectsLegacyDatabaseWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", storeDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE documents(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	_, err = Open(path)
	if !errors.Is(err, ErrReindexRequired) {
		t.Fatalf("err=%v, want ErrReindexRequired", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("legacy open mutated database")
	}
}

func TestOpenLeavesCanceledReindexTransactionUnchanged(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("keep.md", "old", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := s.upsertDocWithHashContext(ctx, "keep.md", "new", 2, HashContent("new"), func(text string) ([]float32, error) {
		v := make([]float32, embedDim)
		cancel()
		return v, nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	content, err := s.GetDoc("keep.md")
	if err != nil || content != "old" {
		t.Fatalf("content=%q err=%v, want old and nil", content, err)
	}
}

func TestOpenRejectsForeignDatabaseWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	db, err := sql.Open("sqlite3", storeDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(path)
	if !errors.Is(err, ErrNotDocumentDatabase) {
		t.Fatalf("err=%v, want ErrNotDocumentDatabase", err)
	}
}

func TestOpenRejectsForeignVersionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	db, err := sql.Open("sqlite3", storeDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE code_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL); PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(path)
	if !errors.Is(err, ErrNotDocumentDatabase) {
		t.Fatalf("err=%v, want ErrNotDocumentDatabase", err)
	}
}

func TestCheckValidatesWithoutCreatingOrMutating(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if err := Check(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Check(missing)=%v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Check created missing database: %v", err)
	}

	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(path); err != nil {
		t.Fatalf("Check(valid)=%v, want nil", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("Check mutated database")
	}
}
