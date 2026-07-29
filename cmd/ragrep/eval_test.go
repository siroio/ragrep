package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/store"
)

// TestEvalRecall runs a small text-mode eval set against a real store (text
// mode needs no embedding model, so it works in CI). "bravo" only appears in
// b.md, so the third case (which wants it in a.md) must miss.
func TestEvalRecall(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("a.md", "alpha content here.", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("b.md", "bravo content here.", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("c.md", "charlie content here.", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	evalPath := filepath.Join(t.TempDir(), "cases.jsonl")
	content := `{"query":"alpha","doc":"a.md"}
{"query":"alpha","doc":"a.md","para":0}
{"query":"bravo","doc":"a.md"}
`
	if err := os.WriteFile(evalPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"eval", "--db", dbPath, "--mode", "text", "-k", "10", evalPath})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 0 {
		t.Fatalf("eval exit=%d, want 0, stdout=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), `miss: "bravo" want a.md`) {
		t.Fatalf("stdout=%q, want miss line for bravo", buf.String())
	}
	if !strings.Contains(buf.String(), "recall@10: 0.667 (2/3)") {
		t.Fatalf("stdout=%q, want recall@10: 0.667 (2/3)", buf.String())
	}
}

// readEvalCases must skip blank lines and correctly distinguish an absent
// "para" (nil -- doc-level match) from an explicit "para":0 (a real, valid
// seq number). A malformed line is a hard parse error, which cmdEval must
// surface as exit 1; a file with zero cases after parsing is also exit 1.
func TestEvalCaseParse(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.jsonl")
	content := "\n{\"query\":\"alpha\",\"doc\":\"a.md\"}\n\n{\"query\":\"bravo\",\"doc\":\"b.md\",\"para\":0}\n\n"
	if err := os.WriteFile(good, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := readEvalCases(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("got %d cases, want 2 (blank lines skipped)", len(cases))
	}
	if cases[0].Para != nil {
		t.Fatalf("case 0 Para=%v, want nil (absent = doc-level)", cases[0].Para)
	}
	if cases[1].Para == nil || *cases[1].Para != 0 {
		t.Fatalf("case 1 Para=%v, want pointer to 0 (explicit, valid seq)", cases[1].Para)
	}

	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{not valid json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvalCases(bad); err == nil {
		t.Fatal("want error for malformed JSON line")
	}

	// A typo'd key ("dok" instead of "doc") parses fine as valid JSON but
	// leaves Doc=="", which would otherwise become a silent guaranteed miss
	// that understates recall instead of surfacing as a parse error.
	missingDoc := filepath.Join(dir, "missing_doc.jsonl")
	if err := os.WriteFile(missingDoc, []byte(`{"query":"x","dok":"a.md"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvalCases(missingDoc); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("readEvalCases(missing doc key) = %v, want a line-numbered error", err)
	}

	missingQuery := filepath.Join(dir, "missing_query.jsonl")
	if err := os.WriteFile(missingQuery, []byte("\n"+`{"doc":"a.md"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvalCases(missingQuery); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("readEvalCases(missing query key) = %v, want a line-numbered error", err)
	}

	db := filepath.Join(t.TempDir(), "index.db")
	if code := run([]string{"eval", "--db", db, "--mode", "text", bad}); code != 1 {
		t.Fatalf("eval malformed file: exit=%d, want 1", code)
	}

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"eval", "--db", db, "--mode", "text", empty}); code != 1 {
		t.Fatalf("eval empty file: exit=%d, want 1", code)
	}
}
