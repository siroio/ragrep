package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/coderetrieval"
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
	if _, err := s.UpsertSymbols(sym.Path, "filehash1", []codeindex.Symbol{sym}, 0, fakeCodeEmbed); err != nil {
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

// --- code expand ---

func TestCmdCodeExpandUsageErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")

	if code := run([]string{"code", "expand", "--db", db, "--relation", "definition"}); code != 1 {
		t.Fatalf("expand with no --symbol: exit=%d, want 1", code)
	}
	if code := run([]string{"code", "expand", "--db", db, "--symbol", "k"}); code != 1 {
		t.Fatalf("expand with no --relation: exit=%d, want 1", code)
	}
	if code := run([]string{"code", "expand", "--db", db, "--symbol", "k", "--relation", "bogus"}); code != 1 {
		t.Fatalf("expand with invalid --relation: exit=%d, want 1", code)
	}
	if code := run([]string{"code", "expand", "--db", db, "--symbol", "k", "--relation", "definition", "extra"}); code != 1 {
		t.Fatalf("expand with extra positional arg: exit=%d, want 1", code)
	}
}

func TestCmdCodeExpandNotFound(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	s, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "expand", "--db", db, "--symbol", "does-not-exist", "--relation", "definition"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 2 {
		t.Fatalf("expand missing symbol: exit=%d, want 2", code)
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Fatalf("stderr=%q, want not-found message", buf.String())
	}
}

// expandTestSymbol inserts one indexed Go symbol directly (bypassing `code
// index`, no language server needed) into root/.ragrep/code.db and returns
// its stable key -- enough setup for expand tests that only care about
// argument handling / server resolution up to (but not through) an actual
// LSP query.
func expandTestSymbol(t *testing.T, root string) (db, key string) {
	t.Helper()
	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db = filepath.Join(ragrepDir, "code.db")
	s, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sym := codeindex.Symbol{
		Key: "expand-target", Language: "go", Kind: "function",
		Name: "main", QualifiedName: "main", Signature: "func main()",
		Path: "main.go",
		Range: codeindex.Range{
			Start: codeindex.Position{Line: 2, Character: 0},
			End:   codeindex.Position{Line: 2, Character: 14},
		},
		Body: "func main() {}",
	}
	sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
	if _, err := s.UpsertSymbols(sym.Path, "hash1", []codeindex.Symbol{sym}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
	return db, sym.Key
}

// An in-workspace symbol whose language has no registered server must fail
// clearly, mirroring `code index`'s unregistered-server check -- and, same
// as index, must never attempt to start anything.
func TestCmdCodeExpandUnregisteredServer(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, key := expandTestSymbol(t, root)
	// No .ragrep/config.json -- config.Load falls back to defaults, so no
	// server is registered for "go".

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "expand", "--db", db, "--symbol", key, "--relation", "definition"})
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

// buildFakeLSPServerFromSrc compiles an arbitrary Go source into a
// standalone executable in dir, the same way code_fakelsp_test.go's
// buildFakeLSPServer does for its one fixed fakeLSPServerSrc -- duplicated
// (rather than parameterizing that helper) since code_fakelsp_test.go is
// out of scope for this task. Skips the test if the "go" toolchain isn't on
// PATH.
func buildFakeLSPServerFromSrc(t *testing.T, dir, name, src string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping fake-LSP-server test")
	}

	srcPath := filepath.Join(dir, name+".go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(dir, name+".exe")
	cmd := exec.Command(goBin, "build", "-o", exePath, srcPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building fake LSP server %s: %v: %s", name, err, stderr.String())
	}
	return exePath
}

// fakeLSPServerAllCapsEmptySrc advertises every capability `code expand`
// cares about and answers every request it drives (definition, references,
// prepareCallHierarchy) with an empty result array -- for exercising the
// "no results" path (as opposed to a capability-gate failure) without a
// real language server.
const fakeLSPServerAllCapsEmptySrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
)

