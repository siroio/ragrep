package coderetrieval

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

func TestManifest_JSONRoundTrip(t *testing.T) {
	m := Manifest{
		IndexRevision: "deadbeef",
		ServerName:    "gopls",
		ServerVersion: "v0.16.0",
		ModelID:       "embeddinggemma-300m",
		Symbols: []SymbolRef{
			{Key: "k1", QualifiedName: "Foo.Bar", Path: "foo.go", StartLine: 1, EndLine: 5, FileHash: "hash1"},
		},
	}

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(b)), `"body"`) {
		t.Fatalf("manifest JSON contains a body field, want symbol references only: %s", b)
	}

	var out Manifest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if out.IndexRevision != m.IndexRevision || out.ServerName != m.ServerName ||
		out.ServerVersion != m.ServerVersion || out.ModelID != m.ModelID {
		t.Fatalf("round trip mismatch: got %+v, want %+v", out, m)
	}
	if len(out.Symbols) != 1 || out.Symbols[0] != m.Symbols[0] {
		t.Fatalf("round trip Symbols mismatch: got %+v, want %+v", out.Symbols, m.Symbols)
	}
}

func TestCheckStale(t *testing.T) {
	unchanged := []byte("package x\nfunc A() {}\n")
	changed := []byte("package y\nfunc B() { /* edited */ }\n")

	m := Manifest{
		Symbols: []SymbolRef{
			{Key: "a", Path: "unchanged.go", FileHash: codeindex.FileHash(unchanged)},
			{Key: "b", Path: "changed.go", FileHash: codeindex.FileHash([]byte("old content"))},
		},
	}

	content := map[string][]byte{
		"unchanged.go": unchanged,
		"changed.go":   changed,
	}
	readFile := func(path string) ([]byte, error) { return content[path], nil }

	report := CheckStale(m, readFile)
	if !report.Stale {
		t.Fatalf("report.Stale = false, want true (changed.go differs)")
	}
	if len(report.Entries) != 2 {
		t.Fatalf("len(report.Entries) = %d, want 2", len(report.Entries))
	}
	for _, e := range report.Entries {
		switch e.Key {
		case "a":
			if e.Stale {
				t.Fatalf("entry %q: Stale = true, want false", e.Key)
			}
		case "b":
			if !e.Stale {
				t.Fatalf("entry %q: Stale = false, want true", e.Key)
			}
		}
	}
}

func TestCheckStale_NothingChanged(t *testing.T) {
	unchanged := []byte("package x\n")
	m := Manifest{Symbols: []SymbolRef{{Key: "a", Path: "x.go", FileHash: codeindex.FileHash(unchanged)}}}
	readFile := func(path string) ([]byte, error) { return unchanged, nil }

	report := CheckStale(m, readFile)
	if report.Stale {
		t.Fatalf("report.Stale = true, want false")
	}
}

func TestCheckStale_MissingFileMarksStaleAndContinues(t *testing.T) {
	present := []byte("package x\n")
	m := Manifest{
		Symbols: []SymbolRef{
			{Key: "a", Path: "deleted.go", FileHash: "irrelevant"},
			{Key: "b", Path: "present.go", FileHash: codeindex.FileHash(present)},
		},
	}
	readFile := func(path string) ([]byte, error) {
		if path == "deleted.go" {
			return nil, os.ErrNotExist
		}
		return present, nil
	}

	report := CheckStale(m, readFile)
	if !report.Stale {
		t.Fatalf("report.Stale = false, want true (deleted.go is unreadable)")
	}
	if len(report.Entries) != 2 {
		t.Fatalf("len(report.Entries) = %d, want 2 -- a missing file must not abort the rest of the report", len(report.Entries))
	}
	for _, e := range report.Entries {
		switch e.Key {
		case "a":
			if !e.Stale {
				t.Fatalf("entry %q (deleted file): Stale = false, want true", e.Key)
			}
		case "b":
			if e.Stale {
				t.Fatalf("entry %q (present, unchanged file): Stale = true, want false", e.Key)
			}
		}
	}
}

