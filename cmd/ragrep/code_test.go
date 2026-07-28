package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/codestore"
)

// --- usage / argument errors (no gopls, no model needed) ---

func TestCmdCodeNoSubcommand(t *testing.T) {
	if code := run([]string{"code"}); code != 1 {
		t.Fatalf("code with no subcommand: exit=%d, want 1", code)
	}
}

func TestCmdCodeUnknownSubcommand(t *testing.T) {
	if code := run([]string{"code", "bogus"}); code != 1 {
		t.Fatalf("code bogus: exit=%d, want 1", code)
	}
}

func TestCmdCodeHelpExitsZero(t *testing.T) {
	if code := run([]string{"code", "-h"}); code != 0 {
		t.Fatalf("code -h: exit=%d, want 0", code)
	}
}

func TestCmdCodeIndexUsageErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), ".ragrep", "code.db")

	// No paths at all.
	if code := run([]string{"code", "index", "--db", db, "--language", "go"}); code != 1 {
		t.Fatalf("index with no paths: exit=%d, want 1", code)
	}
	// No --language.
	if code := run([]string{"code", "index", "--db", db, "somepath"}); code != 1 {
		t.Fatalf("index with no --language: exit=%d, want 1", code)
	}
}

func TestCmdCodeIndexUnsupportedLanguage(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "code.db")

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "index", "--db", db, "--language", "python", root})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("unsupported language: exit=%d, want 1", code)
	}
	if !strings.Contains(buf.String(), "unsupported --language") {
		t.Fatalf("stderr=%q, want message about unsupported language", buf.String())
	}
}

// code index must reject a path outside the workspace root BEFORE ever
// consulting config.Servers -- an unconfigured/absent servers map must not
// change this outcome (mirrors doc cmdIndex's outside-root check).
func TestCmdCodeIndexRejectsOutsideRoot(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "code.db")

	outside := filepath.Join(filepath.Dir(root), "ragrep-code-index-outside-test")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "index", "--db", db, "--language", "go", outside})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("outside-root path: exit=%d, want 1", code)
	}
	if !strings.Contains(buf.String(), "outside the workspace root") {
		t.Fatalf("stderr=%q, want outside-workspace-root message", buf.String())
	}
}

// An in-workspace path with no server registered for --language must fail
// clearly, and must never attempt to start (auto-download, or otherwise
// launch) any server.
func TestCmdCodeIndexUnregisteredServer(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// No .ragrep/config.json at all -- config.Load falls back to defaults,
	// meaning no servers are registered for any language.
	db := filepath.Join(root, ".ragrep", "code.db")

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "index", "--db", db, "--language", "go", root})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("unregistered server: exit=%d, want 1", code)
	}
	if !strings.Contains(buf.String(), "no language server registered") {
		t.Fatalf("stderr=%q, want unregistered-server message", buf.String())
	}
}

// With a registered (but never-invoked) server and zero matching files under
// the root, `code index` must finish successfully without ever starting the
// language server or touching the embedding model -- proven here by
// registering a server executable that doesn't exist: if cmdCodeIndex tried
// to start it, the command would fail instead of reporting "0 indexed".
func TestCmdCodeIndexZeroFilesSkipsServerAndModel(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeRagrepConfig(t, root, `{"servers": {"go": "ragrep-nonexistent-server-binary-xyz"}}`)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("not go"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "code.db")

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"code", "index", "--db", db, "--language", "go", root})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 0 {
		t.Fatalf("zero .go files: exit=%d, want 0, stdout=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "0 indexed") {
		t.Fatalf("stdout=%q, want a 0-indexed summary", buf.String())
	}
}

func writeRagrepConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- discoverCodeFiles: deterministic enumeration + exclusion patterns ---