func main() {
	tp := textproto.NewReader(bufio.NewReader(os.Stdin))
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil {
			return
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return
		}
		idRaw, hasID := msg["id"]
		if !hasID {
			continue
		}
		var method string
		if m, ok := msg["method"]; ok {
			json.Unmarshal(m, &method)
		}
		result := "null"
		switch method {
		case "initialize":
			result = ` + "`" + `{"capabilities":{"definitionProvider":true,"referencesProvider":true,"callHierarchyProvider":true}}` + "`" + `
		case "textDocument/definition", "textDocument/references", "textDocument/prepareCallHierarchy":
			result = "[]"
		}
		resp := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}", string(idRaw), result)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
	}
}
`

// The server capability gate: a server that doesn't advertise the feature
// `--relation` needs must be reported distinctly from a query that ran and
// simply found nothing (see TestCmdCodeExpandNoResults) -- checked here
// against a real (if minimal) LSP-speaking subprocess that only advertises
// definitionProvider, requesting --relation callers (needs call hierarchy).
func TestCmdCodeExpandCapabilityGate(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, key := expandTestSymbol(t, root)
	exePath := buildFakeLSPServer(t, root)
	writeRagrepConfig(t, root, `{"servers": {"go": "`+filepath.ToSlash(exePath)+`"}}`)

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "expand", "--db", db, "--symbol", key, "--relation", "callers"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("capability gate: exit=%d, want 1, stderr=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not supported by server") {
		t.Fatalf("stderr=%q, want a distinct not-supported message", buf.String())
	}
}

// A capability the server DOES advertise, whose query simply returns no
// locations, must be reported differently from the capability gate above
// (exit code and message both differ) -- proving expand distinguishes
// "can't ask" from "asked, got nothing".
func TestCmdCodeExpandNoResults(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, key := expandTestSymbol(t, root)
	exePath := buildFakeLSPServerFromSrc(t, root, "fakelsp-allcaps-empty", fakeLSPServerAllCapsEmptySrc)
	writeRagrepConfig(t, root, `{"servers": {"go": "`+filepath.ToSlash(exePath)+`"}}`)

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "expand", "--db", db, "--symbol", key, "--relation", "references"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 2 {
		t.Fatalf("no results: exit=%d, want 2, stderr=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "no results") {
		t.Fatalf("stderr=%q, want a no-results message", buf.String())
	}
	if strings.Contains(buf.String(), "not supported") {
		t.Fatalf("stderr=%q, no-results must not read like a capability failure", buf.String())
	}
}

// fakeLSPServerReferencesSrcTemplate answers textDocument/references with a
// fixed JSON array of Locations, substituted in for
// LOCATIONS_JSON_PLACEHOLDER -- for exercising `code expand --relation
// references` against duplicate and unresolved locations without a real
// language server (see TestCmdCodeExpandDedupsRelationsAndSkipsUnresolved).
const fakeLSPServerReferencesSrcTemplate = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
)

