package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRunDocumentSearchTextSkipsEmbedding(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("notes/result.md", "search result", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	called := 0
	hits, err := runDocumentSearch(context.Background(), s, "text", "search", 10, nil, func(context.Context, string) ([]float32, error) {
		called++
		return nil, errors.New("text search must not embed")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "notes/result.md" {
		t.Fatalf("hits=%+v, want notes/result.md", hits)
	}
	if called != 0 {
		t.Fatalf("embed calls=%d, want 0", called)
	}
}

func TestRunDocumentSearchVectorPrefixesQuery(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("notes/result.md", "search result", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	var gotQuery string
	hits, err := runDocumentSearch(context.Background(), s, "vector", "find this", 10, nil, func(_ context.Context, query string) ([]float32, error) {
		gotQuery = query
		return fakeEmbed(query)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "notes/result.md" {
		t.Fatalf("hits=%+v, want notes/result.md", hits)
	}
	if gotQuery != "task: search result | query: find this" {
		t.Fatalf("embed query=%q", gotQuery)
	}
}

func TestRunDocumentSearchHybridPreservesTags(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpsertDoc("notes/keep.md", "---\ntags: [wanted]\n---\n\nsearch result", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("notes/drop.md", "---\ntags: [other]\n---\n\nsearch result", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	hits, err := runDocumentSearch(context.Background(), s, "hybrid", "search", 10, []string{"wanted"}, func(_ context.Context, query string) ([]float32, error) {
		return fakeEmbed(query)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc != "notes/keep.md" {
		t.Fatalf("hits=%+v, want only notes/keep.md", hits)
	}
}

func TestRunDocumentSearchRejectsUnknownAndCanceledContext(t *testing.T) {
	s := newTestStore(t)
	if _, err := runDocumentSearch(context.Background(), s, "unknown", "q", 1, nil, nil); err == nil {
		t.Fatal("unknown mode: want error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runDocumentSearch(ctx, s, "text", "q", 1, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error=%v, want context.Canceled", err)
	}
}

type testDocumentDaemonClient struct {
	hits []store.Hit
	err  error
	got  documentSearchRequest
}

func (c *testDocumentDaemonClient) SearchDocuments(_ context.Context, req documentSearchRequest) ([]store.Hit, error) {
	c.got = req
	return c.hits, c.err
}

type documentDaemonClientFunc func(context.Context, documentSearchRequest) ([]store.Hit, error)

func (f documentDaemonClientFunc) SearchDocuments(ctx context.Context, req documentSearchRequest) ([]store.Hit, error) {
	return f(ctx, req)
}

func directDocumentSearchClient() documentDaemonClient {
	return documentDaemonClientFunc(func(_ context.Context, req documentSearchRequest) ([]store.Hit, error) {
		s, err := openStoreAt(req.DB)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return runSearch(s, req.Mode, req.Query, req.K, req.Tags)
	})
}

func injectDocumentDaemonClient(t *testing.T, client documentDaemonClient, factoryErr error) *int {
	t.Helper()
	old := documentDaemonClientFactory
	calls := 0
	documentDaemonClientFactory = func() (documentDaemonClient, error) {
		calls++
		return client, factoryErr
	}
	t.Cleanup(func() { documentDaemonClientFactory = old })
	return &calls
}

func captureSearch(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		stdoutReader.Close()
		stdoutWriter.Close()
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutWriter, stderrWriter
	code := cmdSearch(args)
	stdoutWriter.Close()
	stderrWriter.Close()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	stdout, _ := io.ReadAll(stdoutReader)
	stderr, _ := io.ReadAll(stderrReader)
	stdoutReader.Close()
	stderrReader.Close()
	return code, string(stdout), string(stderr)
}

func TestCmdSearchDaemonForwardsCanonicalRequestAndPreservesTextFormat(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "notes", "result.md")
	if err := os.WriteFile(path, []byte("current content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	client := &testDocumentDaemonClient{hits: []store.Hit{{Doc: "notes/result.md", Para: 2, Lines: "3-4", Score: 0.75, Snippet: "matching text", Heading: "Result", Mtime: info.ModTime().Unix()}}}
	calls := injectDocumentDaemonClient(t, client, nil)

	code, stdout, stderr := captureSearch(t, []string{"--db", filepath.Join(".ragrep", "index.db"), "--mode", "text", "-k", "7", "--tag", "one", "--tag", "two", "matching"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if *calls != 1 {
		t.Fatalf("factory calls=%d, want 1", *calls)
	}
	wantDB, err := filepath.Abs(filepath.Join(".ragrep", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	wantRequest := documentSearchRequest{DB: wantDB, Query: "matching", Mode: "text", K: 7, Tags: []string{"one", "two"}}
	if client.got.DB != wantRequest.DB || client.got.Query != wantRequest.Query || client.got.Mode != wantRequest.Mode || client.got.K != wantRequest.K || !slicesEqual(client.got.Tags, wantRequest.Tags) {
		t.Fatalf("request=%+v, want %+v", client.got, wantRequest)
	}
	if stdout != "notes/result.md#2 (lines 3-4, score 0.7500) | Result\n  matching text\n" {
		t.Fatalf("stdout=%q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr=%q, want empty", stderr)
	}
	if _, err := os.Stat(wantDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CLI must not open the document DB: stat err=%v", err)
	}
}

func TestCmdSearchDaemonPreservesJSONNoHitsErrorsAndStaleWarning(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "stale.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	hit := store.Hit{Doc: "notes/stale.md", Para: 0, Lines: "1", Score: 1, Snippet: "old", Mtime: 0}
	client := &testDocumentDaemonClient{hits: []store.Hit{hit}}
	calls := injectDocumentDaemonClient(t, client, nil)

	code, stdout, stderr := captureSearch(t, []string{"--db", filepath.Join(".ragrep", "index.db"), "--json", "query"})
	if code != 0 || *calls != 1 {
		t.Fatalf("exit=%d calls=%d stderr=%q", code, *calls, stderr)
	}
	hit.Stale = true
	wantJSON, err := json.Marshal([]store.Hit{hit})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(wantJSON)+"\n" {
		t.Fatalf("stdout=%q, want %q", stdout, string(wantJSON)+"\n")
	}
	if !strings.Contains(stderr, "warning: 1 hit(s) reference files modified since indexing") {
		t.Fatalf("stale stderr=%q", stderr)
	}

	client.hits = nil
	code, stdout, stderr = captureSearch(t, []string{"--db", filepath.Join(".ragrep", "index.db"), "query"})
	if code != 2 || stdout != "" || stderr != "no hits\n" {
		t.Fatalf("no hits: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	client.err = &apiError{Code: "internal_error", Message: "daemon unavailable"}
	code, _, _ = captureSearch(t, []string{"--db", filepath.Join(".ragrep", "index.db"), "query"})
	if code != 1 {
		t.Fatalf("typed daemon error exit=%d, want 1", code)
	}
	client.err = errors.New("connection refused")
	code, _, _ = captureSearch(t, []string{"--db", filepath.Join(".ragrep", "index.db"), "query"})
	if code != 1 {
		t.Fatalf("operational daemon error exit=%d, want 1", code)
	}
}

func TestCmdSearchDaemonFreshJSONDoesNotLeakMtime(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "notes", "fresh.md")
	if err := os.WriteFile(path, []byte("fresh content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	hit := store.Hit{Doc: "notes/fresh.md", Para: 1, Lines: "2", Score: 0.5, Snippet: "fresh", Mtime: info.ModTime().Unix()}
	handler := newDaemonHandlerWithDocuments(nil, documentDaemonClientFunc(func(context.Context, documentSearchRequest) ([]store.Hit, error) {
		return []store.Hit{hit}, nil
	}), "secret", nil, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	calls := injectDocumentDaemonClient(t, daemonClient{endpoint: server.URL, token: "secret", client: server.Client()}, nil)

	code, stdout, stderr := captureSearch(t, []string{"--db", filepath.Join(".ragrep", "index.db"), "--json", "fresh"})
	if code != 0 || *calls != 1 || stderr != "" {
		t.Fatalf("exit=%d calls=%d stderr=%q", code, *calls, stderr)
	}
	want, err := json.Marshal([]store.Hit{hit})
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(want)+"\n" || strings.Contains(stdout, "mtime") || strings.Contains(stdout, "stale") {
		t.Fatalf("stdout=%q, want fresh public JSON %q", stdout, string(want)+"\n")
	}
}

func TestCmdSearchDaemonValidatesBeforeFactory(t *testing.T) {
	client := &testDocumentDaemonClient{}
	calls := injectDocumentDaemonClient(t, client, nil)
	for _, args := range [][]string{
		{"--mode", "invalid", "query"},
		{"-k", "0", "query"},
		{"--mode", "text", "   "},
		{"--mode", "text"},
		{"--mode", "text", "one", "two"},
	} {
		code, _, _ := captureSearch(t, args)
		if code != 1 {
			t.Fatalf("args=%v exit=%d, want 1", args, code)
		}
	}
	if *calls != 0 {
		t.Fatalf("factory calls=%d, want 0 for invalid commands", *calls)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -h/--help must print usage and exit 0, not read as a generic parse error
// (exit 1). Doesn't need the model: parsing fails before the embedder or DB
// are ever touched.
func TestHelpExitsZero(t *testing.T) {
	if code := run([]string{"search", "-h"}); code != 0 {
		t.Fatalf("search -h exit=%d, want 0", code)
	}
}

func TestRunDaemonAndWorkspaceTopLevelExitCodes(t *testing.T) {
	if code := run([]string{"daemon", "unknown"}); code != 1 {
		t.Fatalf("daemon unknown exit=%d, want 1", code)
	}
	if code := run([]string{"workspace", "unknown"}); code != 1 {
		t.Fatalf("workspace unknown exit=%d, want 1", code)
	}
}

func TestRunWorkspaceCleanNegativeExitsTwo(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	registry, err := newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"), time.Minute, testWorkspaceOpener(map[string]*workspaceState{}))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	h := newDaemonServerHandler(nil, "secret", registry, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()
	path, err := daemonDiscoveryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonDiscovery(path, daemonDiscovery{Endpoint: ts.URL, Token: "secret", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	root := testWorkspaceRoot(t)
	if code := run([]string{"workspace", "remove", root}); code != 2 {
		t.Fatalf("workspace remove missing exit=%d, want 2", code)
	}
	if code := run([]string{"workspace", "list"}); code != 2 {
		t.Fatalf("workspace list empty exit=%d, want 2", code)
	}
}

func TestRunWorkspaceAddDefaultsToCurrentDirectory(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	requested := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requested <- req.Path
		json.NewEncoder(w).Encode(map[string]string{"root": "root"})
	}))
	defer ts.Close()
	path, err := daemonDiscoveryPath()
	if err != nil || writeDaemonDiscovery(path, daemonDiscovery{Endpoint: ts.URL, Token: "secret", PID: os.Getpid()}) != nil {
		t.Fatal(err)
	}
	if code := run([]string{"workspace", "add"}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if got := <-requested; got != "." {
		t.Fatalf("path=%q, want .", got)
	}
}

func TestRunWorkspaceRemoveUsageRequiresPath(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	path, err := daemonDiscoveryPath()
	if err != nil || writeDaemonDiscovery(path, daemonDiscovery{Endpoint: ts.URL, Token: "secret", PID: os.Getpid()}) != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	code := run([]string{"workspace", "remove"})
	w.Close()
	os.Stderr = oldStderr
	var stderr bytes.Buffer
	_, _ = io.Copy(&stderr, r)
	r.Close()
	if code != 1 || !strings.Contains(stderr.String(), "workspace remove <path>") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
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

// TestHelperProcess is not a real test: it's the "converter" runConverter's
// tests exec as a subprocess (the stdlib os/exec self-exec pattern), guarded
// by GO_WANT_HELPER_PROCESS so a normal `go test` run doesn't execute its
// body as a test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Println("converted:" + filepath.Base(os.Args[len(os.Args)-1]))
	os.Exit(0)
}

// runConverter must substitute {input} into the argv, run it, and return
// stdout; env must reach the child (exec.Command inherits os.Environ() by
// default) so GO_WANT_HELPER_PROCESS set here via t.Setenv is visible to the
// self-exec'd TestHelperProcess above.
func TestRunConverter(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	argv := []string{os.Args[0], "-test.run=TestHelperProcess", "--", "{input}"}

	out, err := runConverter(argv, filepath.Join("tmp", "report.pdf"))
	if err != nil {
		t.Fatalf("runConverter: %v", err)
	}
	if out != "converted:report.pdf\n" {
		t.Fatalf("runConverter output=%q, want %q", out, "converted:report.pdf\n")
	}

	if _, err := runConverter([]string{"ragrep-no-such-converter-binary"}, "x"); err == nil {
		t.Fatal("runConverter: want error for unknown binary, got nil")
	}
}

// From a subdirectory, the default db path resolves to the nearest ancestor's
// existing .ragrep/index.db; with none, it stays cwd-relative.
func TestDefaultDBPathWalksUp(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if got := defaultDBPath(); got != filepath.Join(".ragrep", "index.db") {
		t.Fatalf("no ancestor db: got %q, want cwd-relative default", got)
	}

	rootDB := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(rootDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootDB, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := defaultDBPath(); got != rootDB {
		t.Fatalf("got %q, want %q", got, rootDB)
	}

	t.Setenv("RAGREP_DB", "env.db")
	if got := defaultDBPath(); got != "env.db" {
		t.Fatalf("env should win: got %q", got)
	}
}

// Resolution order for the --db default: RAGREP_DB > .ragrep/config.json's
// "db" field > the plain default -- CLI flag > default is already covered by
// TestDBFlagEnvDefault (an explicit --db always wins over whatever default
// dbFlag was built with).
func TestDefaultDBPathUsesConfig(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"db": "custom/idx.db"}`
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, "custom", "idx.db")
	if got := defaultDBPath(); got != want {
		t.Fatalf("config db: got %q, want %q", got, want)
	}

	t.Setenv("RAGREP_DB", "env.db")
	if got := defaultDBPath(); got != "env.db" {
		t.Fatalf("env should win over config: got %q", got)
	}
}

// A malformed .ragrep/config.json must not crash --db default resolution
// (which runs at flagset-construction time, before any command can report a
// clean error) -- it falls back to the plain default under the discovered
// root instead.
func TestDefaultDBPathBadConfigFallsBack(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.MkdirAll(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, ".ragrep", "index.db")
	if got := defaultDBPath(); got != want {
		t.Fatalf("bad config: got %q, want fallback %q", got, want)
	}
}

// strFlags is a repeatable string flag (e.g. --tag t1 --tag t2); Set appends
// rather than overwrites so multiple occurrences accumulate.
func TestStrFlagsSet(t *testing.T) {
	var tags strFlags
	if err := tags.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := tags.Set("b"); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("tags=%v, want [a b]", tags)
	}
}

// withFrontmatter prepends a tags frontmatter block unless no tags were
// given or content already starts with one.
func TestWithFrontmatter(t *testing.T) {
	got := withFrontmatter("body text", []string{"go", "cli"})
	want := "---\ntags: [go, cli]\n---\n\nbody text"
	if got != want {
		t.Fatalf("with tags: got %q, want %q", got, want)
	}

	got = withFrontmatter("body text", nil)
	if got != "body text" {
		t.Fatalf("no tags: got %q, want passthrough", got)
	}

	existing := "---\ntags: [old]\n---\nbody text"
	got = withFrontmatter(existing, []string{"go"})
	if got != existing {
		t.Fatalf("existing frontmatter: got %q, want passthrough", got)
	}
}

// ragrep add must refuse to overwrite a file that already exists, and must
// do so before reading stdin -- so this test can run to completion (exit 1)
// without a model or stdin input.
func TestCmdAddRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.md")
	if err := os.WriteFile(path, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "index.db")

	if code := run([]string{"add", "--db", db, path}); code != 1 {
		t.Fatalf("add existing file: got exit %d, want 1", code)
	}

	// Flags-first, tags before the path -- the only ordering `flag` accepts
	// (it stops parsing at the first non-flag argument). Reaching the
	// existing-file refusal (exit 1, not a usage error) proves the flagset
	// parsed --tag and --db as flags and the path as the sole positional arg.
	if code := run([]string{"add", "--tag", "x", "--db", db, path}); code != 1 {
		t.Fatalf("add --tag x --db db path: got exit %d, want 1 (refusal)", code)
	}
}

// cmdAdd must validate the target is inside the workspace root BEFORE
// writing anything to disk. Validating late (after os.MkdirAll/os.WriteFile)
// would leave a stray unindexed file outside the workspace when the command
// then fails.
func TestCmdAddRejectsOutsideRoot(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")

	outside := filepath.Join(filepath.Dir(root), "ragrep-add-outside-test.md")
	os.Remove(outside) // in case a previous failed run left it behind
	t.Cleanup(func() { os.Remove(outside) })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("stray content"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	if code := run([]string{"add", "--db", db, outside}); code != 1 {
		t.Fatalf("add outside root: exit=%d, want 1", code)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("add outside root must not create the file on disk; stat err=%v", err)
	}
}

func TestCmdAddKeepsCLIPathRelativeToCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ragrep"), 0o755); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("body"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	db := filepath.Join(root, ".ragrep", "index.db")
	if code := run([]string{"add", "--db", db, filepath.Join("notes", "from-subdir.md")}); code != 0 {
		t.Fatalf("add from workspace subdirectory: exit=%d, want 0", code)
	}
	wantPath := filepath.Join(subdir, "notes", "from-subdir.md")
	if content, err := os.ReadFile(wantPath); err != nil || string(content) != "body" {
		t.Fatalf("cwd-relative file = %q, %v; want body at %s", content, err, wantPath)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "from-subdir.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("add incorrectly wrote relative to workspace root: %v", err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.GetDoc("subdir/notes/from-subdir.md"); err != nil || got != "body" {
		t.Fatalf("indexed CLI key = %q, %v; want subdir/notes/from-subdir.md", got, err)
	}
}

// cmdAdd's flagset must parse `--tag t]... <path>` (flags before the sole
// positional), matching every other ragrep subcommand -- documenting this in
// a flagset-only test protects it independent of the CLI usage strings.
func TestCmdAddFlagOrdering(t *testing.T) {
	fs := newFlagSet("add")
	dbFlag(fs)
	var tags strFlags
	fs.Var(&tags, "tag", "tag to add to the new file's frontmatter (repeatable)")
	if err := fs.Parse([]string{"--tag", "x", "--tag", "y", "some/path.md"}); err != nil {
		t.Fatal(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "some/path.md" {
		t.Fatalf("NArg=%d Arg(0)=%q, want 1 arg %q", fs.NArg(), fs.Arg(0), "some/path.md")
	}
	if len(tags) != 2 || tags[0] != "x" || tags[1] != "y" {
		t.Fatalf("tags=%v, want [x y]", tags)
	}
}

// workspaceRoot derives the workspace root from --db: a ".ragrep/index.db"
// shaped path roots at the grandparent (the workspace dir containing
// .ragrep/), any other db path roots at its own parent directory.
func TestWorkspaceRoot(t *testing.T) {
	tmp, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ragrepDB := filepath.Join(tmp, ".ragrep", "index.db")
	root, err := workspaceRoot(ragrepDB)
	if err != nil {
		t.Fatal(err)
	}
	if root != tmp {
		t.Fatalf(".ragrep/index.db shaped db: root=%q, want grandparent %q", root, tmp)
	}

	plainDB := filepath.Join(tmp, "sub", "plain.db")
	wantSub := filepath.Join(tmp, "sub")
	root, err = workspaceRoot(plainDB)
	if err != nil {
		t.Fatal(err)
	}
	if root != wantSub {
		t.Fatalf("plain db: root=%q, want parent %q", root, wantSub)
	}
}

// All in-workspace path argument styles (relative, ./x, absolute) normalize
// to one canonical root-relative slash key; the root itself normalizes to
// ".", and anything outside the root is rejected with an explicit error.
func TestNormPath(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const want = "docs/foo.md"

	got, err := normPath(filepath.Join(root, "docs", "foo.md"), root)
	if err != nil || got != want {
		t.Fatalf("absolute-in-root: got %q err=%v, want %q", got, err, want)
	}

	t.Chdir(root)
	for _, in := range []string{"docs/foo.md", "./docs/foo.md"} {
		got, err := normPath(in, root)
		if err != nil || got != want {
			t.Fatalf("normPath(%q, root) = %q err=%v, want %q", in, got, err, want)
		}
	}

	got, err = normPath(root, root)
	if err != nil || got != "." {
		t.Fatalf("normPath(root, root) = %q err=%v, want \".\"", got, err)
	}

	outside := filepath.Dir(root) // root's own parent: definitely outside root
	_, err = normPath(outside, root)
	wantErr := fmt.Sprintf("%s is outside the workspace root %s (indexable paths must live under the workspace)", outside, root)
	if err == nil || err.Error() != wantErr {
		t.Fatalf("normPath(outside, root) err=%v, want %q", err, wantErr)
	}
}

// looksAbsKey detects the old (pre-relative) key format: a leading "/" or a
// Windows drive-letter prefix like "C:/". Root-relative keys never look like
// this, so it doubles as the guard predicate for the old-DB check.
func TestLooksAbsKey(t *testing.T) {
	cases := map[string]bool{
		"/u/x.md":   true,
		"C:/u/x.md": true,
		"docs/x.md": false,
		".":         false,
	}
	for k, want := range cases {
		if got := looksAbsKey(k); got != want {
			t.Fatalf("looksAbsKey(%q)=%v, want %v", k, got, want)
		}
	}
}

// A DB carrying old absolute-form keys must be rejected up front with an
// explicit migration message (exit 1), not silently misbehave under the new
// root-relative key scheme.
func TestOldKeyGuard(t *testing.T) {
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("/abs/old/key.md", "content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()
	injectDocumentDaemonClient(t, directDocumentSearchClient(), nil)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	code := run([]string{"search", "--db", db, "--mode", "text", "q"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)

	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(buf.String(), "old absolute-path key format") {
		t.Fatalf("stderr=%q, want guard message", buf.String())
	}
}

// cmdGet resolves a path argument three ways against the same document: the
// verbatim (root-relative) key as printed by search, a cwd-relative path,
// and an absolute path -- regardless of which subdirectory the command runs
// from.
func TestGetFallback(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("docs/auth.md", "auth content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	sub := filepath.Join(root, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if code := run([]string{"get", "--db", db, "docs/auth.md"}); code != 0 {
		t.Fatalf("verbatim root-relative key: exit=%d, want 0", code)
	}
	if code := run([]string{"get", "--db", db, "./auth.md"}); code != 0 {
		t.Fatalf("cwd-relative path: exit=%d, want 0", code)
	}
	if code := run([]string{"get", "--db", db, filepath.Join(sub, "auth.md")}); code != 0 {
		t.Fatalf("absolute path: exit=%d, want 0", code)
	}
}

// If the verbatim key misses and the fallback normPath rejects the arg as
// outside the workspace root, cmdGet must preserve the original "not found"
// result (exit 2) rather than surfacing normPath's outside-workspace error
// as a generic failure (exit 1).
func TestGetFallbackOutsideRootStaysNotFound(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	t.Chdir(root)
	if code := run([]string{"get", "--db", db, "../nonexistent-outside.md"}); code != 2 {
		t.Fatalf("get outside-root arg: exit=%d, want 2 (not found), not 1", code)
	}
}

// cmdIndex must validate every root argument is inside the workspace root UP
// FRONT, before walking any of them -- not only when the walk happens to
// reach an indexable file. Previously an outside-root arg that was empty (or
// contained only skipped files) walked to "0 indexed" and exited 0 instead of
// failing; a companion empty dir INSIDE the root proves the check is about
// root membership, not mere emptiness.
func TestCmdIndexRejectsOutsideRootEvenWhenEmpty(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, ".ragrep", "index.db")

	outside := filepath.Join(filepath.Dir(root), "ragrep-index-outside-empty-test")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })

	if code := run([]string{"index", "--db", db, outside}); code != 1 {
		t.Fatalf("index outside-root empty dir: exit=%d, want 1 (bug: was 0)", code)
	}

	inside := filepath.Join(root, "empty")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"index", "--db", db, inside}); code != 0 {
		t.Fatalf("index inside-root empty dir: exit=%d, want 0", code)
	}
}

func TestCmdIndexPreservesIndexedAndWarningOutputOrder(t *testing.T) {
	root := t.TempDir()
	ragrepDir := filepath.Join(root, ".ragrep")
	if err := os.Mkdir(ragrepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configJSON := `{"converters":{".bad":["ragrep-test-missing-converter","{input}"]}}`
	if err := os.WriteFile(filepath.Join(ragrepDir, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("indexed first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.bad"), []byte("converter fails second"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	code := run([]string{"index", "--db", filepath.Join(ragrepDir, "index.db"), "."})
	os.Stdout, os.Stderr = oldStdout, oldStderr
	w.Close()
	output, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if code != 0 {
		t.Fatalf("index exit=%d, want 0; output=%s", code, output)
	}
	indexedAt := strings.Index(string(output), "indexed a.md")
	warningAt := strings.Index(string(output), "warning: convert ")
	if indexedAt < 0 || warningAt < 0 || indexedAt > warningAt {
		t.Fatalf("output order changed; want indexed a.md before converter warning:\n%s", output)
	}
}

// --prune's existence check must be resolved against the workspace root, not
// the process's cwd: running from a sibling directory of "docs" must still
// prune the doc that's actually gone and keep the one that's actually there.
func TestPruneRootJoinStat(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := filepath.Join(root, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "keep.md"), []byte("keep content"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(root, ".ragrep", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("docs/gone.md", "gone content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertDoc("docs/keep.md", "keep content", 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	t.Chdir(other)

	if code := run([]string{"index", "--prune", "--db", db, "../docs"}); code != 0 {
		t.Fatalf("index --prune exit=%d", code)
	}

	s2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetDoc("docs/gone.md"); err != store.ErrNotFound {
		t.Fatalf("gone.md: want pruned (ErrNotFound), got %v", err)
	}
	if _, err := s2.GetDoc("docs/keep.md"); err != nil {
		t.Fatalf("keep.md: want kept, got %v", err)
	}
}

// markStale flags hits whose on-disk file is missing or whose mtime differs
// from the indexed one; hits already matching the real file are left alone.
func TestMarkStale(t *testing.T) {
	tmpdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpdir, "a.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(tmpdir, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	realMtime := info.ModTime().Unix()

	hits := []store.Hit{
		{Doc: "a.md", Mtime: realMtime},
		{Doc: "a.md", Mtime: realMtime - 10},
		{Doc: "gone.md", Mtime: 1},
	}
	n := markStale(hits, tmpdir)
	if n != 2 {
		t.Fatalf("n=%d, want 2", n)
	}
	if hits[0].Stale {
		t.Fatal("hits[0] (fresh) marked stale")
	}
	if !hits[1].Stale {
		t.Fatal("hits[1] (changed mtime) not marked stale")
	}
	if !hits[2].Stale {
		t.Fatal("hits[2] (missing file) not marked stale")
	}
}
