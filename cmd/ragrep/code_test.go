package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/siroio/ragrep/internal/codeindex"
	"github.com/siroio/ragrep/internal/coderetrieval"
	"github.com/siroio/ragrep/internal/codestore"
	"github.com/siroio/ragrep/internal/lsp"
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

// `ragrep code <sub> -h` must print codeUsage (the `code` subcommand help),
// not the top-level document-index usage -- a bare `-h` mid-subcommand used
// to fall through to parseArgs' hardcoded top-level usage string.
func TestCmdCodeSubcommandHelpPrintsCodeUsage(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	for _, sub := range []string{"index", "search", "get", "expand", "pack", "verify"} {
		r, w, _ := os.Pipe()
		old := os.Stderr
		os.Stderr = w
		code := run([]string{"code", sub, "--db", db, "-h"})
		w.Close()
		os.Stderr = old
		var buf bytes.Buffer
		buf.ReadFrom(r)

		if code != 0 {
			t.Fatalf("code %s -h: exit=%d, want 0, stderr=%q", sub, code, buf.String())
		}
		if !strings.Contains(buf.String(), "uses code.db, never the document index.db") {
			t.Fatalf("code %s -h stderr=%q, want codeUsage (not the top-level document-index usage)", sub, buf.String())
		}
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
	injectCodeServiceDaemon(t, root, db)

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
	injectCodeServiceDaemon(t, root, db)

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

// --- serverIdentity: gopls' serverInfo.version can be a ~2.7KB multi-line
// build-info blob (module list, checksums, ...) rather than a short version
// string -- stored verbatim it bloats every index_runs row and manifest.
// Only the first line is kept, further capped at 120 characters. ---

func TestServerIdentityTruncatesMultilineVersion(t *testing.T) {
	longSecondLine := strings.Repeat("x", 500)
	blob := "golang.org/x/tools/gopls v0.19.1\n" +
		"    golang.org/x/tools/gopls v0.19.1 h1:abc123\n" +
		"    " + longSecondLine

	initResult := &lsp.InitializeResult{ServerInfo: &lsp.ServerInfo{Name: "gopls", Version: blob}}
	name, version := serverIdentity(initResult)
	if name != "gopls" {
		t.Fatalf("name = %q, want gopls", name)
	}
	if strings.ContainsAny(version, "\r\n") {
		t.Fatalf("version = %q, must not contain a newline", version)
	}
	if len(version) > 120 {
		t.Fatalf("version length = %d, want <= 120", len(version))
	}
	if version != "golang.org/x/tools/gopls v0.19.1" {
		t.Fatalf("version = %q, want just the first line", version)
	}
}

func TestServerIdentityCapsLongSingleLineVersion(t *testing.T) {
	long := strings.Repeat("v", 500)
	initResult := &lsp.InitializeResult{ServerInfo: &lsp.ServerInfo{Name: "srv", Version: long}}
	_, version := serverIdentity(initResult)
	if len(version) != 120 {
		t.Fatalf("version length = %d, want exactly 120 (capped)", len(version))
	}
}

func TestServerIdentityDefaultsWhenNoServerInfo(t *testing.T) {
	name, version := serverIdentity(nil)
	if name != "unknown" || version != "unknown" {
		t.Fatalf("serverIdentity(nil) = (%q,%q), want (unknown,unknown)", name, version)
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

func TestNewCodeSymbolOutputIncludesBodyOnlyWhenRequested(t *testing.T) {
	sym := testSymbol()

	withoutBody := newCodeSymbolOutput(sym, false)
	if withoutBody.Key != sym.Key || withoutBody.Language != sym.Language || withoutBody.Kind != sym.Kind ||
		withoutBody.Name != sym.Name || withoutBody.QualifiedName != sym.QualifiedName ||
		withoutBody.Signature != sym.Signature || withoutBody.Documentation != sym.Documentation ||
		withoutBody.Container != sym.Container || withoutBody.Path != sym.Path ||
		withoutBody.StartLine != sym.Range.Start.Line || withoutBody.StartChar != sym.Range.Start.Character ||
		withoutBody.EndLine != sym.Range.End.Line || withoutBody.EndChar != sym.Range.End.Character {
		t.Fatalf("without body=%+v, want all symbol metadata", withoutBody)
	}
	if withoutBody.Body != "" {
		t.Fatalf("without body Body=%q, want empty", withoutBody.Body)
	}

	withBody := newCodeSymbolOutput(sym, true)
	if withBody.Body != sym.Body {
		t.Fatalf("with body Body=%q, want %q", withBody.Body, sym.Body)
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

type fakeCodeDaemonClient struct {
	search func(context.Context, searchRequest) (searchResponse, error)
	get    func(context.Context, getRequest) (codeindex.Symbol, error)
	index  func(context.Context, indexRequest) (indexResult, error)
	expand func(context.Context, expandRequest) ([]codeExpandTarget, error)
	pack   func(context.Context, packRequest) (codePackOutput, error)
	verify func(context.Context, verifyRequest) (codeVerifyOutput, error)
}

func (f fakeCodeDaemonClient) Search(ctx context.Context, req searchRequest) (searchResponse, error) {
	return f.search(ctx, req)
}

func (f fakeCodeDaemonClient) Get(ctx context.Context, req getRequest) (codeindex.Symbol, error) {
	return f.get(ctx, req)
}

func (f fakeCodeDaemonClient) Index(ctx context.Context, req indexRequest) (indexResult, error) {
	return f.index(ctx, req)
}

func (f fakeCodeDaemonClient) Expand(ctx context.Context, req expandRequest) ([]codeExpandTarget, error) {
	return f.expand(ctx, req)
}

func (f fakeCodeDaemonClient) Pack(ctx context.Context, req packRequest) (codePackOutput, error) {
	return f.pack(ctx, req)
}

func (f fakeCodeDaemonClient) Verify(ctx context.Context, req verifyRequest) (codeVerifyOutput, error) {
	return f.verify(ctx, req)
}

func injectCodeDaemonClient(t *testing.T, client codeDaemonClient) *int {
	t.Helper()
	old := codeDaemonClientFactory
	calls := 0
	codeDaemonClientFactory = func() (codeDaemonClient, error) {
		calls++
		return client, nil
	}
	t.Cleanup(func() { codeDaemonClientFactory = old })
	return &calls
}

func injectCodeServiceDaemon(t *testing.T, root, db string) *codeService {
	t.Helper()
	store, err := openCodeStoreAt(db)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newWorkspaceState(root, store, []string{"."}, "go")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	svc := newCodeService(func(got string) (*workspaceState, error) {
		if got != filepath.Clean(root) {
			return nil, fmt.Errorf("unknown workspace %q", got)
		}
		return state, nil
	}, newEmbeddingPool(func() (textEmbedder, error) { return new(serviceTestEmbedder), nil }), nil)
	server := httptest.NewServer(newDaemonHandler(svc, "test-token"))
	injectCodeDaemonClient(t, daemonClient{endpoint: server.URL, token: "test-token", client: server.Client()})
	t.Cleanup(func() {
		server.Close()
		_ = svc.Close()
		_ = state.Close()
		_ = store.Close()
	})
	return svc
}

func captureCodeCommand(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
	}()
	os.Stdout, os.Stderr = stdoutW, stderrW
	code := run(args)
	os.Stdout, os.Stderr = oldStdout, oldStderr
	stdoutW.Close()
	stderrW.Close()
	var stdout, stderr bytes.Buffer
	_, _ = stdout.ReadFrom(stdoutR)
	_, _ = stderr.ReadFrom(stderrR)
	return code, stdout.String(), stderr.String()
}

func TestCmdCodeSearchDaemonText(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	hit := codestore.SymbolHit{
		Key: "key-1", Kind: "function", QualifiedName: "pkg.Foo", Signature: "func Foo()",
		Path: "pkg/foo.go", StartLine: 4, EndLine: 8, Score: 0.25, FTSRank: 1, VecRank: 2, ExactMatch: true,
	}
	client := fakeCodeDaemonClient{search: func(_ context.Context, req searchRequest) (searchResponse, error) {
		if req.Root != filepath.Clean(root) || req.DB != filepath.Clean(db) || req.Query != "Foo" || req.Mode != "text" || req.K != 3 {
			t.Fatalf("request=%+v", req)
		}
		return searchResponse{Hits: []codestore.SymbolHit{hit}, Fresh: true, Generation: 9, Degraded: "vector_unavailable"}, nil
	}}
	calls := injectCodeDaemonClient(t, client)

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "search", "--mode", "text", "-k", "3", "--db", db, "Foo"})
	if code != 0 || stderr != "degraded: vector_unavailable\n" || *calls != 1 {
		t.Fatalf("exit=%d factory calls=%d stdout=%q stderr=%q", code, *calls, stdout, stderr)
	}
	want := "key-1\tfunction\tpkg.Foo\tfunc Foo()\tpkg/foo.go:4-8\tscore=0.2500 (fts=1 vec=2 exact=true)\n"
	if stdout != want {
		t.Fatalf("stdout=%q, want byte-for-byte %q", stdout, want)
	}
}

func TestCmdCodeSearchDaemonJSONDefaultsToAutoFive(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	hit := codestore.SymbolHit{Key: "key-1", Path: "pkg/foo.go"}
	client := fakeCodeDaemonClient{search: func(_ context.Context, req searchRequest) (searchResponse, error) {
		if req.Root != filepath.Clean(root) || req.Query != "Foo" || req.Mode != "auto" || req.K != 5 {
			t.Fatalf("request=%+v", req)
		}
		return searchResponse{Hits: []codestore.SymbolHit{hit}, Fresh: true, Generation: 9, Degraded: "vector_unavailable"}, nil
	}}
	injectCodeDaemonClient(t, client)

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "search", "--json", "--db", db, "Foo"})
	if code != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%q)", err, stdout)
	}
	if len(got) != 4 || got["hits"] == nil || string(got["fresh"]) != "true" || string(got["generation"]) != "9" || string(got["degraded"]) != `"vector_unavailable"` {
		t.Fatalf("JSON object=%s", stdout)
	}
	var hits []codestore.SymbolHit
	if err := json.Unmarshal(got["hits"], &hits); err != nil || !reflect.DeepEqual(hits, []codestore.SymbolHit{hit}) {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestCmdCodeSearchDaemonKeepsLegacyFiveResultCap(t *testing.T) {
	for _, k := range []string{"0", "10"} {
		t.Run(k, func(t *testing.T) {
			root := t.TempDir()
			db := filepath.Join(root, ".ragrep", "code.db")
			client := fakeCodeDaemonClient{search: func(_ context.Context, req searchRequest) (searchResponse, error) {
				if req.K != 5 {
					t.Fatalf("request K=%d, want legacy cap 5", req.K)
				}
				return searchResponse{Hits: []codestore.SymbolHit{{Key: "key-1"}}, Fresh: true}, nil
			}}
			injectCodeDaemonClient(t, client)
			code, _, stderr := captureCodeCommand(t, []string{"code", "search", "-k", k, "--db", db, "Foo"})
			if code != 0 || stderr != "" {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
		})
	}
}

func TestCmdCodeSearchDaemonModes(t *testing.T) {
	for _, mode := range []string{"auto", "text", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			db := filepath.Join(root, ".ragrep", "code.db")
			client := fakeCodeDaemonClient{search: func(_ context.Context, req searchRequest) (searchResponse, error) {
				if req.Mode != mode {
					t.Fatalf("mode=%q, want %q", req.Mode, mode)
				}
				return searchResponse{Hits: []codestore.SymbolHit{{Key: "key-1"}}, Fresh: true}, nil
			}}
			injectCodeDaemonClient(t, client)
			code, _, stderr := captureCodeCommand(t, []string{"code", "search", "--mode", mode, "--db", db, "Foo"})
			if code != 0 || stderr != "" {
				t.Fatalf("exit=%d stderr=%q", code, stderr)
			}
		})
	}
}