func TestDiscoverCodeFilesDeterministicAndExcludes(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Included.
	mustWriteFile("z.go")
	mustWriteFile("a.go")
	mustWriteFile("a_test.go")           // test files ARE included (needed for tests-relation later)
	mustWriteFile("sub/nested.go")       // ordinary subdirectory
	mustWriteFile("testdata/fixture.go") // testdata IS included

	// Excluded.
	mustWriteFile("notes.txt")               // wrong extension
	mustWriteFile("vendor/dep/dep.go")       // vendor/
	mustWriteFile("node_modules/pkg/pkg.go") // node_modules/
	mustWriteFile(".git/objects/x.go")       // hidden dir
	mustWriteFile("dist/out.go")             // build-output dir
	mustWriteFile("build/artifact.go")       // build-output dir
	mustWriteFile("bin/tool.go")             // build-output dir

	// Excluded: generated file (marker line before the package clause).
	genPath := filepath.Join(root, "generated.go")
	genSrc := "// Code generated by mockgen. DO NOT EDIT.\n\npackage p\n"
	if err := os.WriteFile(genPath, []byte(genSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := discoverCodeFiles(root, ".go")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(root, "a.go"),
		filepath.Join(root, "a_test.go"),
		filepath.Join(root, "sub", "nested.go"),
		filepath.Join(root, "testdata", "fixture.go"),
		filepath.Join(root, "z.go"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files %v, want %d files %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q, want %q (full got=%v)", i, got[i], want[i], got)
		}
	}
}

// isGeneratedGoFile: the marker must be a comment line matched within the
// first 5 lines (or up to the first non-comment line, whichever is first) --
// an ordinary doc comment must not false-positive, and a marker past that
// window must not be detected.
func TestIsGeneratedGoFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	generated := write("gen.go", "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage p\n")
	if !isGeneratedGoFile(generated) {
		t.Fatal("want the standard marker line detected as generated")
	}

	ordinary := write("ord.go", "// Package p does things.\npackage p\n")
	if isGeneratedGoFile(ordinary) {
		t.Fatal("an ordinary doc comment must not be treated as generated")
	}

	tooDeep := write("deep.go", "// l1\n// l2\n// l3\n// l4\n// l5\n// Code generated by x. DO NOT EDIT.\npackage p\n")
	if isGeneratedGoFile(tooDeep) {
		t.Fatal("a marker past the first 5 lines must not be detected")
	}
}

// --- code search / get output formatting, against a real codestore built
// directly via its own API with a fake embedder (no ONNX model, no gopls). ---

func fakeCodeEmbed(text string) ([]float32, error) {
	v := make([]float32, codeEmbedDim)
	for i, r := range text {
		v[i%codeEmbedDim] += float32(r % 13)
	}
	return v, nil
}

