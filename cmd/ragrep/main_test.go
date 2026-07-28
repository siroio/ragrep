package main

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

// fakeEmbed returns a fixed-dimension deterministic vector (no ONNX needed).
// Duplicated from internal/store's test helper of the same name: that one is
// unexported to package store and unreachable from here across the package
// boundary, and the value only needs to match store's own embedDim (768).
func fakeEmbed(text string) ([]float32, error) {
	const embedDim = 768
	v := make([]float32, embedDim)
	for i, r := range text {
		v[i%embedDim] += float32(r % 13)
	}
	return v, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// -h/--help must print usage and exit 0, not read as a generic parse error
// (exit 1). Doesn't need the model: parsing fails before the embedder or DB
// are ever touched.
func TestHelpExitsZero(t *testing.T) {
	if code := run([]string{"search", "-h"}); code != 0 {
		t.Fatalf("search -h exit=%d, want 0", code)
	}
}

// getContent's --lines branch: happy path, invalid ranges, out-of-range
// start, clamping past EOF, and CRLF normalization. No model needed.
func TestGetContentLines(t *testing.T) {
	s := newTestStore(t)
	content := "l1\r\nl2\r\nl3\r\nl4\r\nl5"
	if _, err := s.UpsertDoc("a.txt", content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	got, err := getContent(s, "a.txt", "2-3", -1, 0)
	if err != nil || got != "l2\nl3" {
		t.Fatalf("lines 2-3: got %q err=%v", got, err)
	}

	if _, err := getContent(s, "a.txt", "0-3", -1, 0); err == nil {
		t.Fatal("want error for a<1")
	}
	if _, err := getContent(s, "a.txt", "3-2", -1, 0); err == nil {
		t.Fatal("want error for b<a")
	}
	if _, err := getContent(s, "a.txt", "100-200", -1, 0); err != store.ErrNotFound {
		t.Fatalf("want ErrNotFound for a>len(lines), got %v", err)
	}

	got, err = getContent(s, "a.txt", "4-100", -1, 0)
	if err != nil || got != "l4\nl5" {
		t.Fatalf("clamp to EOF: got %q err=%v", got, err)
	}
}

// A panic escaping run() must surface as exit 1 (this CLI's generic error
// code), not Go's own panic exit code 2 -- which would collide with the
// "no hits / not found" contract.
// pruneDecision must only treat a "file gone" stat error as prunable; any
// other stat error (permission denied, transient I/O, AV lock, ...) must
// abort rather than be silently treated as "gone" (which would delete a
// still-valid document).
func TestPruneDecision(t *testing.T) {
	if prune, err := pruneDecision(nil); prune || err != nil {
		t.Fatalf("nil stat err: prune=%v err=%v, want false,nil", prune, err)
	}
	if prune, err := pruneDecision(fs.ErrNotExist); !prune || err != nil {
		t.Fatalf("ErrNotExist: prune=%v err=%v, want true,nil", prune, err)
	}
	other := errors.New("permission denied")
	if prune, err := pruneDecision(other); prune || err != other {
		t.Fatalf("other stat err: prune=%v err=%v, want false,%v", prune, err, other)
	}
}

func TestProtect(t *testing.T) {
	if code := protect(func() int { panic("boom") }); code != 1 {
		t.Fatalf("panic: got %d, want 1", code)
	}
	if code := protect(func() int { return 2 }); code != 2 {
		t.Fatalf("no panic: got %d, want 2", code)
	}
}

// RAGREP_DB overrides the default --db path; an explicit --db still wins.
func TestDBFlagEnvDefault(t *testing.T) {
	t.Setenv("RAGREP_DB", filepath.Join("some", "shared.db"))
	fs := newFlagSet("x")
	db := dbFlag(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *db != filepath.Join("some", "shared.db") {
		t.Fatalf("db=%q, want env default", *db)
	}

	fs2 := newFlagSet("x")
	db2 := dbFlag(fs2)
	if err := fs2.Parse([]string{"--db", "explicit.db"}); err != nil {
		t.Fatal(err)
	}
	if *db2 != "explicit.db" {
		t.Fatalf("db=%q, want explicit.db", *db2)
	}
}