func TestCmdCodeSearchDaemonRejectsInvalidModeBeforeClient(t *testing.T) {
	for _, mode := range []string{"", "vector", "bogus"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			db := filepath.Join(root, ".ragrep", "code.db")
			client := fakeCodeDaemonClient{search: func(context.Context, searchRequest) (searchResponse, error) {
				return searchResponse{Hits: []codestore.SymbolHit{{Key: "unexpected"}}}, nil
			}}
			calls := injectCodeDaemonClient(t, client)
			code, stdout, stderr := captureCodeCommand(t, []string{"code", "search", "--mode=" + mode, "--db", db, "Foo"})
			if code != 1 || *calls != 0 || stdout != "" || !strings.Contains(stderr, "usage: ragrep code search") {
				t.Fatalf("mode=%q exit=%d factory calls=%d stdout=%q stderr=%q", mode, code, *calls, stdout, stderr)
			}
		})
	}
}

func TestCmdCodeSearchDaemonNoHitsAndSyncing(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	tests := []struct {
		name   string
		result searchResponse
		err    error
		code   int
		stderr string
	}{
		{name: "no hits", result: searchResponse{Fresh: true}, code: 2, stderr: "no hits\n"},
		{name: "syncing", err: &apiError{Code: "workspace_syncing", Message: "workspace syncing", Retryable: true}, code: 1, stderr: "workspace syncing; retry\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakeCodeDaemonClient{search: func(context.Context, searchRequest) (searchResponse, error) {
				return tt.result, tt.err
			}}
			injectCodeDaemonClient(t, client)
			code, stdout, stderr := captureCodeCommand(t, []string{"code", "search", "--db", db, "Foo"})
			if code != tt.code || stdout != "" || stderr != tt.stderr {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want exit=%d stderr=%q", code, stdout, stderr, tt.code, tt.stderr)
			}
		})
	}
}

