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

	for _, name := range []string{"sample.go", "sample_test.go"} {
		src, err := os.ReadFile(filepath.Join("testdata", "code", "go", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
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

	// callers: SayHello calls Greet, so expanding Greet's callers must
	// resolve to SayHello.
	var callersCode int
	callersOut := captureStdout(t, func() {
		callersCode = run([]string{"code", "expand", "--db", db, "--symbol", greetKey, "--relation", "callers", "--json"})
	})
	if callersCode != 0 {
		t.Fatalf("code expand --relation callers: exit=%d, output=%q", callersCode, callersOut)
	}
	callerTargets := decodeExpandTargets(t, callersOut)
	if !anyExpandTargetMatches(callerTargets, func(tg codeExpandTarget) bool {
		return tg.Resolved && strings.Contains(tg.QualifiedName, "SayHello")
	}) {
		t.Fatalf("code expand --relation callers output=%v, want a resolved target for SayHello", callerTargets)
	}

	// references: SayHello calls Greet TWICE (see sample.go) -- both calls
	// resolve to the same enclosing symbol (SayHello), so this must not
	// crash with a symbol_edges UNIQUE constraint violation (the duplicate
	// resolved relations must be deduped before being persisted -- see
	// codeindex.DedupResolvedRelations) and must still succeed end-to-end.
	var referencesCode int
	referencesOut := captureStdout(t, func() {
		referencesCode = run([]string{"code", "expand", "--db", db, "--symbol", greetKey, "--relation", "references", "--json"})
	})
	if referencesCode != 0 {
		t.Fatalf("code expand --relation references: exit=%d, output=%q", referencesCode, referencesOut)
	}
	referencesTargets := decodeExpandTargets(t, referencesOut)
	if !anyExpandTargetMatches(referencesTargets, func(tg codeExpandTarget) bool {
		return tg.Resolved && strings.Contains(tg.QualifiedName, "SayHello")
	}) {
		t.Fatalf("code expand --relation references output=%v, want a resolved target for SayHello", referencesTargets)
	}

	// tests: TestGreet (in sample_test.go) references Greet, so that
	// reference must be classified "tests", not "references" -- and expand
	// with --relation tests must return it.
	var testsCode int
	testsOut := captureStdout(t, func() {
		testsCode = run([]string{"code", "expand", "--db", db, "--symbol", greetKey, "--relation", "tests", "--json"})
	})
	if testsCode != 0 {
		t.Fatalf("code expand --relation tests: exit=%d, output=%q", testsCode, testsOut)
	}
	testTargets := decodeExpandTargets(t, testsOut)
	if !anyExpandTargetMatches(testTargets, func(tg codeExpandTarget) bool {
		return strings.HasSuffix(tg.Path, "sample_test.go")
	}) {
		t.Fatalf("code expand --relation tests output=%v, want a target in sample_test.go", testTargets)
	}

	// definition: querying Greet's own declaration position must resolve
	// back to an indexed symbol (never a fabricated key).
	var defCode int
	defOut := captureStdout(t, func() {
		defCode = run([]string{"code", "expand", "--db", db, "--symbol", greetKey, "--relation", "definition", "--json"})
	})
	if defCode != 0 {
		t.Fatalf("code expand --relation definition: exit=%d, output=%q", defCode, defOut)
	}
	defTargets := decodeExpandTargets(t, defOut)
	if len(defTargets) == 0 {
		t.Fatalf("code expand --relation definition returned no targets")
	}
}

func decodeExpandTargets(t *testing.T, jsonOut string) []codeExpandTarget {
	t.Helper()
	var targets []codeExpandTarget
	if err := json.Unmarshal([]byte(jsonOut), &targets); err != nil {
		t.Fatalf("code expand --json output not valid JSON: %v (%q)", err, jsonOut)
	}
	return targets
}

func anyExpandTargetMatches(targets []codeExpandTarget, pred func(codeExpandTarget) bool) bool {
	for _, tg := range targets {
		if pred(tg) {
			return true
		}
	}
	return false
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