func main() {
	tp := textproto.NewReader(bufio.NewReader(os.Stdin))
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		n, err := strconv.Atoi(hdr.Get("Content-Length"))
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tp.R, body); err != nil {
			return
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return
		}
		idRaw, hasID := msg["id"]
		if !hasID {
			continue
		}
		var method string
		if m, ok := msg["method"]; ok {
			json.Unmarshal(m, &method)
		}
		result := "null"
		switch method {
		case "initialize":
			result = ` + "`" + `{"capabilities":{"referencesProvider":true}}` + "`" + `
		case "textDocument/references":
			result = ` + "`" + `LOCATIONS_JSON_PLACEHOLDER` + "`" + `
		}
		resp := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}", string(idRaw), result)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
	}
}
`

// lspLocationJSON renders one textDocument/references Location for path
// (absolute) at the given zero-based line.
func lspLocationJSON(path string, line int) string {
	return fmt.Sprintf(`{"uri":%q,"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":0}}}`,
		fileURI(path), line, line)
}

// A symbol referenced twice from the same enclosing symbol (two locations
// both resolving to the same caller) must not crash `code expand
// --relation references` with symbol_edges' UNIQUE(from_key, to_key, kind,
// source) constraint -- see codeindex.DedupResolvedRelations. A third,
// unresolved location (outside the indexed store) must still show up in the
// printed output but must never reach ReplaceRelations.
func TestCmdCodeExpandDedupsRelationsAndSkipsUnresolved(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainGo := filepath.Join(root, "main.go")
	if err := os.WriteFile(mainGo, []byte("package main\n\nfunc Callee() {}\n\nfunc Caller() {\n\tCallee()\n\tCallee()\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(ragrepDir, "code.db")
	s, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	target := codeindex.Symbol{
		Key: "target-key", Language: "go", Kind: "function",
		Name: "Callee", QualifiedName: "Callee", Signature: "func Callee()",
		Path: "main.go",
		Range: codeindex.Range{
			Start: codeindex.Position{Line: 2, Character: 0},
			End:   codeindex.Position{Line: 2, Character: 16},
		},
		Body: "func Callee() {}",
	}
	target.EmbeddingText = codeindex.RenderEmbeddingText(target)
	caller := codeindex.Symbol{
		Key: "caller-key", Language: "go", Kind: "function",
		Name: "Caller", QualifiedName: "Caller", Signature: "func Caller()",
		Path: "main.go",
		Range: codeindex.Range{
			Start: codeindex.Position{Line: 4, Character: 0},
			End:   codeindex.Position{Line: 7, Character: 1},
		},
		Body: "func Caller() {\n\tCallee()\n\tCallee()\n}",
	}
	caller.EmbeddingText = codeindex.RenderEmbeddingText(caller)
	if _, err := s.UpsertSymbols("main.go", "hash1", []codeindex.Symbol{target, caller}, 0, fakeCodeEmbed); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	// Two locations at different lines, both inside Caller's range (4-7):
	// both resolve to caller-key -- a duplicate resolved relation. A third
	// location outside the workspace root's indexed symbols never resolves.
	locationsJSON := "[" + strings.Join([]string{
		lspLocationJSON(mainGo, 5),
		lspLocationJSON(mainGo, 6),
		lspLocationJSON(filepath.Join(root, "unindexed.go"), 0),
	}, ",") + "]"
	src := strings.ReplaceAll(fakeLSPServerReferencesSrcTemplate, "LOCATIONS_JSON_PLACEHOLDER", locationsJSON)
	exePath := buildFakeLSPServerFromSrc(t, root, "fakelsp-dup-refs", src)
	writeRagrepConfig(t, root, `{"servers": {"go": "`+filepath.ToSlash(exePath)+`"}}`)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"code", "expand", "--db", db, "--symbol", "target-key", "--relation", "references", "--json"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 0 {
		t.Fatalf("expand with duplicate+unresolved locations: exit=%d, want 0, output=%q", code, buf.String())
	}

	var targets []codeExpandTarget
	if err := json.Unmarshal(buf.Bytes(), &targets); err != nil {
		t.Fatalf("output not valid JSON: %v (%q)", err, buf.String())
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %#v, want 3 (two duplicate resolved + one unresolved)", targets)
	}

	// The persisted edge set must be deduped: exactly one references row
	// from target-key to caller-key, not two.
	s2, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rels, err := s2.RelationsFrom("target-key")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range rels {
		if r.ToKey == "caller-key" && r.Kind == "references" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("persisted references edges from target-key to caller-key = %d, want 1 (deduped)", count)
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

// --- code pack / code verify ---

// codePackTestSymbol builds a symbol with a body long enough to matter for
// budget-truncation tests, at the given key/path/qualifiedName.
func codePackTestSymbol(key, path, qualifiedName string) codeindex.Symbol {
	sym := codeindex.Symbol{
		Key: key, Language: "go", Kind: "function",
		Name: qualifiedName, QualifiedName: qualifiedName, Signature: "func " + qualifiedName + "()",
		Path: path,
		Range: codeindex.Range{
			Start: codeindex.Position{Line: 1, Character: 0},
			End:   codeindex.Position{Line: 3, Character: 1},
		},
		Body:     "func " + qualifiedName + "() {\n\t// a reasonably long body so JSON-encoded size is non-trivial\n\treturn\n}\n",
		BodyHash: "bodyhash-" + key,
	}
	sym.EmbeddingText = codeindex.RenderEmbeddingText(sym)
	return sym
}

func TestCmdCodePackUsageErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	if code := run([]string{"code", "pack", "--db", db}); code != 1 {
		t.Fatalf("pack with no --query: exit=%d, want 1", code)
	}
	if code := run([]string{"code", "pack", "--db", db, "--query", "q",
		"--select", "a", "--select", "b", "--select", "c", "--select", "d"}); code != 1 {
		t.Fatalf("pack with 4 --select keys: exit=%d, want 1", code)
	}
}

// runCodePack is `code pack`'s core logic minus the ONNX embedding call --
// tests drive it with a fake query vector, the same way TestFormatCodeSearchHits
// drives formatCodeSearchHits directly instead of going through the real CLI
// (which would require the cached embedding model).
func TestRunCodePackBudgetRespectedAndTruncationSurfaces(t *testing.T) {
	s := newTestCodeStore(t)
	a := codePackTestSymbol("a", "x.go", "A")
	b := codePackTestSymbol("b", "y.go", "B")
	if _, err := s.UpsertSymbols(a.Path, "filehash-a", []codeindex.Symbol{a}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSymbols(b.Path, "filehash-b", []codeindex.Symbol{b}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	if _, err := s.RecordIndexRun("root", "rev123", "go", "gopls", "v1.2.3", codeModelID, when); err != nil {
		t.Fatal(err)
	}

	qv, err := fakeCodeEmbed("A")
	if err != nil {
		t.Fatal(err)
	}

	// First, measure the metadata-only cost (large budget, no --select) so
	// the truncation budget below is derived rather than guessed.
	metaOnly, err := runCodePack(s, "A", qv, 10, 100_000, nil)
	if err != nil {
		t.Fatalf("runCodePack (measure): %v", err)
	}
	if metaOnly.Pack.Truncated {
		t.Fatalf("measurement pack unexpectedly truncated: %+v", metaOnly.Pack)
	}
	budget := metaOnly.Pack.UsedChars + 50 // room for candidates + a sliver, not both bodies

	out, err := runCodePack(s, "A", qv, 10, budget, []string{"a", "b"})
	if err != nil {
		t.Fatalf("runCodePack: %v", err)
	}
	if !out.Pack.Truncated {
		t.Fatalf("pack.Truncated = false, want true: usedChars=%d budget=%d", out.Pack.UsedChars, budget)
	}
	if len(out.Pack.Skipped) == 0 {
		t.Fatal("pack.Skipped is empty, want at least one skipped item")
	}
	if out.Pack.UsedChars > out.Pack.Budget {
		t.Fatalf("pack.UsedChars = %d exceeds budget %d", out.Pack.UsedChars, out.Pack.Budget)
	}

	if out.Manifest.IndexRevision != "rev123" || out.Manifest.ServerName != "gopls" ||
		out.Manifest.ServerVersion != "v1.2.3" || out.Manifest.ModelID != codeModelID {
		t.Fatalf("manifest identity = %+v, want revision=rev123 server=gopls/v1.2.3 model=%s", out.Manifest, codeModelID)
	}
	// Only symbols that actually made it into the pack (not skipped) may
	// appear as manifest refs -- SymbolFileHash would error on any key that
	// isn't packed's own Symbols.
	if len(out.Manifest.Symbols) != len(out.Pack.Symbols) {
		t.Fatalf("len(manifest.Symbols) = %d, want %d (== len(pack.Symbols))", len(out.Manifest.Symbols), len(out.Pack.Symbols))
	}
}

func TestFormatCodePackOutputTextAndJSON(t *testing.T) {
	s := newTestCodeStore(t)
	a := codePackTestSymbol("a", "x.go", "A")
	if _, err := s.UpsertSymbols(a.Path, "filehash-a", []codeindex.Symbol{a}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
	qv, err := fakeCodeEmbed("A")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCodePack(s, "A", qv, 10, 100_000, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}

	var text bytes.Buffer
	if err := formatCodePackOutput(&text, out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "candidates=1") || !strings.Contains(text.String(), "symbols=1") {
		t.Fatalf("text output %q missing expected counts", text.String())
	}

	var js bytes.Buffer
	if err := formatCodePackOutput(&js, out, true); err != nil {
		t.Fatal(err)
	}
	var decoded codePackOutput
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON output invalid: %v (%q)", err, js.String())
	}
	if len(decoded.Pack.Symbols) != 1 || decoded.Pack.Symbols[0].Key != "a" {
		t.Fatalf("decoded pack = %+v, want one symbol key=a", decoded.Pack)
	}
	if decoded.Manifest.Symbols[0].Key != "a" {
		t.Fatalf("decoded manifest = %+v, want one symbol key=a", decoded.Manifest)
	}
}

func TestCmdCodeVerifyUsageErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	if code := run([]string{"code", "verify", "--db", db}); code != 1 {
		t.Fatalf("verify with no --manifest: exit=%d, want 1", code)
	}
}

func TestCmdCodeVerifyMissingManifestFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	if code := run([]string{"code", "verify", "--db", db, "--manifest", filepath.Join(t.TempDir(), "nope.json")}); code != 1 {
		t.Fatalf("verify with missing manifest file: exit=%d, want 1", code)
	}
}

// codeVerifyWorkspace builds a root/.ragrep/code.db workspace with one
// indexed symbol at root/pkg/foo.go, using fileHash = codeindex.FileHash of
// the actual on-disk content -- so a `code verify` run right afterward finds
// the file unmodified (clean) unless the test itself edits it. Returns the
// root, db path, and the manifest built from packing that single symbol.
func codeVerifyWorkspace(t *testing.T) (root, db string, manifest coderetrieval.Manifest) {
	t.Helper()
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("package pkg\n\nfunc A() {}\n")
	if err := os.WriteFile(filepath.Join(root, "pkg", "foo.go"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	db = filepath.Join(root, ".ragrep", "code.db")
	s, err := openCodeStoreAt(db)
	if err != nil {
		t.Fatal(err)
	}

	sym := codePackTestSymbol("a", "pkg/foo.go", "A")
	fileHash := codeindex.FileHash(content)
	if _, err := s.UpsertSymbols(sym.Path, fileHash, []codeindex.Symbol{sym}, 0, fakeCodeEmbed); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.RecordIndexRun("root", "rev1", "go", "gopls", "v1", codeModelID, time.Now()); err != nil {
		s.Close()
		t.Fatal(err)
	}

	qv, err := fakeCodeEmbed("A")
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	out, err := runCodePack(s, "A", qv, 10, 100_000, []string{"a"})
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Manifest.Symbols) != 1 {
		t.Fatalf("codeVerifyWorkspace: manifest has %d symbols, want 1", len(out.Manifest.Symbols))
	}
	return root, db, out.Manifest
}

func writeManifestFile(t *testing.T, m coderetrieval.Manifest) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCmdCodeVerifyManifestRoundTripClean(t *testing.T) {
	_, db, manifest := codeVerifyWorkspace(t)
	manifestPath := writeManifestFile(t, manifest)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"code", "verify", "--db", db, "--manifest", manifestPath, "--json"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 0 {
		t.Fatalf("verify clean manifest: exit=%d, want 0, stdout=%q", code, buf.String())
	}
	var decoded codeVerifyOutput
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, buf.String())
	}
	if !decoded.Clean {
		t.Fatalf("decoded.Clean = false, want true: %+v", decoded)
	}
	if len(decoded.Entries) != 1 || decoded.Entries[0].Stale || !decoded.Entries[0].Resolved {
		t.Fatalf("decoded.Entries = %+v, want one clean, resolved entry", decoded.Entries)
	}
}

func TestCmdCodeVerifyDetectsStaleFile(t *testing.T) {
	root, db, manifest := codeVerifyWorkspace(t)
	manifestPath := writeManifestFile(t, manifest)

	// Edit the file the manifest's only entry points at, after the manifest
	// was built: its file_hash no longer matches.
	if err := os.WriteFile(filepath.Join(root, "pkg", "foo.go"), []byte("package pkg\n\nfunc A() { /* edited */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"code", "verify", "--db", db, "--manifest", manifestPath, "--json"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 2 {
		t.Fatalf("verify after file edit: exit=%d, want 2, stdout=%q", code, buf.String())
	}
	var decoded codeVerifyOutput
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, buf.String())
	}
	if decoded.Clean {
		t.Fatal("decoded.Clean = true, want false after file edit")
	}
	if len(decoded.Entries) != 1 || !decoded.Entries[0].Stale {
		t.Fatalf("decoded.Entries = %+v, want one stale entry", decoded.Entries)
	}
}

// A manifest entry whose stable key no longer resolves (the symbol was
// deleted from the store, not just edited) AND whose qualified_name/path no
// longer identifies any indexed symbol must be reported as an unresolved,
// ambiguous-resolution entry -- not crash the command or silently guess.
func TestCmdCodeVerifyAmbiguousResolutionReportsUnresolved(t *testing.T) {
	root, db, manifest := codeVerifyWorkspace(t)

	// Remove the only symbol at that qualified_name+path from the store, so
	// ResolveRef's key lookup AND its fallback findSymbol lookup both fail:
	// zero matches is ambiguous (see coderetrieval.ErrAmbiguousResolution).
	s, err := openCodeStoreAt(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSymbols("pkg/foo.go", "some-other-hash", nil, 0, fakeCodeEmbed); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	manifestPath := writeManifestFile(t, manifest)
	_ = root

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"code", "verify", "--db", db, "--manifest", manifestPath, "--json"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 2 {
		t.Fatalf("verify with unresolvable entry: exit=%d, want 2, stdout=%q", code, buf.String())
	}
	var decoded codeVerifyOutput
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, buf.String())
	}
	if decoded.Clean {
		t.Fatal("decoded.Clean = true, want false")
	}
	if len(decoded.Entries) != 1 || decoded.Entries[0].Resolved || decoded.Entries[0].Error == "" {
		t.Fatalf("decoded.Entries = %+v, want one unresolved entry with a non-empty Error", decoded.Entries)
	}
}