func TestCmdCodeGetDaemon(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	sym := testSymbol()
	client := fakeCodeDaemonClient{get: func(_ context.Context, req getRequest) (codeindex.Symbol, error) {
		if req.Root != filepath.Clean(root) || req.DB != filepath.Clean(db) || req.Key != sym.Key || !req.Body {
			t.Fatalf("request=%+v", req)
		}
		return sym, nil
	}}
	calls := injectCodeDaemonClient(t, client)
	code, stdout, stderr := captureCodeCommand(t, []string{"code", "get", "--body", "--db", db, "--symbol", sym.Key})
	if code != 0 || stderr != "" || *calls != 1 {
		t.Fatalf("exit=%d factory calls=%d stdout=%q stderr=%q", code, *calls, stdout, stderr)
	}
	var want bytes.Buffer
	if err := formatCodeSymbol(&want, sym, true, false); err != nil {
		t.Fatal(err)
	}
	if stdout != want.String() {
		t.Fatalf("stdout=%q, want byte-for-byte %q", stdout, want.String())
	}
}

func TestCmdCodeGetDaemonNotFoundAndOperationalError(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	tests := []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{name: "not found", err: &apiError{Code: "not_found", Message: "not found"}, code: 2, stderr: "not found\n"},
		{name: "operational", err: errors.New("transport failed"), code: 1, stderr: "error: transport failed\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakeCodeDaemonClient{get: func(context.Context, getRequest) (codeindex.Symbol, error) {
				return codeindex.Symbol{}, tt.err
			}}
			injectCodeDaemonClient(t, client)
			code, stdout, stderr := captureCodeCommand(t, []string{"code", "get", "--db", db, "--symbol", "missing"})
			if code != tt.code || stdout != "" || stderr != tt.stderr {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want exit=%d stderr=%q", code, stdout, stderr, tt.code, tt.stderr)
			}
		})
	}
}

func TestCmdCodeIndexDaemonFormatsStructuredResult(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	client := fakeCodeDaemonClient{index: func(_ context.Context, req indexRequest) (indexResult, error) {
		if req.Root != filepath.Clean(root) || req.DB != filepath.Clean(db) || req.Language != "go" || !reflect.DeepEqual(req.Roots, []string{"."}) {
			t.Fatalf("request=%+v", req)
		}
		return indexResult{Indexed: []string{"a.go"}, Scanned: 2, Pruned: []string{"old.go"}}, nil
	}}
	calls := injectCodeDaemonClient(t, client)

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "index", "--db", db, "--language", "go", root})
	want := "indexed a.go\npruned old.go\ndone: 1 indexed (2 files scanned, 1 pruned)\n"
	if code != 0 || stdout != want || stderr != "" || *calls != 1 {
		t.Fatalf("exit=%d factory calls=%d stdout=%q stderr=%q", code, *calls, stdout, stderr)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("CLI must not create code.db, stat err=%v", err)
	}
}

func TestCmdCodeExpandDaemonKeepsFormattingLocal(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	targets := []codeExpandTarget{
		{Relation: "references", Resolved: true, Key: "caller", Kind: "function", QualifiedName: "Caller", Signature: "func Caller()", Path: "main.go", StartLine: 4, EndLine: 7},
		{Relation: "references", Path: "missing.go", Line: 2},
	}
	client := fakeCodeDaemonClient{expand: func(_ context.Context, req expandRequest) ([]codeExpandTarget, error) {
		if req.Root != filepath.Clean(root) || req.DB != filepath.Clean(db) || req.Key != "target" || req.Relation != "references" {
			t.Fatalf("request=%+v", req)
		}
		return targets, nil
	}}
	calls := injectCodeDaemonClient(t, client)

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "expand", "--db", db, "--symbol", "target", "--relation", "references"})
	want := "references\tcaller\tfunction\tCaller\tfunc Caller()\tmain.go:4-7\nreferences\tunresolved\tmissing.go:2\n"
	if code != 0 || stdout != want || stderr != "" || *calls != 1 {
		t.Fatalf("exit=%d factory calls=%d stdout=%q stderr=%q", code, *calls, stdout, stderr)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("CLI must not create code.db, stat err=%v", err)
	}
}

