//go:build gopls

// This file is a real-fixture integration test against an actual gopls
// binary. It's excluded from the default `go test ./...` run (unit tests
// must not depend on gopls being installed) -- run it explicitly with:
//
//	go test -tags gopls ./cmd/ragrep -run TestGoplsIntegration -v
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/codestore"
)

// captureStdout runs f with os.Stdout redirected to a pipe and returns
// whatever f wrote to it.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String()
}

// TestGoplsIntegration drives `ragrep code index` -> `code search` ->
// `code get` end-to-end against a real gopls, using the fixture at
// testdata/code/go/sample.go (a Greeter struct with a Greet method and a
// NewGreeter constructor).
func TestGoplsIntegration(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH; skipping real-fixture integration test")
	}

	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	src, err := os.ReadFile(filepath.Join("testdata", "code", "go", "sample.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module sample\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), []byte(`{"servers": {"go": "gopls"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(ragrepDir, "code.db")

	var indexCode int
	indexOut := captureStdout(t, func() {
		indexCode = run([]string{"code", "index", "--db", db, "--language", "go", root})
	})
	if indexCode != 0 {
		t.Fatalf("code index: exit=%d, output=%q", indexCode, indexOut)
	}
	if !strings.Contains(indexOut, "indexed sample.go") {
		t.Fatalf("code index output=%q, want it to report sample.go indexed", indexOut)
	}

	// index_runs must have recorded this run (revision/server/model/time) --
	// see recordIndexRun in code.go.
	assertIndexRunRecorded(t, db)

	var searchCode int
	searchOut := captureStdout(t, func() {
		searchCode = run([]string{"code", "search", "--db", db, "--json", "-k", "5", "Greet"})
	})
	if searchCode != 0 {
		t.Fatalf("code search: exit=%d, output=%q", searchCode, searchOut)
	}
	var hits []codestore.SymbolHit
	if err := json.Unmarshal([]byte(searchOut), &hits); err != nil {
		t.Fatalf("code search --json output not valid JSON: %v (%q)", err, searchOut)
	}
	if len(hits) == 0 {
		t.Fatalf("code search for %q returned no hits", "Greet")
	}
	// gopls reports a Go method as a flat, top-level documentSymbol whose
	// Name already carries the receiver (e.g. "(Greeter).Greet") rather than
	// nesting it under the struct's children -- so match on Kind+substring,
	// not an exact Name.
	var greetKey string
	for _, h := range hits {
		if h.Kind == "method" && strings.Contains(h.Name, "Greet") {
			greetKey = h.Key
		}
	}
	if greetKey == "" {
		t.Fatalf("code search hits=%v, want a method hit for Greet", hits)
	}

	var getCode int
	getOut := captureStdout(t, func() {
		getCode = run([]string{"code", "get", "--db", db, "--symbol", greetKey, "--body"})
	})
	if getCode != 0 {
		t.Fatalf("code get: exit=%d, output=%q", getCode, getOut)
	}
	if !strings.Contains(getOut, "hello, ") {
		t.Fatalf("code get --body output=%q, want the Greet method body", getOut)
	}
}

func assertIndexRunRecorded(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM index_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("index_runs has no rows after `code index`")
	}
}