func newTestCodeStore(t *testing.T) *codestore.Store {
	t.Helper()
	s, err := codestore.Open(filepath.Join(t.TempDir(), "code.db"), "test-model", codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testSymbol() codeindex.Symbol {
	sym := codeindex.Symbol{
		Key:           "deadbeef",
		Language:      "go",
		Kind:          "function",
		Name:          "Foo",
		QualifiedName: "Foo",
		Signature:     "func Foo()",
		Documentation: "Foo does things.",
		Path:          "pkg/foo.go",
		Range: codeindex.Range{
			Start: codeindex.Position{Line: 1, Character: 0},
			End:   codeindex.Position{Line: 3, Character: 1},
		},
		Body:     "func Foo() {}\n",
		BodyHash: "bodyhash1",
	}
	sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
	return sym
}

func TestFormatCodeSearchHits(t *testing.T) {
	s := newTestCodeStore(t)
	sym := testSymbol()
	if _, err := s.UpsertSymbols(sym.Path, "filehash1", []codeindex.Symbol{sym}, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchSymbolsText("Foo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("SearchSymbolsText: got %d hits, want 1", len(hits))
	}

	var text bytes.Buffer
	if err := formatCodeSearchHits(&text, hits, false); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	for _, want := range []string{sym.Key, sym.Kind, sym.QualifiedName, sym.Signature, "pkg/foo.go:1-3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, sym.Body) {
		t.Fatalf("text output must not include body: %q", out)
	}

	var js bytes.Buffer
	if err := formatCodeSearchHits(&js, hits, true); err != nil {
		t.Fatal(err)
	}
	var decoded []codestore.SymbolHit
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON output invalid: %v (%q)", err, js.String())
	}
	if len(decoded) != 1 || decoded[0].Key != sym.Key {
		t.Fatalf("decoded JSON=%v, want one hit with key %q", decoded, sym.Key)
	}
}

func TestFormatCodeSymbol(t *testing.T) {
	sym := testSymbol()

	var meta bytes.Buffer
	if err := formatCodeSymbol(&meta, sym, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(meta.String(), sym.Body) {
		t.Fatalf("metadata-only output must not include body: %q", meta.String())
	}
	for _, want := range []string{sym.Key, sym.Kind, sym.QualifiedName, sym.Signature} {
		if !strings.Contains(meta.String(), want) {
			t.Fatalf("metadata output %q missing %q", meta.String(), want)
		}
	}

	var withBody bytes.Buffer
	if err := formatCodeSymbol(&withBody, sym, true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withBody.String(), sym.Body) {
		t.Fatalf("--body output must include body: %q", withBody.String())
	}

	var js bytes.Buffer
	if err := formatCodeSymbol(&js, sym, true, true); err != nil {
		t.Fatal(err)
	}
	var decoded codeSymbolOutput
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON output invalid: %v (%q)", err, js.String())
	}
	if decoded.Key != sym.Key || decoded.Body != sym.Body {
		t.Fatalf("decoded=%+v, want key=%q body=%q", decoded, sym.Key, sym.Body)
	}

	var jsNoBody bytes.Buffer
	if err := formatCodeSymbol(&jsNoBody, sym, false, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsNoBody.String(), `"body"`) {
		t.Fatalf("JSON without --body must omit the body field entirely: %q", jsNoBody.String())
	}
}

// --- code search / code get CLI-level argument errors and not-found paths
// (open a real (empty) code.db, but never touch the embedding model) ---

func TestCmdCodeSearchUsageErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	if code := run([]string{"code", "search", "--db", db}); code != 1 {
		t.Fatalf("search with no query: exit=%d, want 1", code)
	}
	if code := run([]string{"code", "search", "--db", db, "a", "b"}); code != 1 {
		t.Fatalf("search with two args: exit=%d, want 1", code)
	}
}

func TestCmdCodeGetUsageErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	if code := run([]string{"code", "get", "--db", db}); code != 1 {
		t.Fatalf("get with no --symbol: exit=%d, want 1", code)
	}
}

func TestCmdCodeGetNotFound(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	s, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "get", "--db", db, "--symbol", "does-not-exist"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 2 {
		t.Fatalf("get missing symbol: exit=%d, want 2", code)
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Fatalf("stderr=%q, want not-found message", buf.String())
	}
}

// --- document `index` command's default code-extension exclusion ---

func TestCmdIndexExcludesCodeExtensionsByDefault(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"index", "--db", db, root})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 0 {
		t.Fatalf("index: exit=%d, want 0, stdout=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "1 excluded") {
		t.Fatalf("stdout=%q, want it to report 1 excluded file", buf.String())
	}
	if strings.Contains(buf.String(), "indexed main.go") {
		t.Fatalf("stdout=%q, main.go must not have been indexed", buf.String())
	}

	s, err := openStoreAt(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetDoc("main.go"); err == nil {
		t.Fatal("main.go must not be in the document index by default")
	}
}

// --include-code disables the default code-extension exclusion.
func TestCmdIndexIncludeCodeFlag(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")

	if code := run([]string{"index", "--db", db, "--include-code", root}); code != 0 {
		t.Fatalf("index --include-code: exit=%d, want 0", code)
	}

	s, err := openStoreAt(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetDoc("main.go"); err != nil {
		t.Fatalf("main.go should be indexed with --include-code: %v", err)
	}
}