func TestCodeCommandsUseCustomDBThroughWorkspaceRegistry(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "package p\n\nfunc Foo() {\n\n}\n"
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	docServer := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-custom-db-index", fakeLSPServerDocumentSymbolSrc)
	writeRagrepConfig(t, root, `{"servers":{"go":"`+filepath.ToSlash(docServer)+`"}}`)

	registry, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := newCodeServiceForDB(registry.ResolveCode, newEmbeddingPool(func() (textEmbedder, error) {
		return new(serviceTestEmbedder), nil
	}), nil)
	server := httptest.NewServer(newDaemonServerHandler(svc, "test-token", registry, nil))
	injectCodeDaemonClient(t, daemonClient{endpoint: server.URL, token: "test-token", client: server.Client()})
	t.Cleanup(func() {
		server.Close()
		_ = svc.Close()
		_ = registry.Close()
	})

	customDB := filepath.Join(root, "custom-code.db")
	code, _, stderr := captureCodeCommand(t, []string{"code", "index", "--db", customDB, "--language", "go", root})
	if code != 0 || stderr != "" {
		t.Fatalf("custom index: exit=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(customDB); err != nil {
		t.Fatalf("custom DB was not created: %v", err)
	}
	defaultDB := filepath.Join(root, ".ragrep", "code.db")
	if _, err := os.Stat(defaultDB); !os.IsNotExist(err) {
		t.Fatalf("default DB must remain untouched, stat err=%v", err)
	}

	store, err := codestore.Open(customDB, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := store.FindByQualifiedName("Foo", "a.go")
	_ = store.Close()
	if err != nil || len(matches) != 1 {
		t.Fatalf("custom DB matches=%v err=%v", matches, err)
	}
	key := matches[0].Key

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "search", "--db", customDB, "Foo"})
	if code != 0 || !strings.Contains(stdout, key) || stderr != "" {
		t.Fatalf("custom search: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = captureCodeCommand(t, []string{"code", "get", "--db", customDB, "--symbol", key})
	if code != 0 || !strings.Contains(stdout, key) || stderr != "" {
		t.Fatalf("custom get: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	emptyServer := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-custom-db-expand", fakeLSPServerAllCapsEmptySrc)
	writeRagrepConfig(t, root, `{"servers":{"go":"`+filepath.ToSlash(emptyServer)+`"}}`)
	_ = svc.lsps.Close()
	svc.lsps = newLSPPool(time.Hour, nil)
	code, stdout, stderr = captureCodeCommand(t, []string{"code", "expand", "--db", customDB, "--symbol", key, "--relation", "references"})
	if code != 2 || stdout != "" || stderr != "no results\n" {
		t.Fatalf("custom expand: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestWorkspaceRegistryCustomDBLeaseControlsIdleEviction(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeRagrepConfig(t, root, `{}`)
	registry, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), 30*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	customDB := filepath.Join(root, "custom-code.db")
	_, canonicalDB, _, err := codeWorkspacePaths(root, customDB)
	if err != nil {
		t.Fatal(err)
	}
	key := codeWorkspaceKey{root: root, db: canonicalDB}

	release, err := registry.AcquireCode(root, customDB)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	registry.mu.Lock()
	retainedWhileLeased := registry.codeEntries[key] != nil
	persistentEntries := len(registry.entries)
	registry.mu.Unlock()
	if !retainedWhileLeased || persistentEntries != 0 {
		t.Fatalf("retained while leased=%v persistent entries=%d", retainedWhileLeased, persistentEntries)
	}
	release()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		registry.mu.Lock()
		_, exists := registry.codeEntries[key]
		registry.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("custom DB entry was not evicted after lease release")
}

func TestWorkspaceRegistrySeparatesCustomDBsAndClosesActiveLeases(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeRagrepConfig(t, root, `{}`)
	registry, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	dbA := filepath.Join(root, "a-code.db")
	dbB := filepath.Join(root, "b-code.db")
	releaseA, err := registry.AcquireCode(root, dbA)
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := registry.AcquireCode(root, dbB)
	if err != nil {
		releaseA()
		t.Fatal(err)
	}
	stateA, err := registry.ResolveCode(root, dbA)
	if err != nil {
		t.Fatal(err)
	}
	stateB, err := registry.ResolveCode(root, dbB)
	if err != nil {
		t.Fatal(err)
	}
	if stateA == stateB || len(registry.codeEntries) != 2 || len(registry.entries) != 0 {
		t.Fatalf("stateA==stateB: %v, custom entries=%d persistent entries=%d", stateA == stateB, len(registry.codeEntries), len(registry.entries))
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	releaseA()
	releaseB()
	if len(registry.codeEntries) != 0 {
		t.Fatalf("custom entries after Close=%d, want 0", len(registry.codeEntries))
	}
	if _, err := registry.ResolveCode(root, dbA); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("ResolveCode after Close error=%v, want closed error", err)
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
	root := filepath.Dir(db)
	injectCodeServiceDaemon(t, root, db)

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
	content := []byte("package main\n\nfunc main() {}\n")
	if err := os.WriteFile(filepath.Join(root, "main.go"), content, 0o644); err != nil {
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
	if _, err := s.UpsertSymbols(sym.Path, codeindex.FileHash(content), []codeindex.Symbol{sym}, 0, fakeCodeEmbed); err != nil {
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
	injectCodeServiceDaemon(t, root, db)
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
	injectCodeServiceDaemon(t, root, db)

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
	injectCodeServiceDaemon(t, root, db)

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
	content := []byte("package main\n\nfunc Callee() {}\n\nfunc Caller() {\n\tCallee()\n\tCallee()\n}\n")
	if err := os.WriteFile(mainGo, content, 0o644); err != nil {
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
	if _, err := s.UpsertSymbols("main.go", codeindex.FileHash(content), []codeindex.Symbol{target, caller}, 0, fakeCodeEmbed); err != nil {
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
	injectCodeServiceDaemon(t, root, db)

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

// fakeLSPServerDocumentSymbolSrc advertises documentSymbolProvider and
// answers every textDocument/documentSymbol request with the same one-symbol
// fixture (a "Foo" function at lines 1-3), regardless of which file was
// asked about -- Symbol.Key still differs per file since it embeds Path, so
// this is enough to exercise `code index`'s full per-file
// upsert/prune pipeline without a real language server.
const fakeLSPServerDocumentSymbolSrc = `package main

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
			result = ` + "`" + `{"capabilities":{"documentSymbolProvider":true}}` + "`" + `
		case "textDocument/documentSymbol":
			result = ` + "`" + `[{"name":"Foo","detail":"func Foo()","kind":12,"range":{"start":{"line":1,"character":0},"end":{"line":3,"character":1}},"selectionRange":{"start":{"line":1,"character":5},"end":{"line":1,"character":8}},"children":[]}]` + "`" + `
		}
		resp := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}", string(idRaw), result)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(resp), resp)
	}
}
`

// `code index` must remove symbols (and their fts/vec/edges rows -- see
// codestore.DeleteSymbolsForPath) for files that were indexed before but are
// no longer discovered under the indexed root, by default, without a flag --
// mirroring the document index's --prune but always on, since a code index
// with stale symbols silently returning wrong search/expand results is worse
// than the cost of an extra ListPaths query.
func TestCmdCodeIndexPrunesDeletedFiles(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fileSrc := "package p\n\nfunc Foo() {\n\n}\n"
	aPath := filepath.Join(root, "a.go")
	bPath := filepath.Join(root, "b.go")
	if err := os.WriteFile(aPath, []byte(fileSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte(fileSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// Built outside root: root is what `code index` walks, and a stray .go
	// source file for the fake server itself would get discovered/indexed
	// too.
	exePath := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-docsym", fakeLSPServerDocumentSymbolSrc)
	writeRagrepConfig(t, root, `{"servers": {"go": "`+filepath.ToSlash(exePath)+`"}}`)
	db := filepath.Join(root, ".ragrep", "code.db")
	injectCodeServiceDaemon(t, root, db)

	runIndex := func() (code int, out string) {
		r, w, _ := os.Pipe()
		old := os.Stdout
		os.Stdout = w
		code = run([]string{"code", "index", "--db", db, "--language", "go", root})
		w.Close()
		os.Stdout = old
		var buf bytes.Buffer
		buf.ReadFrom(r)
		return code, buf.String()
	}

	code1, out1 := runIndex()
	if code1 != 0 {
		t.Fatalf("first index: exit=%d, output=%q", code1, out1)
	}
	if !strings.Contains(out1, "indexed a.go") || !strings.Contains(out1, "indexed b.go") {
		t.Fatalf("first index output=%q, want both a.go and b.go reported indexed", out1)
	}

	assertPaths := func(want ...string) {
		t.Helper()
		s, err := codestore.Open(db, codeModelID, codeEmbedDim)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		got, err := s.ListPaths()
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ListPaths() = %v, want %v", got, want)
		}
	}
	assertPaths("a.go", "b.go")

	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}

	code2, out2 := runIndex()
	if code2 != 0 {
		t.Fatalf("second index (after deleting b.go): exit=%d, output=%q", code2, out2)
	}
	if !strings.Contains(out2, "1 pruned") {
		t.Fatalf("second index output=%q, want it to report 1 pruned", out2)
	}
	if strings.Contains(out2, "indexed a.go") {
		t.Fatalf("second index output=%q, a.go is unchanged and must not be re-reported as indexed", out2)
	}
	assertPaths("a.go")
}

// A `code expand` call records its own index_runs row (see
// serverIdentity/RecordIndexRun in cmdCodeExpand), which used to make
// LatestIndexRun -- and therefore any manifest built afterward -- advertise
// a revision at which symbols were never actually (re)indexed, since it just
// picked the highest id regardless of scope. Now `code index` scopes its own
// run "index:..." and `code expand` scopes its "expand:...", and
// LatestIndexRun only ever considers "index:"-scoped rows -- so an expand
// call right after an index must leave LatestIndexRun pointing at the index
// run, unchanged.
func TestLatestIndexRunSurvivesSubsequentExpand(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fileSrc := "package p\n\nfunc Foo() {\n\n}\n"
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(fileSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	docSymExe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-docsym-scope", fakeLSPServerDocumentSymbolSrc)
	writeRagrepConfig(t, root, `{"servers": {"go": "`+filepath.ToSlash(docSymExe)+`"}}`)
	db := filepath.Join(root, ".ragrep", "code.db")
	svc := injectCodeServiceDaemon(t, root, db)

	if code := run([]string{"code", "index", "--db", db, "--language", "go", root}); code != 0 {
		t.Fatalf("code index: exit=%d", code)
	}

	s, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	indexRun, err := s.LatestIndexRun()
	if err != nil {
		t.Fatalf("LatestIndexRun after index: %v", err)
	}
	if !strings.HasPrefix(indexRun.Scope, "index:") {
		t.Fatalf("index run scope = %q, want an \"index:\" prefix", indexRun.Scope)
	}
	matches, err := s.FindByQualifiedName("Foo", "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("FindByQualifiedName(Foo, a.go) = %v, want exactly 1 match", matches)
	}
	key := matches[0].Key
	s.Close()

	// Switch the configured server to one that answers textDocument/references
	// (with an empty result -- RecordIndexRun happens regardless of whether
	// the query found anything, so a "no results" expand still needs to not
	// disturb LatestIndexRun).
	emptyExe := buildFakeLSPServerFromSrc(t, t.TempDir(), "fakelsp-allcaps-empty-scope", fakeLSPServerAllCapsEmptySrc)
	writeRagrepConfig(t, root, `{"servers": {"go": "`+filepath.ToSlash(emptyExe)+`"}}`)
	_ = svc.lsps.Close()
	svc.lsps = newLSPPool(time.Hour, nil)
	run([]string{"code", "expand", "--db", db, "--symbol", key, "--relation", "references"}) // exit code irrelevant here

	s2, err := codestore.Open(db, codeModelID, codeEmbedDim)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after, err := s2.LatestIndexRun()
	if err != nil {
		t.Fatalf("LatestIndexRun after expand: %v", err)
	}
	if after.ID != indexRun.ID || after.Scope != indexRun.Scope {
		t.Fatalf("LatestIndexRun after expand = %+v, want unchanged from before expand (%+v)", after, indexRun)
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

func runTestCodePack(t *testing.T, s *codestore.Store, query string, k, budget int, selectedKeys []string) codePackOutput {
	t.Helper()
	qv, err := fakeCodeEmbed(query)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchSymbolsHybrid(query, qv, k)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCodePack(s, hits, budget, selectedKeys, s.GetSymbol)
	if err != nil {
		t.Fatal(err)
	}
	return out
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
	if code := run([]string{"code", "pack", "--db", db, "--query", "q", "extra-positional-arg"}); code != 1 {
		t.Fatalf("pack with a stray positional arg: exit=%d, want 1", code)
	}
}

func TestCodeServicePackReturnsFreshLiveBodyAndCapsCandidates(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	for i := range 6 {
		ws.save(t, fmt.Sprintf("handler%d.go", i), fmt.Sprintf("package service\nfunc Handler%d() { SharedOperation() }", i))
	}
	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "handler0.go"})
	if err != nil || len(search.Hits) != 1 || !search.Hits[0].Live {
		t.Fatalf("search=%+v err=%v", search, err)
	}

	out, err := svc.Pack(context.Background(), packRequest{
		Root: ws.root, Query: "SharedOperation", K: 99, Budget: 100_000,
		SelectedKeys: []string{search.Hits[0].Key},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Fresh || out.Generation == 0 || len(out.Pack.Candidates) != 5 {
		t.Fatalf("out=%+v", out)
	}
	if len(out.Pack.Symbols) != 1 || !strings.Contains(out.Pack.Symbols[0].Body, "SharedOperation") {
		t.Fatalf("symbols=%+v", out.Pack.Symbols)
	}
	if len(out.Manifest.Symbols) != 1 || out.Manifest.Symbols[0].FileHash != out.Pack.Symbols[0].BodyHash {
		t.Fatalf("manifest=%+v symbols=%+v", out.Manifest, out.Pack.Symbols)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatal(err)
	}
	if string(wrapper["fresh"]) != "true" || string(wrapper["generation"]) != "1" ||
		bytes.Contains(wrapper["manifest"], []byte(`"fresh"`)) || bytes.Contains(wrapper["manifest"], []byte(`"generation"`)) {
		t.Fatalf("freshness must be on the outer wrapper, not the manifest: %s", b)
	}
}

func TestCodeServicePackUsesAutoModeForExactSymbol(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	body := "package service\nfunc ExactPackHandler() {}"
	sym := serviceSymbol("service.go", "ExactPackHandler", "func ExactPackHandler() {}")
	putServiceSymbol(t, ws.store, sym, ws.save(t, "service.go", body))

	out, err := svc.Pack(context.Background(), packRequest{Root: ws.root, Query: sym.Name, Budget: 100_000})
	if err != nil || len(out.Pack.Candidates) != 1 || out.Pack.Candidates[0].Key != sym.Key || embedder.calls.Load() != 0 {
		t.Fatalf("out=%+v embed calls=%d err=%v", out, embedder.calls.Load(), err)
	}
}

func TestCodeServicePackUsesSingleGenerationSnapshot(t *testing.T) {
	svc, ws, embedder := newTestCodeService(t)
	oldBody := "package service\nfunc OldPackHandler() { SharedOperation() }"
	ws.save(t, "service.go", oldBody)
	generation := barrierGeneration(t, ws.workspaceState)
	live, err := ws.store.SearchLiveText("OldPackHandler", 1)
	if err != nil || len(live) != 1 {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	embedder.started = started
	embedder.release = release

	packed := make(chan struct {
		out codePackOutput
		err error
	}, 1)
	go func() {
		out, err := svc.Pack(context.Background(), packRequest{
			Root: ws.root, Query: "SharedOperation details", Budget: 100_000, SelectedKeys: []string{live[0].Key},
		})
		packed <- struct {
			out codePackOutput
			err error
		}{out, err}
	}()
	<-started
	ws.save(t, "service.go", "package service\nfunc NewPackHandler() {}")
	refreshed := make(chan error, 1)
	go func() { refreshed <- ws.refreshPaths([]string{filepath.Join(ws.root, "service.go")}) }()
	prematureRefresh := false
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatal(err)
		}
		prematureRefresh = true
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	result := <-packed
	if prematureRefresh || result.err != nil || result.out.Generation != generation || len(result.out.Pack.Symbols) != 1 || result.out.Pack.Symbols[0].Body != oldBody {
		t.Fatalf("prematureRefresh=%v out=%+v err=%v", prematureRefresh, result.out, result.err)
	}
	if !prematureRefresh {
		if err := <-refreshed; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodeServicePackRejectsReplacedLiveKey(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc FirstVersion() {}")
	first, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "FirstVersion"})
	if err != nil || len(first.Hits) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	ws.save(t, "service.go", "package service\nfunc SecondVersion() {}")
	if err := ws.refreshPaths([]string{filepath.Join(ws.root, "service.go")}); err != nil {
		t.Fatal(err)
	}

	_, err = svc.Pack(context.Background(), packRequest{
		Root: ws.root, Query: "SecondVersion", Budget: 100_000,
		SelectedKeys: []string{first.Hits[0].Key},
	})
	if !errors.Is(err, ErrStaleLiveKey) {
		t.Fatalf("Pack stale key err=%v, want stale_live_key", err)
	}
}

func TestCodeServicePackRejectsMoreThanThreeSelectedKeys(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	_, err := svc.Pack(context.Background(), packRequest{
		Root: ws.root, Query: "anything", Budget: 100_000,
		SelectedKeys: []string{"a", "b", "c", "d"},
	})
	if err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Fatalf("err=%v, want selected-key limit", err)
	}
}

func TestCodeServicePackExpandsOnlyStoredRelations(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "func Source() { Target() }\nfunc Target() {}"
	hash := ws.save(t, "service.go", body)
	source := serviceSymbol("service.go", "Source", "func Source() { Target() }")
	target := serviceSymbol("service.go", "Target", "func Target() {}")
	target.Range = codeindex.Range{Start: codeindex.Position{Line: 1}, End: codeindex.Position{Line: 2}}
	if _, err := ws.store.UpsertSymbols("service.go", hash, []codeindex.Symbol{source, target}, 0, fakeCodeEmbed); err != nil {
		t.Fatal(err)
	}
	relation := codeindex.Relation{FromKey: source.Key, ToKey: target.Key, Kind: "callees", Source: "stored"}
	if err := ws.store.ReplaceRelations(0, source.Key, []string{"callees"}, []codeindex.Relation{relation}); err != nil {
		t.Fatal(err)
	}

	out, err := svc.Pack(context.Background(), packRequest{
		Root: ws.root, Query: "Source", Budget: 100_000, SelectedKeys: []string{source.Key},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Pack.Relations, []codeindex.Relation{relation}) {
		t.Fatalf("relations=%+v, want stored relation only", out.Pack.Relations)
	}
}

func TestCmdCodePackUsesDaemonAndKeepsFormatter(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	want := codePackOutput{Fresh: true, Generation: 7, Pack: coderetrieval.ContextPack{Budget: 1000}}
	client := fakeCodeDaemonClient{pack: func(_ context.Context, req packRequest) (codePackOutput, error) {
		if req.Root != filepath.Clean(root) || req.DB != filepath.Clean(db) || req.Query != "Foo" || req.K != 10 || req.Budget != 1000 || !reflect.DeepEqual(req.SelectedKeys, []string{"key"}) {
			t.Fatalf("request=%+v", req)
		}
		return want, nil
	}}
	injectCodeDaemonClient(t, client)

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "pack", "--db", db, "--query", "Foo", "--select", "key", "--budget", "1000", "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var got codePackOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v err=%v, want=%+v", got, err, want)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("CLI must not create code.db, stat err=%v", err)
	}
}

// runCodePack is `code pack`'s post-search core. These tests supply durable
// hits directly so they never need the daemon embedding pool.
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
	if _, err := s.RecordIndexRun("index:root", "rev123", "go", "gopls", "v1.2.3", codeModelID, when); err != nil {
		t.Fatal(err)
	}

	// First, measure the metadata-only cost (large budget, no --select) so
	// the truncation budget below is derived rather than guessed.
	metaOnly := runTestCodePack(t, s, "A", 10, 100_000, nil)
	if metaOnly.Pack.Truncated {
		t.Fatalf("measurement pack unexpectedly truncated: %+v", metaOnly.Pack)
	}
	budget := metaOnly.Pack.UsedChars + 50 // room for candidates + a sliver, not both bodies

	out := runTestCodePack(t, s, "A", 10, budget, []string{"a", "b"})
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
	out := runTestCodePack(t, s, "A", 10, 100_000, []string{"a"})

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
	_, _, manifest := codeVerifyWorkspace(t)
	manifestPath := writeManifestFile(t, manifest)
	if code := run([]string{"code", "verify", "--db", db, "--manifest", manifestPath, "extra-positional-arg"}); code != 1 {
		t.Fatalf("verify with a stray positional arg: exit=%d, want 1", code)
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
	if _, err := s.RecordIndexRun("index:root", "rev1", "go", "gopls", "v1", codeModelID, time.Now()); err != nil {
		s.Close()
		t.Fatal(err)
	}

	out := runTestCodePack(t, s, "A", 10, 100_000, []string{"a"})
	s.Close()
	if len(out.Manifest.Symbols) != 1 {
		t.Fatalf("codeVerifyWorkspace: manifest has %d symbols, want 1", len(out.Manifest.Symbols))
	}
	injectCodeServiceDaemon(t, root, db)
	return root, db, out.Manifest
}

func TestCodeServiceVerifyCrossesBarrierAndDetectsSuppressedChange(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	body := "package service\nfunc Current() {}"
	hash := ws.save(t, "service.go", body)
	sym := serviceSymbol("service.go", "Current", "func Current() {}")
	putServiceSymbol(t, ws.store, sym, hash)
	manifest := coderetrieval.Manifest{Symbols: []coderetrieval.SymbolRef{{
		Key: sym.Key, QualifiedName: sym.QualifiedName, Path: sym.Path, FileHash: hash,
	}}}

	clean, err := svc.Verify(context.Background(), verifyRequest{Root: ws.root, Manifest: manifest})
	if err != nil || !clean.Clean {
		t.Fatalf("clean=%+v err=%v", clean, err)
	}
	ws.save(t, "service.go", "package service\nfunc Current() { changed() }")
	stale, err := svc.Verify(context.Background(), verifyRequest{Root: ws.root, Manifest: manifest})
	if err != nil || stale.Clean || len(stale.Entries) != 1 || !stale.Entries[0].Stale {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
}

func TestCodeServiceVerifyResolvesCurrentLivePack(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc LiveCurrent() {}")
	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "LiveCurrent"})
	if err != nil || len(search.Hits) != 1 || !search.Hits[0].Live {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	pack, err := svc.Pack(context.Background(), packRequest{
		Root: ws.root, Query: "LiveCurrent", Budget: 100_000, SelectedKeys: []string{search.Hits[0].Key},
	})
	if err != nil {
		t.Fatal(err)
	}

	verified, err := svc.Verify(context.Background(), verifyRequest{Root: ws.root, Manifest: pack.Manifest})
	if err != nil || !verified.Clean || len(verified.Entries) != 1 || !verified.Entries[0].Resolved || verified.Entries[0].Stale {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
}

func TestCodeServiceVerifyMarksReplacedLiveKeyStaleAndUnresolved(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	ws.save(t, "service.go", "package service\nfunc FirstLive() {}")
	search, err := svc.Search(context.Background(), searchRequest{Root: ws.root, Query: "FirstLive"})
	if err != nil || len(search.Hits) != 1 {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	pack, err := svc.Pack(context.Background(), packRequest{
		Root: ws.root, Query: "FirstLive", Budget: 100_000, SelectedKeys: []string{search.Hits[0].Key},
	})
	if err != nil {
		t.Fatal(err)
	}
	ws.save(t, "service.go", "package service\nfunc SecondLive() {}")
	if err := ws.refreshPaths([]string{filepath.Join(ws.root, "service.go")}); err != nil {
		t.Fatal(err)
	}

	verified, err := svc.Verify(context.Background(), verifyRequest{Root: ws.root, Manifest: pack.Manifest})
	if err != nil || verified.Clean || len(verified.Entries) != 1 || verified.Entries[0].Resolved || !verified.Entries[0].Stale || verified.Entries[0].Error == "" {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
}

func TestCodeServiceVerifyWaitsForBarrier(t *testing.T) {
	svc, ws, _ := newTestCodeService(t)
	unblock := make(chan struct{})
	ws.enumerate = func(string, string) ([]string, error) {
		<-unblock
		return nil, nil
	}
	t.Cleanup(func() { close(unblock) })

	out, err := svc.Verify(context.Background(), verifyRequest{Root: ws.root, Manifest: coderetrieval.Manifest{
		Symbols: []coderetrieval.SymbolRef{{Key: "missing", Path: "missing.go"}},
	}})
	if !errors.Is(err, ErrWorkspaceSyncing) || len(out.Entries) != 0 || out.Clean {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestRunCodeVerifyCancellationStopsFurtherReadsAndResolution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manifest := coderetrieval.Manifest{Symbols: []coderetrieval.SymbolRef{
		{Key: "a", Path: "a.go", FileHash: codeindex.FileHash([]byte("a"))},
		{Key: "b", Path: "b.go", FileHash: codeindex.FileHash([]byte("b"))},
	}}
	reads, resolutions := 0, 0
	out, err := runCodeVerify(ctx, manifest,
		func(string) ([]byte, error) {
			reads++
			cancel()
			return []byte("a"), nil
		},
		func(ref coderetrieval.SymbolRef) (coderetrieval.SymbolRef, error) {
			resolutions++
			return ref, nil
		})
	if !errors.Is(err, context.Canceled) || reads != 1 || resolutions != 0 || len(out.Entries) != 0 {
		t.Fatalf("out=%+v err=%v reads=%d resolutions=%d", out, err, reads, resolutions)
	}
}

func TestRunCodeVerifySamePathComparesEachManifestHash(t *testing.T) {
	body := []byte("current")
	manifest := coderetrieval.Manifest{Symbols: []coderetrieval.SymbolRef{
		{Key: "fresh", Path: "same.go", FileHash: codeindex.FileHash(body)},
		{Key: "stale", Path: "same.go", FileHash: "old-hash"},
	}}
	reads := 0
	out, err := runCodeVerify(context.Background(), manifest,
		func(string) ([]byte, error) {
			reads++
			return body, nil
		},
		func(ref coderetrieval.SymbolRef) (coderetrieval.SymbolRef, error) { return ref, nil })
	if err != nil || reads != 1 || len(out.Entries) != 2 || out.Entries[0].Stale || !out.Entries[1].Stale {
		t.Fatalf("out=%+v err=%v reads=%d", out, err, reads)
	}
}

func TestCmdCodeVerifyUsesDaemonAndPreservesExitCode(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	manifest := coderetrieval.Manifest{Symbols: []coderetrieval.SymbolRef{{Key: "a", Path: "a.go"}}}
	manifestPath := writeManifestFile(t, manifest)
	want := codeVerifyOutput{Entries: []codeVerifyEntry{{Key: "a", Path: "a.go", Stale: true, Resolved: true}}, Clean: false}
	client := fakeCodeDaemonClient{verify: func(_ context.Context, req verifyRequest) (codeVerifyOutput, error) {
		if req.Root != filepath.Clean(root) || req.DB != filepath.Clean(db) || !reflect.DeepEqual(req.Manifest, manifest) {
			t.Fatalf("request=%+v", req)
		}
		return want, nil
	}}
	injectCodeDaemonClient(t, client)

	code, stdout, stderr := captureCodeCommand(t, []string{"code", "verify", "--db", db, "--manifest", manifestPath, "--json"})
	if code != 2 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var got codeVerifyOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v err=%v, want=%+v", got, err, want)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("CLI must not create code.db, stat err=%v", err)
	}
}

func TestCmdCodePackLiveThenVerifyIsClean(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, ".ragrep", "code.db")
	svc := injectCodeServiceDaemon(t, root, db)
	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte("package service\nfunc LiveCLI() {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	search, err := svc.Search(context.Background(), searchRequest{Root: root, DB: db, Query: "LiveCLI"})
	if err != nil || len(search.Hits) != 1 || !search.Hits[0].Live {
		t.Fatalf("search=%+v err=%v", search, err)
	}

	packCode, packJSON, packStderr := captureCodeCommand(t, []string{
		"code", "pack", "--db", db, "--query", "LiveCLI", "--select", search.Hits[0].Key, "--json",
	})
	if packCode != 0 || packStderr != "" {
		t.Fatalf("pack exit=%d stdout=%q stderr=%q", packCode, packJSON, packStderr)
	}
	packPath := filepath.Join(t.TempDir(), "live-pack.json")
	if err := os.WriteFile(packPath, []byte(packJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	verifyCode, verifyJSON, verifyStderr := captureCodeCommand(t, []string{
		"code", "verify", "--db", db, "--manifest", packPath, "--json",
	})
	if verifyCode != 0 || verifyStderr != "" {
		t.Fatalf("verify exit=%d stdout=%q stderr=%q", verifyCode, verifyJSON, verifyStderr)
	}
	var verified codeVerifyOutput
	if err := json.Unmarshal([]byte(verifyJSON), &verified); err != nil || !verified.Clean {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
}

func TestDaemonPackRejectsMoreThanThreeSelectedKeysAsBadRequest(t *testing.T) {
	server := httptest.NewServer(newDaemonHandler(nil, "test-token"))
	defer server.Close()
	client := daemonClient{endpoint: server.URL, token: "test-token", client: server.Client()}

	_, err := client.Pack(context.Background(), packRequest{
		Root: t.TempDir(), DB: filepath.Join(t.TempDir(), "code.db"), Query: "q",
		SelectedKeys: []string{"a", "b", "c", "d"},
	})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != "bad_request" {
		t.Fatalf("err=%v, want typed bad_request", err)
	}
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

// `code verify --manifest FILE` is documented (README) to be pointed
// directly at a `code pack --json` output file, which is the wrapper shape
// {"pack":...,"manifest":...} -- not a bare Manifest. Verify must unwrap it
// (using the real symbols inside "manifest"), not silently unmarshal the
// wrapper into a bare Manifest and get zero symbols (which used to always
// report clean=true).
func TestCmdCodeVerifyAcceptsPackJSONWrapperAndDetectsStaleness(t *testing.T) {
	root, db, manifest := codeVerifyWorkspace(t)
	packPath := writePackWrapperFile(t, manifest)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	code := run([]string{"code", "verify", "--db", db, "--manifest", packPath, "--json"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 0 {
		t.Fatalf("verify against pack-wrapper JSON: exit=%d, want 0, stdout=%q", code, buf.String())
	}
	var decoded codeVerifyOutput
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, buf.String())
	}
	if !decoded.Clean || len(decoded.Entries) != 1 {
		t.Fatalf("decoded = %+v, want one clean entry", decoded)
	}

	// Edit the file the manifest's only entry points at: verify against the
	// same pack-wrapper file must now detect staleness, not stay clean.
	if err := os.WriteFile(filepath.Join(root, "pkg", "foo.go"), []byte("package pkg\n\nfunc A() { /* edited */ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r2, w2, _ := os.Pipe()
	os.Stdout = w2
	code2 := run([]string{"code", "verify", "--db", db, "--manifest", packPath, "--json"})
	w2.Close()
	os.Stdout = old
	var buf2 bytes.Buffer
	buf2.ReadFrom(r2)

	if code2 != 2 {
		t.Fatalf("verify after edit: exit=%d, want 2, stdout=%q", code2, buf2.String())
	}
	var decoded2 codeVerifyOutput
	if err := json.Unmarshal(buf2.Bytes(), &decoded2); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, buf2.String())
	}
	if decoded2.Clean {
		t.Fatal("decoded2.Clean = true, want false after file edit")
	}
}

// A manifest that loads with zero symbols -- whether that's junk JSON, an
// empty object, or any other shape that isn't a real manifest/pack -- must
// be reported as an error, never as a clean (or even non-clean-but-parsed)
// verify result: an empty manifest silently reporting clean=true is exactly
// how this bug went unnoticed.
func TestCmdCodeVerifyRejectsManifestWithZeroSymbols(t *testing.T) {
	db := filepath.Join(t.TempDir(), "code.db")
	junkPath := filepath.Join(t.TempDir(), "junk.json")
	if err := os.WriteFile(junkPath, []byte(`{"hello":"world"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	code := run([]string{"code", "verify", "--db", db, "--manifest", junkPath})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)

	if code != 1 {
		t.Fatalf("verify with zero-symbol manifest: exit=%d, want 1, stderr=%q", code, buf.String())
	}
	if !strings.Contains(buf.String(), "zero symbols") {
		t.Fatalf("stderr=%q, want a clear zero-symbols message", buf.String())
	}
}

// writePackWrapperFile writes m wrapped exactly the way `code pack --json`
// emits it (codePackOutput's own json tags: "pack","manifest") -- Pack is
// left zero-value since verify never reads it, only "manifest".
func writePackWrapperFile(t *testing.T, m coderetrieval.Manifest) string {
	t.Helper()
	out := codePackOutput{Manifest: m}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pack.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
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