func TestResolveRef_KeyStillValid(t *testing.T) {
	ref := SymbolRef{Key: "a", QualifiedName: "A", Path: "x.go", StartLine: 1, EndLine: 5, FileHash: "h"}
	getSymbol := func(k string) (codeindex.Symbol, error) { return codeindex.Symbol{Key: k}, nil }
	findSymbol := func(qn, path string) ([]codeindex.Symbol, error) {
		t.Fatalf("findSymbol should not be called when the key still resolves")
		return nil, nil
	}

	got, err := ResolveRef(ref, getSymbol, findSymbol)
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != ref {
		t.Fatalf("got %+v, want unchanged %+v", got, ref)
	}
}

func TestResolveRef_UniqueMatchResolves(t *testing.T) {
	ref := SymbolRef{Key: "old-key", QualifiedName: "A", Path: "x.go", StartLine: 1, EndLine: 5, FileHash: "h"}
	getSymbol := func(k string) (codeindex.Symbol, error) { return codeindex.Symbol{}, codestore.ErrNotFound }
	findSymbol := func(qn, path string) ([]codeindex.Symbol, error) {
		if qn != "A" || path != "x.go" {
			t.Fatalf("findSymbol called with (%q, %q), want (A, x.go)", qn, path)
		}
		return []codeindex.Symbol{{
			Key: "new-key", QualifiedName: "A", Path: "x.go",
			Range: codeindex.Range{Start: codeindex.Position{Line: 2}, End: codeindex.Position{Line: 6}},
		}}, nil
	}

	got, err := ResolveRef(ref, getSymbol, findSymbol)
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.Key != "new-key" {
		t.Fatalf("got.Key = %q, want %q", got.Key, "new-key")
	}
	if got.StartLine != 2 || got.EndLine != 6 {
		t.Fatalf("got range = [%d,%d], want [2,6]", got.StartLine, got.EndLine)
	}
}

func TestResolveRef_ZeroMatchesHalts(t *testing.T) {
	ref := SymbolRef{Key: "old-key", QualifiedName: "A", Path: "x.go"}
	getSymbol := func(k string) (codeindex.Symbol, error) { return codeindex.Symbol{}, codestore.ErrNotFound }
	findSymbol := func(qn, path string) ([]codeindex.Symbol, error) { return nil, nil }

	_, err := ResolveRef(ref, getSymbol, findSymbol)
	if !errors.Is(err, ErrAmbiguousResolution) {
		t.Fatalf("err = %v, want wrapping ErrAmbiguousResolution", err)
	}
}

func TestResolveRef_MultipleMatchesHalts(t *testing.T) {
	ref := SymbolRef{Key: "old-key", QualifiedName: "A", Path: "x.go"}
	getSymbol := func(k string) (codeindex.Symbol, error) { return codeindex.Symbol{}, codestore.ErrNotFound }
	findSymbol := func(qn, path string) ([]codeindex.Symbol, error) {
		return []codeindex.Symbol{{Key: "k1"}, {Key: "k2"}}, nil
	}

	_, err := ResolveRef(ref, getSymbol, findSymbol)
	if !errors.Is(err, ErrAmbiguousResolution) {
		t.Fatalf("err = %v, want wrapping ErrAmbiguousResolution", err)
	}
}

func TestResolveRef_OtherGetSymbolErrorPropagates(t *testing.T) {
	wantErr := errors.New("db exploded")
	ref := SymbolRef{Key: "a"}
	getSymbol := func(k string) (codeindex.Symbol, error) { return codeindex.Symbol{}, wantErr }
	findSymbol := func(qn, path string) ([]codeindex.Symbol, error) {
		t.Fatalf("findSymbol should not be called on a non-NotFound error")
		return nil, nil
	}

	_, err := ResolveRef(ref, getSymbol, findSymbol)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}
